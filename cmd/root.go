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
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"strings"
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
	kubeAPIQPS      float32
	kubeAPIBurst    int
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
	run.Float32Var(&options.kubeAPIQPS, "kube-api-qps", 0, "Override Kubernetes API client QPS. Use 0 to keep the kubeconfig/client-go default.")
	run.IntVar(&options.kubeAPIBurst, "kube-api-burst", 0, "Override Kubernetes API client burst. Use 0 to keep the kubeconfig/client-go default.")

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
	include, exclude, err := buildResourceMatcher(opts.includeFilters, opts.excludeFilters)
	if err != nil {
		return err
	}

	config = configWithKubeAPIOverrides(config, opts)

	discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return fmt.Errorf("can't create discovery client: %w", err)
	}

	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("can't create dynamic client: %w", err)
	}

	apiResourceLists, err := discoverAPIResources(discoveryClient, include)
	if err != nil {
		return fmt.Errorf("can't get server resources: %w", err)
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	factory := dynamicinformer.NewDynamicSharedInformerFactory(dynamicClient, 0)
	tracker := NewChangeTracker(opts.signal, opts.changeThreshold, 5)
	watchResources := selectWatchResources(apiResourceLists, include, exclude)

	noResync := time.Duration(0)
	handlerSyncs := make([]cache.InformerSynced, 0, len(watchResources))
	for _, watchResource := range watchResources {
		gvr := watchResource.GVR
		informer := factory.ForResource(gvr).Informer()
		if err := informer.SetTransform(stripManagedFieldsTransform); err != nil {
			return fmt.Errorf("can't set transform for %s: %w", gvr.String(), err)
		}
		registration, err := informer.AddEventHandlerWithOptions(cache.ResourceEventHandlerDetailedFuncs{
			AddFunc: func(rawObj interface{}, _ bool) {
				if obj, ok := unstructuredFromObject(rawObj); ok {
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
			DeleteFunc: func(rawObj interface{}) {
				if obj, ok := unstructuredFromObject(rawObj); ok {
					tracker.ObserveDelete(gvr, obj)
				}
			},
		}, cache.HandlerOptions{ResyncPeriod: &noResync})
		if err != nil {
			return fmt.Errorf("can't add event handler for %s: %w", gvr.String(), err)
		}
		handlerSyncs = append(handlerSyncs, registration.HasSynced)

		if err := informer.SetWatchErrorHandler(func(r *cache.Reflector, err error) {
			logger.Error("error in watch", "resource", gvr.String(), "error", err)
		}); err != nil {
			return fmt.Errorf("can't set watch error handler for %s: %w", gvr.String(), err)
		}
	}

	watchCtx, cancelWatch := context.WithCancel(ctx)
	defer func() {
		cancelWatch()
		factory.Shutdown()
	}()

	factory.Start(watchCtx.Done())
	if !waitForInformerSync(watchCtx, factory, handlerSyncs) {
		return DisplayDiffs(tracker.Snapshot())
	}

	waitForRunDuration(ctx, opts.runDuration)
	return DisplayDiffs(tracker.Snapshot())
}

func validateOptions(opts runOptions) error {
	if opts.changeThreshold < 1 {
		return fmt.Errorf("--change-threshold must be greater than 0")
	}
	if !opts.signal.Valid() {
		return fmt.Errorf("--signal must be one of: %s", strings.Join(validSignals(), ", "))
	}
	if opts.kubeAPIQPS < 0 {
		return fmt.Errorf("--kube-api-qps must be greater than or equal to 0")
	}
	if opts.kubeAPIBurst < 0 {
		return fmt.Errorf("--kube-api-burst must be greater than or equal to 0")
	}
	if _, _, err := buildResourceMatcher(opts.includeFilters, opts.excludeFilters); err != nil {
		return err
	}
	return nil
}

func configWithKubeAPIOverrides(config *rest.Config, opts runOptions) *rest.Config {
	copied := rest.CopyConfig(config)
	if opts.kubeAPIQPS > 0 {
		copied.QPS = opts.kubeAPIQPS
	}
	if opts.kubeAPIBurst > 0 {
		copied.Burst = opts.kubeAPIBurst
	}
	return copied
}

type resourceDiscoveryClient interface {
	ServerResourcesForGroupVersion(groupVersion string) (*v1.APIResourceList, error)
	ServerGroupsAndResources() ([]*v1.APIGroup, []*v1.APIResourceList, error)
}

func discoverAPIResources(discoveryClient resourceDiscoveryClient, include resourceMatcher) ([]*v1.APIResourceList, error) {
	if include.Empty() {
		_, apiResourceLists, err := discoveryClient.ServerGroupsAndResources()
		if err == nil {
			return apiResourceLists, nil
		}
		if len(apiResourceLists) > 0 && discovery.IsGroupDiscoveryFailedError(err) {
			logger.Warn("partial server resource discovery", "error", err)
			return apiResourceLists, nil
		}
		return nil, err
	}

	groupVersions := sortedIncludedGroupVersions(include)
	apiResourceLists := make([]*v1.APIResourceList, 0, len(groupVersions))
	errs := make([]error, 0)
	for _, groupVersion := range groupVersions {
		apiResourceList, err := discoveryClient.ServerResourcesForGroupVersion(groupVersion)
		if err != nil {
			logger.Warn("can't get server resources for group version", "groupVersion", groupVersion, "error", err)
			errs = append(errs, fmt.Errorf("%s: %w", groupVersion, err))
			continue
		}
		if apiResourceList == nil {
			continue
		}
		apiResourceLists = append(apiResourceLists, apiResourceList)
	}

	if len(apiResourceLists) == 0 && len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return apiResourceLists, nil
}

func sortedIncludedGroupVersions(include resourceMatcher) []string {
	groupVersions := make(map[string]struct{})
	for gvr := range include {
		groupVersions[gvr.GroupVersion().String()] = struct{}{}
	}

	sorted := make([]string, 0, len(groupVersions))
	for groupVersion := range groupVersions {
		sorted = append(sorted, groupVersion)
	}
	sort.Strings(sorted)
	return sorted
}

type watchResource struct {
	GVR         schema.GroupVersionResource
	APIResource v1.APIResource
}

func selectWatchResources(apiResourceLists []*v1.APIResourceList, include, exclude resourceMatcher) []watchResource {
	watchResources := make([]watchResource, 0)
	for _, apiResourceList := range apiResourceLists {
		if apiResourceList == nil {
			continue
		}
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
			if shouldWatchResource(gvr, resource, include, exclude) {
				watchResources = append(watchResources, watchResource{
					GVR:         gvr,
					APIResource: resource,
				})
			}
		}
	}

	sort.Slice(watchResources, func(i, j int) bool {
		return watchResources[i].GVR.String() < watchResources[j].GVR.String()
	})
	return watchResources
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

func stripManagedFieldsTransform(rawObj interface{}) (interface{}, error) {
	obj, ok := rawObj.(*unstructured.Unstructured)
	if !ok {
		return rawObj, nil
	}

	copied := obj.DeepCopy()
	unstructured.RemoveNestedField(copied.Object, "metadata", "managedFields")
	return copied, nil
}

func unstructuredFromObject(rawObj interface{}) (*unstructured.Unstructured, bool) {
	switch obj := rawObj.(type) {
	case *unstructured.Unstructured:
		return obj, true
	case cache.DeletedFinalStateUnknown:
		return unstructuredFromObject(obj.Obj)
	case *cache.DeletedFinalStateUnknown:
		return unstructuredFromObject(obj.Obj)
	default:
		return nil, false
	}
}

func waitForInformerSync(ctx context.Context, factory dynamicinformer.DynamicSharedInformerFactory, handlerSyncs []cache.InformerSynced) bool {
	for gvr, synced := range factory.WaitForCacheSync(ctx.Done()) {
		if !synced {
			logger.Warn("informer cache did not sync", "resource", gvr.String())
			return false
		}
	}
	if len(handlerSyncs) == 0 {
		return true
	}
	if !cache.WaitForCacheSync(ctx.Done(), handlerSyncs...) {
		logger.Warn("informer handlers did not sync")
		return false
	}
	return true
}

func waitForRunDuration(ctx context.Context, runDuration time.Duration) {
	if runDuration <= 0 {
		<-ctx.Done()
		return
	}

	timer := time.NewTimer(runDuration)
	defer timer.Stop()

	select {
	case <-ctx.Done():
	case <-timer.C:
	}
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
			resource := tview.NewTreeNode(fmt.Sprintf("%s (%d changes)", historyLabel(id, history), history.Count)).Collapse()
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
	sort.Slice(ids, func(i, j int) bool {
		left := historyLabel(ids[i], histories[ids[i]])
		right := historyLabel(ids[j], histories[ids[j]])
		if left == right {
			return ids[i] < ids[j]
		}
		return left < right
	})
	return ids
}

func historyLabel(key string, history ResourceHistory) string {
	id := history.ID
	if id == "" {
		id = key
	}
	if history.Deleted {
		return id + " [deleted]"
	}
	return id
}
