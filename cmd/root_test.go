package cmd

import (
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

func TestConfigWithKubeAPIOverridesCopiesConfig(t *testing.T) {
	config := &rest.Config{QPS: 5, Burst: 10}

	got := configWithKubeAPIOverrides(config, runOptions{
		kubeAPIQPS:   50,
		kubeAPIBurst: 100,
	})

	if got == config {
		t.Fatal("config should be copied")
	}
	if got.QPS != 50 {
		t.Fatalf("QPS = %v, want 50", got.QPS)
	}
	if got.Burst != 100 {
		t.Fatalf("Burst = %d, want 100", got.Burst)
	}
	if config.QPS != 5 || config.Burst != 10 {
		t.Fatalf("original config mutated: %#v", config)
	}
}

func TestConfigWithKubeAPIOverridesAddsReadOnlyTransport(t *testing.T) {
	config := &rest.Config{}

	got := configWithKubeAPIOverrides(config, runOptions{readOnly: true})

	if got.WrapTransport == nil {
		t.Fatal("WrapTransport should be set")
	}
	if config.WrapTransport != nil {
		t.Fatal("original config should not be mutated")
	}
}

func TestReadOnlyRoundTripper(t *testing.T) {
	next := &recordingRoundTripper{}
	transport := readOnlyTransportWrapper(next)

	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		req, err := http.NewRequest(method, "https://example.test/apis", nil)
		if err != nil {
			t.Fatalf("NewRequest(%s) error = %v", method, err)
		}
		if _, err := transport.RoundTrip(req); err != nil {
			t.Fatalf("RoundTrip(%s) error = %v", method, err)
		}
	}

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		req, err := http.NewRequest(method, "https://example.test/apis", nil)
		if err != nil {
			t.Fatalf("NewRequest(%s) error = %v", method, err)
		}
		if _, err := transport.RoundTrip(req); err == nil {
			t.Fatalf("RoundTrip(%s) should be blocked", method)
		}
	}

	if next.count != 3 {
		t.Fatalf("allowed request count = %d, want 3", next.count)
	}
}

func TestNamespaceForWatch(t *testing.T) {
	if got := namespaceForWatch(runOptions{}); got != v1.NamespaceAll {
		t.Fatalf("namespaceForWatch(default) = %q, want all namespaces", got)
	}
	if got := namespaceForWatch(runOptions{allNamespaces: true}); got != v1.NamespaceAll {
		t.Fatalf("namespaceForWatch(all) = %q, want all namespaces", got)
	}
	if got := namespaceForWatch(runOptions{namespaceScope: "prod"}); got != "prod" {
		t.Fatalf("namespaceForWatch(prod) = %q, want prod", got)
	}
}

func TestListOptionsTweak(t *testing.T) {
	opts := runOptions{
		labelSelector:  "app=api",
		fieldSelector:  "metadata.name=api",
		watchListLimit: 250,
	}
	listOptions := v1.ListOptions{}

	listOptionsTweak(opts)(&listOptions)

	if listOptions.LabelSelector != "app=api" {
		t.Fatalf("LabelSelector = %q, want app=api", listOptions.LabelSelector)
	}
	if listOptions.FieldSelector != "metadata.name=api" {
		t.Fatalf("FieldSelector = %q, want metadata.name=api", listOptions.FieldSelector)
	}
	if listOptions.Limit != 250 {
		t.Fatalf("Limit = %d, want 250", listOptions.Limit)
	}
}

