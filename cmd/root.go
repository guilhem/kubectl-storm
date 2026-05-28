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
	"net/http"
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
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/metadata/metadatainformer"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	cliflag "k8s.io/component-base/cli/flag"
	"k8s.io/klog/v2"
)

const (
	defaultRunDuration     = time.Minute
	defaultChangeThreshold = 5
	defaultMaxDiffs        = 5
)

type watchMode string

const (
	watchModeFull     watchMode = "full"
	watchModeMetadata watchMode = "metadata"
)

type runOptions struct {
	runDuration     time.Duration
	changeThreshold int
	signal          changeSignal
	watchMode       watchMode
	includeFilters  []string
	excludeFilters  []string
	namespaceScope  string
	allNamespaces   bool
	labelSelector   string
	fieldSelector   string
	watchListLimit  int64
	kubeAPIQPS      float32
	kubeAPIBurst    int
	readOnly        bool
}

var options = runOptions{
	runDuration:     defaultRunDuration,
	changeThreshold: defaultChangeThreshold,
	signal:          signalResourceVersion,
	watchMode:       watchModeFull,
	readOnly:        true,
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
	run.Var(&options.watchMode, "watch-mode", "Watch mode: full or metadata.")
	run.StringArrayVar(&options.includeFilters, "include-resource", nil, "Only watch this resource, repeatable. Format: group/version/resource, core/v1/pods, or /v1/pods.")
	run.StringArrayVar(&options.excludeFilters, "exclude-resource", nil, "Exclude this resource, repeatable. Format: group/version/resource, core/v1/pods, or /v1/pods.")
	run.StringVar(&options.namespaceScope, "namespace-scope", "", "Only watch resources in this namespace. By default all namespaces are watched.")
	run.BoolVar(&options.allNamespaces, "all-namespaces", false, "Explicitly watch all namespaces. This is already the default unless --namespace-scope is set.")
	run.StringVar(&options.labelSelector, "label-selector", "", "Label selector applied to watched list/watch requests.")
	run.StringVar(&options.fieldSelector, "field-selector", "", "Field selector applied to watched list/watch requests.")
	run.Int64Var(&options.watchListLimit, "watch-list-page-size", 0, "Requested page size for watched list requests. Use 0 to let client-go decide.")
	run.Float32Var(&options.kubeAPIQPS, "kube-api-qps", 0, "Override Kubernetes API client QPS. Use 0 to keep the kubeconfig/client-go default.")
	run.IntVar(&options.kubeAPIBurst, "kube-api-burst", 0, "Override Kubernetes API client burst. Use 0 to keep the kubeconfig/client-go default.")
	run.BoolVar(&options.readOnly, "read-only", true, "Block non-read Kubernetes API requests before they reach the API server.")

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
	runCtx, cancelRun := contextWithRunDuration(ctx, opts.runDuration)
	defer cancelRun()

	factory, err := newResourceInformerFactory(config, dynamicClient, opts)
	if err != nil {
		return err
	}

	tracker := NewChangeTracker(opts.signal, opts.changeThreshold, defaultMaxDiffs)
	diffRecorder := newMetadataDiffRecorder(dynamicFullObjectGetter{client: dynamicClient}, tracker, defaultMaxDiffs)
	if normalizedWatchMode(opts.watchMode) != watchModeMetadata {
		diffRecorder = nil
	}
	watchResources := selectWatchResources(apiResourceLists, include, exclude, opts)

	watchCtx, cancelWatch := context.WithCancel(runCtx)
	var stopWatchesOnce sync.Once
	stopWatches := func() {
		stopWatchesOnce.Do(func() {
			cancelWatch()
			factory.Shutdown()
		})
	}
	defer stopWatches()

	handlerSyncs := make([]cache.InformerSynced, 0, len(watchResources))
	for _, watchResource := range watchResources {
		handlerSync, err := registerWatchResourceHandler(watchCtx, factory, watchResource.GVR, tracker, diffRecorder)
		if err != nil {
			return err
		}
		handlerSyncs = append(handlerSyncs, handlerSync)
	}

	factory.Start(watchCtx.Done())
	if !waitForInformerSync(watchCtx, factory, handlerSyncs) {
		stopWatches()
		return DisplayDiffs(tracker.Snapshot())
	}

	waitForRunDuration(runCtx)
	stopWatches()
	return DisplayDiffs(tracker.Snapshot())
}

func (m watchMode) String() string {
	if m == "" {
		return string(watchModeFull)
	}
	return string(m)
}

func (m *watchMode) Set(value string) error {
	mode := watchMode(value)
	if !mode.Valid() {
		return fmt.Errorf("watch mode must be one of: %s", strings.Join(validWatchModes(), ", "))
	}
	*m = mode
	return nil
}

