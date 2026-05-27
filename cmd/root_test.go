package cmd

import (
	"errors"
	"reflect"
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

func TestUnstructuredFromObjectSupportsDeletedFinalStateUnknown(t *testing.T) {
	want := testObject("default", "api", "1", 1, "nginx:1")

	got, ok := unstructuredFromObject(cache.DeletedFinalStateUnknown{Obj: want})
	if !ok {
		t.Fatal("DeletedFinalStateUnknown should be accepted")
	}
	if got != want {
		t.Fatal("unexpected object recovered from tombstone")
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
