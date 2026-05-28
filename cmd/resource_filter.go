package cmd

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

type resourceMatcher map[schema.GroupVersionResource]struct{}

func buildResourceMatcher(includes, excludes []string) (resourceMatcher, resourceMatcher, error) {
	includeMatcher, err := newResourceMatcher(includes)
	if err != nil {
		return nil, nil, err
	}

	excludeMatcher, err := newResourceMatcher(append(defaultExcludedResources(), excludes...))
	if err != nil {
		return nil, nil, err
	}

	return includeMatcher, excludeMatcher, nil
}

func newResourceMatcher(filters []string) (resourceMatcher, error) {
	matcher := make(resourceMatcher)
	for _, filter := range filters {
		gvr, err := parseResourceFilter(filter)
		if err != nil {
			return nil, err
		}
		matcher[gvr] = struct{}{}
	}
	return matcher, nil
}

func defaultExcludedResources() []string {
	return []string{
		"core/v1/events",
		"events.k8s.io/v1/events",
		"coordination.k8s.io/v1/leases",
	}
}

func parseResourceFilter(value string) (schema.GroupVersionResource, error) {
	resourceSeparator := strings.LastIndex(value, "/")
	if resourceSeparator < 0 {
		return schema.GroupVersionResource{}, fmt.Errorf("resource filter %q must use group/version/resource", value)
	}

	groupVersion := value[:resourceSeparator]
	resource := value[resourceSeparator+1:]
	if !strings.Contains(groupVersion, "/") || resource == "" {
		return schema.GroupVersionResource{}, fmt.Errorf("resource filter %q must include version and resource", value)
	}

	groupVersion = strings.TrimPrefix(groupVersion, "/")
	groupVersion = strings.TrimPrefix(groupVersion, "core/")

	gv, err := schema.ParseGroupVersion(groupVersion)
	if err != nil {
		return schema.GroupVersionResource{}, fmt.Errorf("resource filter %q has invalid group/version: %w", value, err)
	}
	if gv.Version == "" {
		return schema.GroupVersionResource{}, fmt.Errorf("resource filter %q must include version and resource", value)
	}
	return gv.WithResource(resource), nil
}

func (m resourceMatcher) Empty() bool {
	return len(m) == 0
}

func (m resourceMatcher) Matches(gvr schema.GroupVersionResource) bool {
	_, ok := m[gvr]
	return ok
}
