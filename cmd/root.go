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
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
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

type generationHistory struct {
	lock        *sync.Mutex
	lastVersion string
	count       int
}

// historiesMap stocke l'ensemble des historiques :
//
//	map[GVR]map[cle_objet]*generationHistory
var historiesMap = make(map[schema.GroupVersionResource]map[string]*generationHistory)

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:          "kubectl-storm",
	Short:        "A tool to detect too many generation changes in Kubernetes resources",
	Long:         `kubectl-storm is a tool to detect too many generation changes in Kubernetes resources.`,
	SilenceUsage: true,
	// Uncomment the following line if your bare application
	// has an action associated with it:
	RunE: func(cmd *cobra.Command, args []string) error {
		config, err := configFlags.ToRESTConfig()
		if err != nil {
			return fmt.Errorf("Can't recover config get: %v", err)
		}
		return run(cmd.Context(), config)
	},
}

var configFlags = genericclioptions.NewConfigFlags(true)

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	err := rootCmd.Execute()
	if err != nil {
		os.Exit(1)
	}
}

var runDuration time.Duration
var maxGenerationChanges int

var logger *slog.Logger

func init() {
	logger = slog.Default()

	klog.SetSlogLogger(logger)

	namedFlagSet := cliflag.NamedFlagSets{}

	run := namedFlagSet.FlagSet("run")

	// 1m in time.Duration
	defaultDuration, _ := time.ParseDuration("1m")
	run.DurationVarP(&runDuration, "run-duration", "r", defaultDuration, "Run duration")
	run.IntVarP(&maxGenerationChanges, "generation-changes", "g", 5, "Generation changes")

	// parse log level
	slog.SetLogLoggerLevel(slog.LevelInfo)

	// Cobra also supports local flags, which will only run
	// when this action is called directly.
	run.BoolP("toggle", "t", false, "Help message for toggle")

	rootCmd.Flags().AddFlagSet(run)

	config := namedFlagSet.FlagSet("config")
	configFlags.AddFlags(config)

	rootCmd.Flags().AddFlagSet(config)

	// group flogs by sections in usage
	rootCmd.SetUsageFunc(func(cmd *cobra.Command) error {
		fmt.Fprintf(cmd.OutOrStderr(), "Usage: %s\n", cmd.UseLine())
		cliflag.PrintSections(cmd.OutOrStderr(), namedFlagSet, 80)
		return nil
	})
}

func run(ctx context.Context, config *rest.Config) error {

	discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return fmt.Errorf("Can't create discovery client: %v", err)
	}

	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("Can't create dynamic client: %v", err)
	}

	factory := dynamicinformer.NewDynamicSharedInformerFactory(dynamicClient, time.Second*30)

	_, apiResourceLists, err := discoveryClient.ServerGroupsAndResources()
	if err != nil {
		return fmt.Errorf("Can't get server resources: %v", err)
	}

	var cancel context.CancelFunc

	// context with timeout
	if runDuration > 0 {
		ctx, cancel = context.WithTimeout(ctx, runDuration)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}
	defer cancel()

	// // Interception des signaux pour arrêter correctement
	// sigChan := make(chan os.Signal, 1)
	// signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	// go func() {
	// 	<-sigChan
	// 	log.Println("Signal reçu, arrêt des watchers...")
	// 	cancel()
	// }()

	ignoreList := []string{
		"Event",
		// leases
		"Lease",
	}

	var wg sync.WaitGroup

	// Démarrer les watchers
	for _, apiResourceList := range apiResourceLists {

		// Ignore list

		// Ignore resources in ignoreList

		gv, err := schema.ParseGroupVersion(apiResourceList.GroupVersion)
		if err != nil {
			logger.Warn("Can't parse group version", "GroupVersion", apiResourceList.GroupVersion)
			continue
		}

		for _, resource := range apiResourceList.APIResources {

			if slices.Contains(ignoreList, resource.Kind) {
				logger.Debug("Ignoring resource", "resource", resource.Kind)
				continue
			}

			// Ignore resources that don't support watch
			if !slices.Contains(resource.Verbs, "watch") {
				continue
			}

			// Ignore subresources
			if strings.Contains(resource.Name, "/") {
				continue
			}

			gvr := schema.GroupVersionResource{
				Group:    gv.Group,
				Version:  gv.Version,
				Resource: resource.Name,
			}

			// Initialisation de la map historique
			historiesMap[gvr] = make(map[string]*generationHistory)

			informer := factory.ForResource(gvr).Informer()

			informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
				AddFunc: func(rawObj interface{}) {
					obj, ok := rawObj.(*unstructured.Unstructured)
					if !ok {
						return
					}

					uid := string(obj.GetUID())
					version := obj.GetResourceVersion()
					if version == "" {
						return
					}

					resourceMap := historiesMap[gvr] // map[string]*generationHistory
					if _, exists := resourceMap[uid]; !exists {
						resourceMap[uid] = &generationHistory{
							lastVersion: version,
							count:       1,
							lock:        &sync.Mutex{},
						}
					}
				},
				UpdateFunc: func(oldObj, rawObj interface{}) {
					obj, ok := rawObj.(*unstructured.Unstructured)
					if !ok {
						return
					}

					uid := string(obj.GetUID())
					version := obj.GetResourceVersion()
					if version == "" {
						return
					}

					resourceMap := historiesMap[gvr] // map[string]*generationHistory

					genHist := resourceMap[uid]

					// Compare this version with the last one
					if version != genHist.lastVersion {
						genHist.lock.Lock()
						defer genHist.lock.Unlock()
						genHist.count++
						genHist.lastVersion = version
					}
				},
			})

			informer.SetWatchErrorHandler(func(r *cache.Reflector, err error) {
				logger.Error("Error in watch", "resource", gvr.Resource, "error", err)
			})

			wg.Add(1)
			go func() {
				defer wg.Done()
				informer.Run(ctx.Done())
			}()
		}
	}

	wg.Wait()

	// Print the results
	for gvr, resourceMap := range historiesMap {
		for uid, genHist := range resourceMap {
			if genHist.count > maxGenerationChanges {
				logger.Info("Too many generation changes", "resource", gvr.Resource, "uid", uid, "count list", genHist.count)
			}
		}
	}

	return nil
}