func (m watchMode) Type() string {
	return "watch-mode"
}

func (m watchMode) Valid() bool {
	switch m {
	case "", watchModeFull, watchModeMetadata:
		return true
	default:
		return false
	}
}

func normalizedWatchMode(mode watchMode) watchMode {
	if mode == "" {
		return watchModeFull
	}
	return mode
}

func validWatchModes() []string {
	return []string{string(watchModeFull), string(watchModeMetadata)}
}

func validateOptions(opts runOptions) error {
	if opts.changeThreshold < 1 {
		return fmt.Errorf("--change-threshold must be greater than 0")
	}
	if !opts.signal.Valid() {
		return fmt.Errorf("--signal must be one of: %s", strings.Join(validSignals(), ", "))
	}
	if !opts.watchMode.Valid() {
		return fmt.Errorf("--watch-mode must be one of: %s", strings.Join(validWatchModes(), ", "))
	}
	if opts.namespaceScope != "" && opts.allNamespaces {
		return fmt.Errorf("--namespace-scope and --all-namespaces can't be used together")
	}
	if opts.labelSelector != "" {
		if _, err := labels.Parse(opts.labelSelector); err != nil {
			return fmt.Errorf("--label-selector is invalid: %w", err)
		}
	}
	if opts.fieldSelector != "" {
		if _, err := fields.ParseSelector(opts.fieldSelector); err != nil {
			return fmt.Errorf("--field-selector is invalid: %w", err)
		}
	}
	if opts.watchListLimit < 0 {
		return fmt.Errorf("--watch-list-page-size must be greater than or equal to 0")
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
	if opts.readOnly {
		copied.Wrap(readOnlyTransportWrapper)
	}
	return copied
}

func readOnlyTransportWrapper(rt http.RoundTripper) http.RoundTripper {
	return readOnlyRoundTripper{next: rt}
}

type readOnlyRoundTripper struct {
	next http.RoundTripper
}

func (r readOnlyRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	switch req.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return r.next.RoundTrip(req)
	default:
		return nil, fmt.Errorf("read-only client blocked %s %s", req.Method, req.URL.String())
	}
}

func namespaceForWatch(opts runOptions) string {
	if opts.namespaceScope != "" {
		return opts.namespaceScope
	}
	return v1.NamespaceAll
}

func contextWithRunDuration(ctx context.Context, runDuration time.Duration) (context.Context, context.CancelFunc) {
	if runDuration > 0 {
		return context.WithTimeout(ctx, runDuration)
	}
	return context.WithCancel(ctx)
}

func listOptionsTweak(opts runOptions) func(*v1.ListOptions) {
	return func(listOptions *v1.ListOptions) {
		if opts.labelSelector != "" {
			listOptions.LabelSelector = opts.labelSelector
		}
		if opts.fieldSelector != "" {
			listOptions.FieldSelector = opts.fieldSelector
		}
		if opts.watchListLimit > 0 && !listOptions.Watch {
			listOptions.Limit = opts.watchListLimit
		}
	}
}

type resourceInformerFactory interface {
	ForResource(gvr schema.GroupVersionResource) informers.GenericInformer
	Start(stopCh <-chan struct{})
	WaitForCacheSync(stopCh <-chan struct{}) map[schema.GroupVersionResource]bool
	Shutdown()
}

type scopeAwareResourceInformerFactory struct {
	namespacedFactory resourceInformerFactory
	clusterFactory    resourceInformerFactory
	discoveryClient   resourceDiscoveryClient

	mu              sync.RWMutex
	namespacedByGVR map[schema.GroupVersionResource]bool
}

func (f *scopeAwareResourceInformerFactory) ForResource(gvr schema.GroupVersionResource) informers.GenericInformer {
	if f.isNamespacedResource(gvr) {
		return f.namespacedFactory.ForResource(gvr)
	}
	return f.clusterFactory.ForResource(gvr)
}

func (f *scopeAwareResourceInformerFactory) Start(stopCh <-chan struct{}) {
	f.namespacedFactory.Start(stopCh)
	f.clusterFactory.Start(stopCh)
}

func (f *scopeAwareResourceInformerFactory) WaitForCacheSync(stopCh <-chan struct{}) map[schema.GroupVersionResource]bool {
	result := f.clusterFactory.WaitForCacheSync(stopCh)
	for gvr, synced := range f.namespacedFactory.WaitForCacheSync(stopCh) {
		result[gvr] = synced
	}
	return result
}

func (f *scopeAwareResourceInformerFactory) Shutdown() {
	f.namespacedFactory.Shutdown()
	f.clusterFactory.Shutdown()
}

