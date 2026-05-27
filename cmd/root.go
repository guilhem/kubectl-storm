/*
Copyright © 2025 Guilhem Lettron <glettron@akaimai.com>

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	cliflag "k8s.io/component-base/cli/flag"
	"k8s.io/klog/v2"
)

const (
	defaultRunDuration     = time.Minute
	defaultChangeThreshold = 5
)

type runOptions struct {
	runDuration     time.Duration
	changeThreshold int
	signal          changeSignal
	includeFilters  []string
	excludeFilters  []string
}

var options = runOptions{
	runDuration:     defaultRunDuration,
	changeThreshold: defaultChangeThreshold,
	signal:          signalResourceVersion,
}

var logger *slog.Logger

var configFlags = genericclioptions.NewConfigFlags(true)

var rootCmd = &cobra.Command{
	Use:          "kubectl-storm",
	Short:        "Detect excessive Kubernetes resource changes",
	Long:         `kubectl-storm detects excessive Kubernetes resource changes and shows useful diffs for investigation.`,
	SilenceUsage: true,
	PreRunE: func(cmd *cobra.Command, args []string) error {
		return validateOptions(options)
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		config, err := configFlags.ToRESTConfig()
		if err != nil {
			return fmt.Errorf("can't recover config: %w", err)
		}
		return run(cmd.Context(), config, options)
	},
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func init() {
	logger = slog.Default()
	klog.SetSlogLogger(logger)
	slog.SetLogLoggerLevel(slog.LevelInfo)

	namedFlagSet := cliflag.NamedFlagSets{}

	run := namedFlagSet.FlagSet("run")
	run.DurationVarP(&options.runDuration, "run-duration", "r", defaultRunDuration, "Run duration. Use 0 to run until interrupted.")
	run.IntVar(&options.changeThreshold, "change-threshold", defaultChangeThreshold, "Number of observed values before a resource is reported.")
	run.IntVarP(&options.changeThreshold, "generation-changes", "g", defaultChangeThreshold, "Deprecated alias for --change-threshold.")
	_ = run.MarkDeprecated("generation-changes", "use --change-threshold instead")
	run.Var(&options.signal, "signal", "Signal to observe: resource-version or generation.")
	run.StringArrayVar(&options.includeFilters, "include-resource", nil, "Only watch this resource, repeatable. Format: group/version/resource, core/v1/pods, or /v1/pods.")
	run.StringArrayVar(&options.excludeFilters, "exclude-resource", nil, "Exclude this resource, repeatable. Format: group/version/resource, core/v1/pods, or /v1/pods.")

	rootCmd.Flags().AddFlagSet(run)

	config := namedFlagSet.FlagSet("config")
	configFlags.AddFlags(config)
	rootCmd.Flags().AddFlagSet(config)

	rootCmd.SetUsageFunc(func(cmd *cobra.Command) error {
		fmt.Fprintf(cmd.OutOrStderr(), "Usage: %s\n", cmd.UseLine())
		cliflag.PrintSections(cmd.OutOrStderr(), namedFlagSet, 80)
		return nil
	})
}

func run(ctx context.Context, config *rest.Config, opts runOptions) error {
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return fmt.Errorf("can't create discovery client: %w", err)
	}

	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("can't create dynamic client: %w", err)
	}

	_, apiResourceLists, err := discoveryClient.ServerGroupsAndResources()
	if err != nil {
		return fmt.Errorf("can't get server resources: %w", err)
	}

	include, exclude, err := buildResourceMatcher(opts.includeFilters, opts.excludeFilters)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	var cancel context.CancelFunc
	if opts.runDuration > 0 {
		ctx, cancel = context.WithTimeout(ctx, opts.runDuration)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}
	defer cancel()

	factory := dynamicinformer.NewDynamicSharedInformerFactory(dynamicClient, 30*time.Second)
	tracker := NewChangeTracker(opts.signal, opts.changeThreshold, 5)

	var wg sync.WaitGroup
	for _, apiResourceList := range apiResourceLists {
		gv, err := schema.ParseGroupVersion(apiResourceList.GroupVersion)
		if err != nil {
			logger.Warn("can't parse group version", "groupVersion", apiResourceList.GroupVersion)
			continue
		}

		for _, resource := range apiResourceList.APIResources {
			gvr := schema.GroupVersionResource{
				Group:    gv.Group,
				Version:  gv.Version,
				Resource: resource.Name,
			}

			if !shouldWatchResource(gvr, resource, include, exclude) {
				continue
			}

			informer := factory.ForResource(gvr).Informer()
			informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
				AddFunc: func(rawObj interface{}) {
					if obj, ok := rawObj.(*unstructured.Unstructured); ok {
						tracker.ObserveAdd(gvr, obj)
					}
				},
				UpdateFunc: func(oldObj, rawObj interface{}) {
					oldUnstructured, oldOK := oldObj.(*unstructured.Unstructured)
					newUnstructured, newOK := rawObj.(*unstructured.Unstructured)
					if !newOK {
						return
					}
					if !oldOK {
						oldUnstructured = newUnstructured
					}
					tracker.ObserveUpdate(gvr, oldUnstructured, newUnstructured)
				},
			})

			informer.SetWatchErrorHandler(func(r *cache.Reflector, err error) {
				logger.Error("error in watch", "resource", gvr.String(), "error", err)
			})

			wg.Add(1)
			go func() {
				defer wg.Done()
				informer.Run(ctx.Done())
			}()
		}
	}

	wg.Wait()
	return DisplayDiffs(tracker.Snapshot())
}

func validateOptions(opts runOptions) error {
	if opts.changeThreshold < 1 {
		return fmt.Errorf("--change-threshold must be greater than 0")
	}
	if !opts.signal.Valid() {
		return fmt.Errorf("--signal must be one of: %s", strings.Join(validSignals(), ", "))
	}
	if _, _, err := buildResourceMatcher(opts.includeFilters, opts.excludeFilters); err != nil {
		return err
	}
	return nil
}

func shouldWatchResource(gvr schema.GroupVersionResource, resource v1.APIResource, include, exclude resourceMatcher) bool {
	if strings.Contains(resource.Name, "/") {
		return false
	}
	if !hasVerb(resource.Verbs, "list") || !hasVerb(resource.Verbs, "watch") {
		return false
	}
	if exclude.Matches(gvr) {
		return false
	}
	return include.Empty() || include.Matches(gvr)
}

func hasVerb(verbs []string, want string) bool {
	for _, verb := range verbs {
		if verb == want {
			return true
		}
	}
	return false
}

func DisplayDiffs(histories HistoriesMap) error {
	filteredHistories := filterHistoriesWithDiffs(histories)
	if len(filteredHistories) == 0 {
		logger.Info("no diffs found")
		return nil
	}

	app := tview.NewApplication()
	root := tview.NewTreeNode("updated").SetColor(tcell.ColorRed).SetSelectable(false)

	for _, gvr := range sortedGVRs(filteredHistories) {
		resourceMap := filteredHistories[gvr]
		node := tview.NewTreeNode(gvr.String()).SetColor(tcell.ColorYellow).SetSelectable(false)
		root.AddChild(node)

		for _, id := range sortedIDs(resourceMap) {
			history := resourceMap[id]
			resource := tview.NewTreeNode(fmt.Sprintf("%s (%d changes)", id, history.Count)).Collapse()
			node.AddChild(resource)

			for i, diff := range history.Diffs {
				resource.AddChild(tview.NewTreeNode(fmt.Sprintf("diff %d", i+1)).
					SetReference(diff).
					SetSelectable(true))
			}
		}
	}

	tree := tview.NewTreeView().SetRoot(root).SetCurrentNode(root)
	diffView := tview.NewTextView().SetDynamicColors(true).SetScrollable(true)
	diffView.SetBorder(true).SetTitle("Diff")

	tree.SetSelectedFunc(func(node *tview.TreeNode) {
		if len(node.GetChildren()) == 0 {
			if diff, ok := node.GetReference().(string); ok {
				diffView.SetText(diff)
			}
			return
		}
		node.SetExpanded(!node.IsExpanded())
	})

	footer := tview.NewTextView().
		SetText("TAB: switch focus | ENTER: expand/select | q/Esc: quit")

	app.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyTab:
			if app.GetFocus() == tree {
				app.SetFocus(diffView)
			} else {
				app.SetFocus(tree)
			}
			return nil
		case tcell.KeyEsc:
			app.Stop()
			return nil
		}
		if event.Rune() == 'q' {
			app.Stop()
			return nil
		}
		return event
	})

	flex := tview.NewFlex().
		SetDirection(tview.FlexRow).
		AddItem(
			tview.NewFlex().
				SetDirection(tview.FlexColumn).
				AddItem(tree, 0, 1, true).
				AddItem(diffView, 0, 2, false),
			0, 1, true,
		).
		AddItem(footer, 1, 1, false)

	if err := app.SetRoot(flex, true).SetFocus(tree).Run(); err != nil {
		return fmt.Errorf("can't run tview: %w", err)
	}
	return nil
}

func filterHistoriesWithDiffs(histories HistoriesMap) HistoriesMap {
	filtered := make(HistoriesMap)
	for gvr, resourceMap := range histories {
		for id, history := range resourceMap {
			if len(history.Diffs) == 0 {
				continue
			}
			if _, exists := filtered[gvr]; !exists {
				filtered[gvr] = make(map[string]ResourceHistory)
			}
			filtered[gvr][id] = history
		}
	}
	return filtered
}

func sortedGVRs(histories HistoriesMap) []schema.GroupVersionResource {
	gvrs := make([]schema.GroupVersionResource, 0, len(histories))
	for gvr := range histories {
		gvrs = append(gvrs, gvr)
	}
	sort.Slice(gvrs, func(i, j int) bool {
		return gvrs[i].String() < gvrs[j].String()
	})
	return gvrs
}

func sortedIDs(histories map[string]ResourceHistory) []string {
	ids := make([]string, 0, len(histories))
	for id := range histories {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
