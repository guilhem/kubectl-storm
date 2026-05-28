package cmd

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

var deploymentGVR = schema.GroupVersionResource{
	Group:    "apps",
	Version:  "v1",
	Resource: "deployments",
}

func TestTrackerResourceVersionCountsWritesAfterThreshold(t *testing.T) {
	tracker := NewChangeTracker(signalResourceVersion, 2, 5)

	tracker.ObserveAdd(deploymentGVR, testObject("default", "api", "1", 1, "nginx:1"))
	tracker.ObserveUpdate(deploymentGVR, testObject("default", "api", "1", 1, "nginx:1"), testObject("default", "api", "2", 1, "nginx:1"))
	tracker.ObserveUpdate(deploymentGVR, testObject("default", "api", "2", 1, "nginx:1"), testObject("default", "api", "3", 1, "nginx:2"))

	history := historyByID(t, tracker.Snapshot(), deploymentGVR, "default/api")
	if history.Count != 3 {
		t.Fatalf("count = %d, want 3", history.Count)
	}
	if len(history.Diffs) != 1 {
		t.Fatalf("diff count = %d, want 1", len(history.Diffs))
	}
	if strings.Contains(history.Diffs[0], "resourceVersion") {
		t.Fatalf("diff should not include resourceVersion: %s", history.Diffs[0])
	}
	if !strings.Contains(history.Diffs[0], "nginx:2") {
		t.Fatalf("diff should include spec change: %s", history.Diffs[0])
	}
}

func TestTrackerGenerationIgnoresResourceVersionOnlyUpdates(t *testing.T) {
	tracker := NewChangeTracker(signalGeneration, 1, 5)

	tracker.ObserveAdd(deploymentGVR, testObject("default", "api", "1", 1, "nginx:1"))
	tracker.ObserveUpdate(deploymentGVR, testObject("default", "api", "1", 1, "nginx:1"), testObject("default", "api", "2", 1, "nginx:2"))

	history := historyByID(t, tracker.Snapshot(), deploymentGVR, "default/api")
	if history.Count != 1 {
		t.Fatalf("count = %d, want 1", history.Count)
	}
	if len(history.Diffs) != 0 {
		t.Fatalf("diff count = %d, want 0", len(history.Diffs))
	}
}

func TestTrackerGenerationCountsGenerationChanges(t *testing.T) {
	tracker := NewChangeTracker(signalGeneration, 1, 5)

	tracker.ObserveAdd(deploymentGVR, testObject("default", "api", "1", 1, "nginx:1"))
	tracker.ObserveUpdate(deploymentGVR, testObject("default", "api", "1", 1, "nginx:1"), testObject("default", "api", "2", 2, "nginx:2"))

	history := historyByID(t, tracker.Snapshot(), deploymentGVR, "default/api")
	if history.Count != 2 {
		t.Fatalf("count = %d, want 2", history.Count)
	}
	if len(history.Diffs) != 1 {
		t.Fatalf("diff count = %d, want 1", len(history.Diffs))
	}
	if strings.Contains(history.Diffs[0], "resourceVersion") {
		t.Fatalf("generation diff should focus on spec: %s", history.Diffs[0])
	}
	if !strings.Contains(history.Diffs[0], "nginx:2") {
		t.Fatalf("generation diff should include spec change: %s", history.Diffs[0])
	}
}

func TestTrackerInitializesMissingHistoryFromUpdate(t *testing.T) {
	tracker := NewChangeTracker(signalResourceVersion, 1, 5)

	tracker.ObserveUpdate(deploymentGVR, testObject("default", "api", "1", 1, "nginx:1"), testObject("default", "api", "2", 1, "nginx:2"))

	history := historyByID(t, tracker.Snapshot(), deploymentGVR, "default/api")
	if history.Count != 2 {
		t.Fatalf("count = %d, want 2", history.Count)
	}
	if len(history.Diffs) != 1 {
		t.Fatalf("diff count = %d, want 1", len(history.Diffs))
	}
}

func TestTrackerIsolatesResources(t *testing.T) {
	tracker := NewChangeTracker(signalResourceVersion, 1, 5)
	serviceGVR := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "services"}

	tracker.ObserveAdd(deploymentGVR, testObject("default", "api", "1", 1, "nginx:1"))
	tracker.ObserveAdd(serviceGVR, testObject("default", "api", "1", 1, "nginx:1"))
	tracker.ObserveUpdate(deploymentGVR, testObject("default", "api", "1", 1, "nginx:1"), testObject("default", "api", "2", 1, "nginx:2"))

	snapshot := tracker.Snapshot()
	deploymentHistory := historyByID(t, snapshot, deploymentGVR, "default/api")
	if deploymentHistory.Count != 2 {
		t.Fatalf("deployment count = %d, want 2", deploymentHistory.Count)
	}
	serviceHistory := historyByID(t, snapshot, serviceGVR, "default/api")
	if serviceHistory.Count != 1 {
		t.Fatalf("service count = %d, want 1", serviceHistory.Count)
	}
}

func TestTrackerDeleteDropsHistoryWithoutDiffs(t *testing.T) {
	tracker := NewChangeTracker(signalResourceVersion, 2, 5)
	obj := testObject("default", "api", "1", 1, "nginx:1")

	tracker.ObserveAdd(deploymentGVR, obj)
	tracker.ObserveDelete(deploymentGVR, obj)

	if _, exists := tracker.Snapshot()[deploymentGVR]; exists {
		t.Fatal("history without diffs should be removed after delete")
	}
}

