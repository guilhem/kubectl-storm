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
	parts := strings.Split(value, "/")
	if len(parts) != 3 {
		return schema.GroupVersionResource{}, fmt.Errorf("resource filter %q must use group/version/resource", value)
	}

	group := parts[0]
	if group == "core" {
		group = ""
	}

	gvr := schema.GroupVersionResource{
		Group:    group,
		Version:  parts[1],
		Resource: parts[2],
	}

	if gvr.Version == "" || gvr.Resource == "" {
		return schema.GroupVersionResource{}, fmt.Errorf("resource filter %q must include version and resource", value)
	}

	return gvr, nil
}

func (m resourceMatcher) Empty() bool {
	return len(m) == 0
}

func (m resourceMatcher) Matches(gvr schema.GroupVersionResource) bool {
	_, ok := m[gvr]
	return ok
}
