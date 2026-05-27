package cmd

import (
	"fmt"
	"sync"

	"github.com/google/go-cmp/cmp"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type ResourceHistory struct {
	LastValue string
	Count     int
	Diffs     []string
}

type HistoriesMap map[schema.GroupVersionResource]map[string]ResourceHistory

type ChangeTracker struct {
	lock      sync.Mutex
	signal    changeSignal
	threshold int
	maxDiffs  int
	histories HistoriesMap
}

func NewChangeTracker(signal changeSignal, threshold, maxDiffs int) *ChangeTracker {
	return &ChangeTracker{
		signal:    signal,
		threshold: threshold,
		maxDiffs:  maxDiffs,
		histories: make(HistoriesMap),
	}
}

func (t *ChangeTracker) ObserveAdd(gvr schema.GroupVersionResource, obj *unstructured.Unstructured) {
	if obj == nil {
		return
	}

	value, ok := t.signalValue(obj)
	if !ok {
		return
	}

	t.lock.Lock()
	defer t.lock.Unlock()

	resourceMap := t.resourceMap(gvr)
	id := resourceID(obj)
	if _, exists := resourceMap[id]; exists {
		return
	}
	resourceMap[id] = ResourceHistory{
		LastValue: value,
		Count:     1,
	}
}

func (t *ChangeTracker) ObserveUpdate(gvr schema.GroupVersionResource, oldObj, newObj *unstructured.Unstructured) {
	if newObj == nil {
		return
	}

	newValue, ok := t.signalValue(newObj)
	if !ok {
		return
	}

	id := resourceID(newObj)

	t.lock.Lock()
	defer t.lock.Unlock()

	resourceMap := t.resourceMap(gvr)
	history, exists := resourceMap[id]
	if !exists {
		if oldObj != nil {
			if oldValue, ok := t.signalValue(oldObj); ok {
				history = ResourceHistory{
					LastValue: oldValue,
					Count:     1,
				}
			}
		}
		if history.Count == 0 {
			resourceMap[id] = ResourceHistory{
				LastValue: newValue,
				Count:     1,
			}
			return
		}
	}

	if history.LastValue == newValue {
		resourceMap[id] = history
		return
	}

	history.Count++
	history.LastValue = newValue
	if history.Count > t.threshold && len(history.Diffs) < t.maxDiffs {
		history.Diffs = append(history.Diffs, diffObjects(t.signal, oldObj, newObj))
	}
	resourceMap[id] = history
}

func (t *ChangeTracker) Snapshot() HistoriesMap {
	t.lock.Lock()
	defer t.lock.Unlock()

	snapshot := make(HistoriesMap, len(t.histories))
	for gvr, resourceMap := range t.histories {
		copiedResourceMap := make(map[string]ResourceHistory, len(resourceMap))
		for id, history := range resourceMap {
			copiedHistory := history
			copiedHistory.Diffs = append([]string(nil), history.Diffs...)
			copiedResourceMap[id] = copiedHistory
		}
		snapshot[gvr] = copiedResourceMap
	}
	return snapshot
}

func (t *ChangeTracker) resourceMap(gvr schema.GroupVersionResource) map[string]ResourceHistory {
	resourceMap, exists := t.histories[gvr]
	if !exists {
		resourceMap = make(map[string]ResourceHistory)
		t.histories[gvr] = resourceMap
	}
	return resourceMap
}

func (t *ChangeTracker) signalValue(obj *unstructured.Unstructured) (string, bool) {
	switch t.signal {
	case signalGeneration:
		return fmt.Sprintf("%d", obj.GetGeneration()), true
	case signalResourceVersion:
		version := obj.GetResourceVersion()
		return version, version != ""
	default:
		return "", false
	}
}

func resourceID(obj *unstructured.Unstructured) string {
	if obj.GetNamespace() == "" {
		return obj.GetName()
	}
	return fmt.Sprintf("%s/%s", obj.GetNamespace(), obj.GetName())
}

func diffObjects(signal changeSignal, oldObj, newObj *unstructured.Unstructured) string {
	return cmp.Diff(sanitizeForDiff(signal, oldObj), sanitizeForDiff(signal, newObj))
}

func sanitizeForDiff(signal changeSignal, obj *unstructured.Unstructured) any {
	if obj == nil {
		return nil
	}

	if signal == signalGeneration {
		spec, exists, _ := unstructured.NestedFieldNoCopy(obj.Object, "spec")
		if exists {
			return spec
		}
	}

	copied := obj.DeepCopy()
	unstructured.RemoveNestedField(copied.Object, "metadata", "resourceVersion")
	unstructured.RemoveNestedField(copied.Object, "metadata", "managedFields")
	unstructured.RemoveNestedField(copied.Object, "metadata", "creationTimestamp")
	unstructured.RemoveNestedField(copied.Object, "metadata", "generation")
	unstructured.RemoveNestedField(copied.Object, "metadata", "uid")
	return copied.Object
}