func TestDiscoverAPIResourcesAllowsPartialDiscovery(t *testing.T) {
	want := []*v1.APIResourceList{{GroupVersion: "apps/v1"}}
	discoveryClient := &fakeResourceDiscovery{
		allLists: want,
		allErr: &discovery.ErrGroupDiscoveryFailed{Groups: map[schema.GroupVersion]error{
			{Group: "broken", Version: "v1"}: errors.New("unavailable"),
		}},
	}

	got, err := discoverAPIResources(discoveryClient, resourceMatcher{})
	if err != nil {
		t.Fatalf("discoverAPIResources() error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("discoverAPIResources() = %#v, want %#v", got, want)
	}
}

func TestDiscoverAPIResourcesUsesIncludedGroupVersions(t *testing.T) {
	discoveryClient := &fakeResourceDiscovery{
		resourcesByGroupVersion: map[string]*v1.APIResourceList{
			"apps/v1": {GroupVersion: "apps/v1"},
			"v1":      {GroupVersion: "v1"},
		},
	}
	include := resourceMatcher{
		{Group: "apps", Version: "v1", Resource: "deployments"}: {},
		{Group: "", Version: "v1", Resource: "pods"}:            {},
	}

	got, err := discoverAPIResources(discoveryClient, include)
	if err != nil {
		t.Fatalf("discoverAPIResources() error = %v", err)
	}
	if discoveryClient.allCalled {
		t.Fatal("full discovery should not be used when includes are provided")
	}
	if want := []string{"apps/v1", "v1"}; !reflect.DeepEqual(discoveryClient.requestedGroupVersions, want) {
		t.Fatalf("requested group versions = %#v, want %#v", discoveryClient.requestedGroupVersions, want)
	}
	if len(got) != 2 {
		t.Fatalf("resource list count = %d, want 2", len(got))
	}
}

func TestSelectWatchResourcesFiltersAndSorts(t *testing.T) {
	include, exclude, err := buildResourceMatcher([]string{"apps/v1/deployments", "core/v1/pods"}, []string{"core/v1/pods"})
	if err != nil {
		t.Fatalf("buildResourceMatcher() error = %v", err)
	}
	apiResourceLists := []*v1.APIResourceList{
		{
			GroupVersion: "v1",
			APIResources: []v1.APIResource{
				{Name: "pods", Verbs: []string{"list", "watch"}},
			},
		},
		{
			GroupVersion: "apps/v1",
			APIResources: []v1.APIResource{
				{Name: "replicasets", Verbs: []string{"list"}},
				{Name: "deployments/status", Verbs: []string{"list", "watch"}},
				{Name: "deployments", Verbs: []string{"list", "watch"}},
			},
		},
	}

	got := selectWatchResources(apiResourceLists, include, exclude)
	if len(got) != 1 {
		t.Fatalf("watch resource count = %d, want 1", len(got))
	}
	if got[0].GVR != deploymentGVR {
		t.Fatalf("watch resource = %#v, want %#v", got[0].GVR, deploymentGVR)
	}
}

func TestStripManagedFieldsTransform(t *testing.T) {
	obj := testObject("default", "api", "1", 2, "nginx:1")

	rawObj, err := stripManagedFieldsTransform(obj)
	if err != nil {
		t.Fatalf("stripManagedFieldsTransform() error = %v", err)
	}
	transformed, ok := rawObj.(*unstructured.Unstructured)
	if !ok {
		t.Fatalf("stripManagedFieldsTransform() returned %T", rawObj)
	}

	if _, exists, _ := unstructured.NestedFieldNoCopy(transformed.Object, "metadata", "managedFields"); exists {
		t.Fatal("managedFields should be removed")
	}
	if transformed.GetResourceVersion() != "1" {
		t.Fatalf("resourceVersion = %q, want 1", transformed.GetResourceVersion())
	}
	if transformed.GetGeneration() != 2 {
		t.Fatalf("generation = %d, want 2", transformed.GetGeneration())
	}
	if string(transformed.GetUID()) != "uid-api" {
		t.Fatalf("uid = %q, want uid-api", transformed.GetUID())
	}
	if _, exists, _ := unstructured.NestedFieldNoCopy(obj.Object, "metadata", "managedFields"); !exists {
		t.Fatal("original object should not be mutated")
	}
}

func TestStripManagedFieldsTransformPartialMetadata(t *testing.T) {
	obj := testMetadataObject("default", "api", "uid-api", "1", 2)
	obj.ManagedFields = []v1.ManagedFieldsEntry{{Manager: "kubectl"}}

	rawObj, err := stripManagedFieldsTransform(obj)
	if err != nil {
		t.Fatalf("stripManagedFieldsTransform() error = %v", err)
	}
	transformed, ok := rawObj.(*v1.PartialObjectMetadata)
	if !ok {
		t.Fatalf("stripManagedFieldsTransform() returned %T", rawObj)
	}

	if len(transformed.ManagedFields) != 0 {
		t.Fatal("managedFields should be removed")
	}
	if len(obj.ManagedFields) == 0 {
		t.Fatal("original object should not be mutated")
	}
}

func TestUnstructuredFromObjectSupportsDeletedFinalStateUnknown(t *testing.T) {
	want := testObject("default", "api", "1", 1, "nginx:1")

	got, ok := observedFromObject(cache.DeletedFinalStateUnknown{Obj: want})
	if !ok {
		t.Fatal("DeletedFinalStateUnknown should be accepted")
	}
	if got != want {
		t.Fatal("unexpected object recovered from tombstone")
	}
}

func TestObservedFromObjectSupportsPartialMetadataTombstone(t *testing.T) {
	want := testMetadataObject("default", "api", "uid-api", "1", 1)

	got, ok := observedFromObject(cache.DeletedFinalStateUnknown{Obj: want})
	if !ok {
		t.Fatal("metadata tombstone should be accepted")
	}
	if got != want {
		t.Fatal("unexpected object recovered from metadata tombstone")
	}
}

func TestMetadataDiffRecorderFetchesOnlyHotObjects(t *testing.T) {
	tracker := NewChangeTracker(signalResourceVersion, 2, 2)
	getter := &fakeFullObjectGetter{
		objects: []*unstructured.Unstructured{
			testObject("default", "api", "3", 1, "nginx:2"),
			testObject("default", "api", "4", 1, "nginx:3"),
		},
	}
	recorder := newMetadataDiffRecorder(getter, tracker, 2)

	first := testMetadataObject("default", "api", "uid-api", "1", 1)
	second := testMetadataObject("default", "api", "uid-api", "2", 1)
	third := testMetadataObject("default", "api", "uid-api", "3", 1)
	fourth := testMetadataObject("default", "api", "uid-api", "4", 1)

	tracker.ObserveAdd(deploymentGVR, first)
	observation := tracker.ObserveUpdate(deploymentGVR, first, second)
	recorder.Capture(context.Background(), deploymentGVR, second, observation)
	if getter.count != 0 {
		t.Fatalf("full GET count before threshold = %d, want 0", getter.count)
	}

	observation = tracker.ObserveUpdate(deploymentGVR, second, third)
	recorder.Capture(context.Background(), deploymentGVR, third, observation)
	if getter.count != 1 {
		t.Fatalf("full GET count after threshold = %d, want 1", getter.count)
	}

	observation = tracker.ObserveUpdate(deploymentGVR, third, fourth)
	recorder.Capture(context.Background(), deploymentGVR, fourth, observation)
	if getter.count != 2 {
		t.Fatalf("full GET count after second hot update = %d, want 2", getter.count)
	}

	history := historyByID(t, tracker.Snapshot(), deploymentGVR, "default/api")
	if len(history.Diffs) != 2 {
		t.Fatalf("diff count = %d, want 2", len(history.Diffs))
	}
	if !strings.Contains(history.Diffs[0], "metadata mode") {
		t.Fatalf("first diff should explain lazy capture: %s", history.Diffs[0])
	}
	if !strings.Contains(history.Diffs[1], "nginx:3") {
		t.Fatalf("second diff should include full object change: %s", history.Diffs[1])
	}

	if snapshots := recorder.snapshotCount(deploymentGVR, resourceKey(fourth)); snapshots > 3 {
		t.Fatalf("snapshot count = %d, want at most 3", snapshots)
	}
}

func TestMetadataDiffRecorderAllowsConcurrentCapture(t *testing.T) {
	tracker := NewChangeTracker(signalResourceVersion, 0, 5)
	getter := &concurrentFullObjectGetter{obj: testObject("default", "api", "10", 1, "nginx:2")}
	recorder := newMetadataDiffRecorder(getter, tracker, 5)

	obj := testMetadataObject("default", "api", "uid-api", "10", 1)
	tracker.ObserveAdd(deploymentGVR, obj)
	observation := Observation{
		Key:     resourceKey(obj),
		ID:      resourceID(obj),
		Count:   2,
		Changed: true,
		Hot:     true,
	}

	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			recorder.Capture(context.Background(), deploymentGVR, obj, observation)
		}()
	}
	wg.Wait()

	if snapshots := recorder.snapshotCount(deploymentGVR, resourceKey(obj)); snapshots > 6 {
		t.Fatalf("snapshot count = %d, want at most 6", snapshots)
	}
}