func (f *scopeAwareResourceInformerFactory) isNamespacedResource(gvr schema.GroupVersionResource) bool {
	f.mu.RLock()
	namespaced, ok := f.namespacedByGVR[gvr]
	f.mu.RUnlock()
	if ok {
		return namespaced
	}

	resourceList, err := f.discoveryClient.ServerResourcesForGroupVersion(gvr.GroupVersion().String())
	if err != nil {
		klog.Warningf("failed to discover scope for %s: %v; treating as namespaced to avoid widening to cluster scope", gvr.String(), err)
		return true
	}

	for _, resource := range resourceList.APIResources {
		if resource.Name == gvr.Resource {
			namespaced = resource.Namespaced

			f.mu.Lock()
			f.namespacedByGVR[gvr] = namespaced
			f.mu.Unlock()

			return namespaced
		}
	}

	klog.Warningf("failed to determine scope for %s from discovery results; treating as namespaced to avoid widening to cluster scope", gvr.String())
	return true
}

func newResourceInformerFactory(config *rest.Config, dynamicClient dynamic.Interface, opts runOptions) (resourceInformerFactory, error) {
	namespace := namespaceForWatch(opts)
	tweakListOptions := listOptionsTweak(opts)

	buildFactory := func(factoryNamespace string) (resourceInformerFactory, error) {
		switch normalizedWatchMode(opts.watchMode) {
		case watchModeFull:
			return dynamicinformer.NewFilteredDynamicSharedInformerFactory(dynamicClient, 0, factoryNamespace, tweakListOptions), nil
		case watchModeMetadata:
			metadataClient, err := metadata.NewForConfig(config)
			if err != nil {
				return nil, fmt.Errorf("can't create metadata client: %w", err)
			}
			return metadatainformer.NewFilteredSharedInformerFactory(metadataClient, 0, factoryNamespace, tweakListOptions), nil
		default:
			return nil, fmt.Errorf("unsupported watch mode %q", opts.watchMode)
		}
	}

	if namespace == v1.NamespaceAll {
		return buildFactory(namespace)
	}

	namespacedFactory, err := buildFactory(namespace)
	if err != nil {
		return nil, err
	}

	clusterFactory, err := buildFactory(v1.NamespaceAll)
	if err != nil {
		return nil, err
	}

	discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("can't create discovery client: %w", err)
	}

	return &scopeAwareResourceInformerFactory{
		namespacedFactory: namespacedFactory,
		clusterFactory:    clusterFactory,
		discoveryClient:   discoveryClient,
		namespacedByGVR:   make(map[schema.GroupVersionResource]bool),
	}, nil
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
	groupVersions := sets.New[string]()
	for gvr := range include {
		groupVersions.Insert(gvr.GroupVersion().String())
	}
	return sets.List(groupVersions)
}

type watchResource struct {
	GVR         schema.GroupVersionResource
	APIResource v1.APIResource
}

func selectWatchResources(apiResourceLists []*v1.APIResourceList, include, exclude resourceMatcher, opts runOptions) []watchResource {
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
			if opts.namespaceScope != "" && !resource.Namespaced {
				continue
			}
			gvr := gv.WithResource(resource.Name)
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
	if !sets.New(resource.Verbs...).HasAll("list", "watch") {
		return false
	}
	if exclude.Matches(gvr) {
		return false
	}
	return include.Empty() || include.Matches(gvr)
}

func stripManagedFieldsTransform(rawObj interface{}) (interface{}, error) {
	obj, ok := rawObj.(runtime.Object)
	if !ok {
		return rawObj, nil
	}

	copied := obj.DeepCopyObject()
	meta, err := apimeta.Accessor(copied)
	if err != nil {
		return rawObj, nil
	}
	meta.SetManagedFields(nil)
	return copied, nil
}

type diffRecorder interface {
	Capture(ctx context.Context, gvr schema.GroupVersionResource, obj ObservedObject, observation Observation)
}