func TestTrackerDeleteKeepsHistoryWithDiffs(t *testing.T) {
	tracker := NewChangeTracker(signalResourceVersion, 1, 5)
	oldObj := testObject("default", "api", "1", 1, "nginx:1")
	newObj := testObject("default", "api", "2", 1, "nginx:2")

	tracker.ObserveAdd(deploymentGVR, oldObj)
	tracker.ObserveUpdate(deploymentGVR, oldObj, newObj)
	tracker.ObserveDelete(deploymentGVR, newObj)

	history := historyByID(t, tracker.Snapshot(), deploymentGVR, "default/api")
	if !history.Deleted {
		t.Fatal("history with diffs should be marked deleted")
	}
	if len(history.Diffs) != 1 {
		t.Fatalf("diff count = %d, want 1", len(history.Diffs))
	}
}

func TestTrackerReplacementUpdateUsesUID(t *testing.T) {
	tracker := NewChangeTracker(signalResourceVersion, 1, 5)
	oldObj := testObjectWithUID("default", "api", "uid-old", "1", 1, "nginx:1")
	oldChanged := testObjectWithUID("default", "api", "uid-old", "2", 1, "nginx:2")
	recreated := testObjectWithUID("default", "api", "uid-new", "3", 1, "nginx:3")

	tracker.ObserveAdd(deploymentGVR, oldObj)
	tracker.ObserveUpdate(deploymentGVR, oldObj, oldChanged)
	tracker.ObserveUpdate(deploymentGVR, oldChanged, recreated)

	snapshot := tracker.Snapshot()[deploymentGVR]
	if len(snapshot) != 2 {
		t.Fatalf("history count = %d, want 2", len(snapshot))
	}

	oldHistory := historyByUID(t, tracker.Snapshot(), deploymentGVR, "uid-old")
	if !oldHistory.Deleted {
		t.Fatal("old UID history should be marked deleted")
	}
	if oldHistory.Count != 2 {
		t.Fatalf("old history count = %d, want 2", oldHistory.Count)
	}

	newHistory := historyByUID(t, tracker.Snapshot(), deploymentGVR, "uid-new")
	if newHistory.Deleted {
		t.Fatal("new UID history should be active")
	}
	if newHistory.Count != 1 {
		t.Fatalf("new history count = %d, want 1", newHistory.Count)
	}
}

func TestTrackerAcceptsPartialObjectMetadata(t *testing.T) {
	tracker := NewChangeTracker(signalResourceVersion, 2, 5)
	first := testMetadataObject("default", "api", "uid-api", "1", 1)
	second := testMetadataObject("default", "api", "uid-api", "2", 1)
	third := testMetadataObject("default", "api", "uid-api", "3", 1)

	tracker.ObserveAdd(deploymentGVR, first)
	tracker.ObserveUpdate(deploymentGVR, first, second)
	observation := tracker.ObserveUpdate(deploymentGVR, second, third)

	if !observation.Hot {
		t.Fatal("third observed value should be hot")
	}

	history := historyByID(t, tracker.Snapshot(), deploymentGVR, "default/api")
	if history.Count != 3 {
		t.Fatalf("count = %d, want 3", history.Count)
	}
	if len(history.Diffs) != 0 {
		t.Fatalf("metadata tracker should not create full diffs by itself, got %d", len(history.Diffs))
	}
}

func TestSanitizeDiffRemovesVolatileFields(t *testing.T) {
	oldObj := testObject("default", "api", "1", 1, "nginx:1")
	newObj := testObject("default", "api", "2", 2, "nginx:2")

	diff := diffObjects(signalResourceVersion, oldObj, newObj)
	for _, field := range []string{"resourceVersion", "managedFields", "creationTimestamp", "generation", "uid"} {
		if strings.Contains(diff, field) {
			t.Fatalf("diff should not include %s: %s", field, diff)
		}
	}
	if !strings.Contains(diff, "nginx:2") {
		t.Fatalf("diff should include spec change: %s", diff)
	}
}

func testMetadataObject(namespace, name, uid, resourceVersion string, generation int64) *metav1.PartialObjectMetadata {
	return &metav1.PartialObjectMetadata{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			UID:             types.UID(uid),
			ResourceVersion: resourceVersion,
			Generation:      generation,
		},
	}
}

func testObject(namespace, name, resourceVersion string, generation int64, image string) *unstructured.Unstructured {
	return testObjectWithUID(namespace, name, "uid-"+name, resourceVersion, generation, image)
}

func testObjectWithUID(namespace, name, uid, resourceVersion string, generation int64, image string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata": map[string]any{
				"name":              name,
				"namespace":         namespace,
				"resourceVersion":   resourceVersion,
				"generation":        generation,
				"uid":               uid,
				"creationTimestamp": "2026-05-27T10:00:00Z",
				"managedFields": []any{
					map[string]any{"manager": "kubectl"},
				},
			},
			"spec": map[string]any{
				"template": map[string]any{
					"spec": map[string]any{
						"containers": []any{
							map[string]any{"name": "api", "image": image},
						},
					},
				},
			},
		},
	}
}

func historyByID(t *testing.T, histories HistoriesMap, gvr schema.GroupVersionResource, id string) ResourceHistory {
	t.Helper()

	for _, history := range histories[gvr] {
		if history.ID == id {
			return history
		}
	}
	t.Fatalf("missing history for id %s", id)
	return ResourceHistory{}
}

func historyByUID(t *testing.T, histories HistoriesMap, gvr schema.GroupVersionResource, uid string) ResourceHistory {
	t.Helper()

	for _, history := range histories[gvr] {
		if history.UID == uid {
			return history
		}
	}
	t.Fatalf("missing history for uid %s", uid)
	return ResourceHistory{}
}