type fakeResourceDiscovery struct {
	allCalled               bool
	allLists                []*v1.APIResourceList
	allErr                  error
	resourcesByGroupVersion map[string]*v1.APIResourceList
	errsByGroupVersion      map[string]error
	requestedGroupVersions  []string
}

func (f *fakeResourceDiscovery) ServerResourcesForGroupVersion(groupVersion string) (*v1.APIResourceList, error) {
	f.requestedGroupVersions = append(f.requestedGroupVersions, groupVersion)
	if err := f.errsByGroupVersion[groupVersion]; err != nil {
		return nil, err
	}
	return f.resourcesByGroupVersion[groupVersion], nil
}

func (f *fakeResourceDiscovery) ServerGroupsAndResources() ([]*v1.APIGroup, []*v1.APIResourceList, error) {
	f.allCalled = true
	return nil, f.allLists, f.allErr
}

type recordingRoundTripper struct {
	count int
}

func (r *recordingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	r.count++
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("")),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

type fakeFullObjectGetter struct {
	count   int
	objects []*unstructured.Unstructured
}

func (f *fakeFullObjectGetter) Get(ctx context.Context, gvr schema.GroupVersionResource, namespace, name string) (*unstructured.Unstructured, error) {
	f.count++
	if len(f.objects) == 0 {
		return nil, errors.New("no object")
	}
	obj := f.objects[0]
	f.objects = f.objects[1:]
	return obj, nil
}

type concurrentFullObjectGetter struct {
	lock  sync.Mutex
	count int
	obj   *unstructured.Unstructured
}

func (f *concurrentFullObjectGetter) Get(ctx context.Context, gvr schema.GroupVersionResource, namespace, name string) (*unstructured.Unstructured, error) {
	f.lock.Lock()
	defer f.lock.Unlock()

	f.count++
	return f.obj.DeepCopy(), nil
}