func registerWatchResourceHandler(ctx context.Context, factory resourceInformerFactory, gvr schema.GroupVersionResource, tracker *ChangeTracker, recorder diffRecorder) (cache.InformerSynced, error) {
	informer := factory.ForResource(gvr).Informer()
	if err := informer.SetTransform(stripManagedFieldsTransform); err != nil {
		return nil, fmt.Errorf("can't set transform for %s: %w", gvr.String(), err)
	}

	noResync := time.Duration(0)
	registration, err := informer.AddEventHandlerWithOptions(cache.ResourceEventHandlerDetailedFuncs{
		AddFunc: func(rawObj interface{}, _ bool) {
			if obj, ok := observedFromObject(rawObj); ok {
				tracker.ObserveAdd(gvr, obj)
			}
		},
		UpdateFunc: func(oldObj, rawObj interface{}) {
			oldObserved, oldOK := observedFromObject(oldObj)
			newObserved, newOK := observedFromObject(rawObj)
			if !newOK {
				return
			}
			if !oldOK {
				oldObserved = newObserved
			}
			observation := tracker.ObserveUpdate(gvr, oldObserved, newObserved)
			if recorder != nil {
				recorder.Capture(ctx, gvr, newObserved, observation)
			}
		},
		DeleteFunc: func(rawObj interface{}) {
			if obj, ok := observedFromObject(rawObj); ok {
				tracker.ObserveDelete(gvr, obj)
			}
		},
	}, cache.HandlerOptions{ResyncPeriod: &noResync})
	if err != nil {
		return nil, fmt.Errorf("can't add event handler for %s: %w", gvr.String(), err)
	}

	if err := informer.SetWatchErrorHandler(func(r *cache.Reflector, err error) {
		logger.Error("error in watch", "resource", gvr.String(), "error", err)
	}); err != nil {
		return nil, fmt.Errorf("can't set watch error handler for %s: %w", gvr.String(), err)
	}

	return registration.HasSynced, nil
}

func observedFromObject(rawObj interface{}) (ObservedObject, bool) {
	switch obj := rawObj.(type) {
	case cache.DeletedFinalStateUnknown:
		return observedFromObject(obj.Obj)
	case *cache.DeletedFinalStateUnknown:
		return observedFromObject(obj.Obj)
	default:
		meta, err := apimeta.Accessor(rawObj)
		return meta, err == nil
	}
}

type fullObjectGetter interface {
	Get(ctx context.Context, gvr schema.GroupVersionResource, namespace, name string) (*unstructured.Unstructured, error)
}

type dynamicFullObjectGetter struct {
	client dynamic.Interface
}

func (g dynamicFullObjectGetter) Get(ctx context.Context, gvr schema.GroupVersionResource, namespace, name string) (*unstructured.Unstructured, error) {
	return g.client.Resource(gvr).Namespace(namespace).Get(ctx, name, v1.GetOptions{})
}

type metadataDiffRecorder struct {
	getter       fullObjectGetter
	tracker      *ChangeTracker
	maxSnapshots int
	lock         sync.Mutex
	snapshots    map[schema.GroupVersionResource]map[string][]any
}

func newMetadataDiffRecorder(getter fullObjectGetter, tracker *ChangeTracker, maxDiffs int) *metadataDiffRecorder {
	return &metadataDiffRecorder{
		getter:       getter,
		tracker:      tracker,
		maxSnapshots: maxDiffs + 1,
		snapshots:    make(map[schema.GroupVersionResource]map[string][]any),
	}
}

func (r *metadataDiffRecorder) Capture(ctx context.Context, gvr schema.GroupVersionResource, obj ObservedObject, observation Observation) {
	if r == nil || !observation.Changed || !observation.Hot || observation.Key == "" {
		return
	}

	if r.snapshotCount(gvr, observation.Key) >= r.maxSnapshots {
		return
	}

	fullObj, err := r.getter.Get(ctx, gvr, obj.GetNamespace(), obj.GetName())
	if err != nil {
		logger.Warn("can't fetch full object for metadata diff", "resource", gvr.String(), "id", observation.ID, "error", err)
		return
	}

	snapshot := sanitizeForDiff(r.tracker.signal, fullObj)
	diff, ok := r.appendSnapshot(gvr, observation.Key, snapshot)
	if !ok {
		return
	}
	r.tracker.AddDiff(gvr, observation.Key, diff)
}

func (r *metadataDiffRecorder) snapshotCount(gvr schema.GroupVersionResource, key string) int {
	r.lock.Lock()
	defer r.lock.Unlock()

	return len(r.snapshots[gvr][key])
}

func (r *metadataDiffRecorder) appendSnapshot(gvr schema.GroupVersionResource, key string, snapshot any) (string, bool) {
	r.lock.Lock()
	defer r.lock.Unlock()

	resourceSnapshots := r.snapshots[gvr]
	if resourceSnapshots == nil {
		resourceSnapshots = make(map[string][]any)
		r.snapshots[gvr] = resourceSnapshots
	}

	snapshots := resourceSnapshots[key]
	if len(snapshots) >= r.maxSnapshots {
		return "", false
	}

	if len(snapshots) == 0 {
		resourceSnapshots[key] = append(snapshots, snapshot)
		return "metadata mode: captured first full snapshot; diff will be available after the next hot change", true
	}

	previous := snapshots[len(snapshots)-1]
	resourceSnapshots[key] = append(snapshots, snapshot)
	return diffSanitized(previous, snapshot), true
}

func waitForInformerSync(ctx context.Context, factory resourceInformerFactory, handlerSyncs []cache.InformerSynced) bool {
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

func waitForRunDuration(ctx context.Context) {
	<-ctx.Done()
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
