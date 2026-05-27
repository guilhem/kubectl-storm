package cmd

import (
	"fmt"
	"sync"

	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type ObservedObject interface {
	metav1.Object
}

type Observation struct {
	Key     string
	ID      string
	Count   int
	Changed bool
	Hot     bool
}

type ResourceHistory struct {
	ID        string
	UID       string
	LastValue string
	Count     int
	Diffs     []string
	Deleted   bool
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

func (t *ChangeTracker) ObserveAdd(gvr schema.GroupVersionResource, obj ObservedObject) Observation {
	if obj == nil {
		return Observation{}
	}

	value, ok := t.signalValue(obj)
	if !ok {
		return Observation{}
	}

	t.lock.Lock()
	defer t.lock.Unlock()

	resourceMap := t.resourceMap(gvr)
	key := resourceKey(obj)
	history, exists := resourceMap[key]
	if exists {
		history.ID = resourceID(obj)
		history.UID = resourceUID(obj)
		history.LastValue = value
		history.Deleted = false
		if history.Count == 0 {
			history.Count = 1
		}
		resourceMap[key] = history
		return t.observation(key, history, false)
	}
	history = newResourceHistory(obj, value, 1)
	resourceMap[key] = history
	return t.observation(key, history, false)
}

func (t *ChangeTracker) ObserveUpdate(gvr schema.GroupVersionResource, oldObj, newObj ObservedObject) Observation {
	if newObj == nil {
		return Observation{}
	}

	if isReplacementUpdate(oldObj, newObj) {
		t.ObserveDelete(gvr, oldObj)
		return t.ObserveAdd(gvr, newObj)
	}

	newValue, ok := t.signalValue(newObj)
	if !ok {
		return Observation{}
	}

	key := resourceKey(newObj)

	t.lock.Lock()
	defer t.lock.Unlock()

	resourceMap := t.resourceMap(gvr)
	history, exists := resourceMap[key]
	if !exists {
		if oldObj != nil {
			if oldValue, ok := t.signalValue(oldObj); ok {
				history = newResourceHistory(newObj, oldValue, 1)
			}
		}
		if history.Count == 0 {
			history = newResourceHistory(newObj, newValue, 1)
			resourceMap[key] = history
			return t.observation(key, history, false)
		}
	}

	if history.LastValue == newValue {
		history.ID = resourceID(newObj)
		history.UID = resourceUID(newObj)
		history.Deleted = false
		resourceMap[key] = history
		return t.observation(key, history, false)
	}

	history.Count++
	history.LastValue = newValue
	history.ID = resourceID(newObj)
	history.UID = resourceUID(newObj)
	history.Deleted = false
	if history.Count > t.threshold && len(history.Diffs) < t.maxDiffs {
		oldUnstructured, oldOK := oldObj.(*unstructured.Unstructured)
		newUnstructured, newOK := newObj.(*unstructured.Unstructured)
		if oldOK && newOK {
			history.Diffs = append(history.Diffs, diffObjects(t.signal, oldUnstructured, newUnstructured))
		}
	}
	resourceMap[key] = history
	return t.observation(key, history, true)
}

func (t *ChangeTracker) ObserveDelete(gvr schema.GroupVersionResource, obj ObservedObject) {
	if obj == nil {
		return
	}

	key := resourceKey(obj)

	t.lock.Lock()
	defer t.lock.Unlock()

	resourceMap, exists := t.histories[gvr]
	if !exists {
		return
	}

	history, exists := resourceMap[key]
	if !exists {
		return
	}

	if len(history.Diffs) == 0 {
		delete(resourceMap, key)
		if len(resourceMap) == 0 {
			delete(t.histories, gvr)
		}
		return
	}

	history.ID = resourceID(obj)
	history.UID = resourceUID(obj)
	history.Deleted = true
	resourceMap[key] = history
}

func (t *ChangeTracker) AddDiff(gvr schema.GroupVersionResource, key, diff string) bool {
	t.lock.Lock()
	defer t.lock.Unlock()

	resourceMap, exists := t.histories[gvr]
	if !exists {
		return false
	}

	history, exists := resourceMap[key]
	if !exists || len(history.Diffs) >= t.maxDiffs {
		return false
	}

	history.Diffs = append(history.Diffs, diff)
	resourceMap[key] = history
	return true
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

func (t *ChangeTracker) observation(key string, history ResourceHistory, changed bool) Observation {
	return Observation{
		Key:     key,
		ID:      history.ID,
		Count:   history.Count,
		Changed: changed,
		Hot:     history.Count > t.threshold,
	}
}

func newResourceHistory(obj ObservedObject, value string, count int) ResourceHistory {
	return ResourceHistory{
		ID:        resourceID(obj),
		UID:       resourceUID(obj),
		LastValue: value,
		Count:     count,
	}
}

func (t *ChangeTracker) signalValue(obj ObservedObject) (string, bool) {
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

func resourceID(obj ObservedObject) string {
	if obj.GetNamespace() == "" {
		return obj.GetName()
	}
	return fmt.Sprintf("%s/%s", obj.GetNamespace(), obj.GetName())
}

func resourceUID(obj ObservedObject) string {
	return string(obj.GetUID())
}

func resourceKey(obj ObservedObject) string {
	id := resourceID(obj)
	uid := resourceUID(obj)
	if uid == "" {
		return id
	}
	return fmt.Sprintf("%s#%s", id, uid)
}

func isReplacementUpdate(oldObj, newObj ObservedObject) bool {
	if oldObj == nil || newObj == nil {
		return false
	}
	oldUID := resourceUID(oldObj)
	newUID := resourceUID(newObj)
	return oldUID != "" && newUID != "" && oldUID != newUID
}

func diffObjects(signal changeSignal, oldObj, newObj *unstructured.Unstructured) string {
	return diffSanitized(sanitizeForDiff(signal, oldObj), sanitizeForDiff(signal, newObj))
}

func diffSanitized(oldObj, newObj any) string {
	return cmp.Diff(oldObj, newObj)
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
