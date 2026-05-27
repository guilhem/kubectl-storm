package cmd

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
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

	history := tracker.Snapshot()[deploymentGVR]["default/api"]
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

	history := tracker.Snapshot()[deploymentGVR]["default/api"]
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

	history := tracker.Snapshot()[deploymentGVR]["default/api"]
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

	history := tracker.Snapshot()[deploymentGVR]["default/api"]
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
	if snapshot[deploymentGVR]["default/api"].Count != 2 {
		t.Fatalf("deployment count = %d, want 2", snapshot[deploymentGVR]["default/api"].Count)
	}
	if snapshot[serviceGVR]["default/api"].Count != 1 {
		t.Fatalf("service count = %d, want 1", snapshot[serviceGVR]["default/api"].Count)
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

func testObject(namespace, name, resourceVersion string, generation int64, image string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata": map[string]any{
				"name":              name,
				"namespace":         namespace,
				"resourceVersion":   resourceVersion,
				"generation":        generation,
				"uid":               "uid-" + name,
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
