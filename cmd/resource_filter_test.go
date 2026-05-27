package cmd

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestParseResourceFilter(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  schema.GroupVersionResource
	}{
		{
			name:  "named group",
			value: "apps/v1/deployments",
			want:  schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
		},
		{
			name:  "core alias",
			value: "core/v1/pods",
			want:  schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"},
		},
		{
			name:  "empty core group",
			value: "/v1/services",
			want:  schema.GroupVersionResource{Group: "", Version: "v1", Resource: "services"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseResourceFilter(test.value)
			if err != nil {
				t.Fatalf("parseResourceFilter() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("parseResourceFilter() = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestParseResourceFilterRejectsInvalidValues(t *testing.T) {
	for _, value := range []string{"pods", "v1/pods", "apps/v1/", "apps//deployments"} {
		if _, err := parseResourceFilter(value); err == nil {
			t.Fatalf("parseResourceFilter(%q) should fail", value)
		}
	}
}

func TestBuildResourceMatcherIncludesDefaultExcludes(t *testing.T) {
	include, exclude, err := buildResourceMatcher([]string{"apps/v1/deployments"}, []string{"batch/v1/jobs"})
	if err != nil {
		t.Fatalf("buildResourceMatcher() error = %v", err)
	}

	if !include.Matches(schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}) {
		t.Fatal("include matcher should include apps/v1/deployments")
	}
	for _, gvr := range []schema.GroupVersionResource{
		{Group: "", Version: "v1", Resource: "events"},
		{Group: "events.k8s.io", Version: "v1", Resource: "events"},
		{Group: "coordination.k8s.io", Version: "v1", Resource: "leases"},
		{Group: "batch", Version: "v1", Resource: "jobs"},
	} {
		if !exclude.Matches(gvr) {
			t.Fatalf("exclude matcher should include %s", gvr.String())
		}
	}
}

func TestShouldWatchResource(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	resource := v1.APIResource{Name: "deployments", Verbs: []string{"get", "list", "watch"}}
	include, exclude, err := buildResourceMatcher(nil, nil)
	if err != nil {
		t.Fatalf("buildResourceMatcher() error = %v", err)
	}

	if !shouldWatchResource(gvr, resource, include, exclude) {
		t.Fatal("expected deployments to be watched")
	}

	if shouldWatchResource(gvr, v1.APIResource{Name: "deployments/status", Verbs: []string{"list", "watch"}}, include, exclude) {
		t.Fatal("expected subresources to be skipped")
	}
	if shouldWatchResource(gvr, v1.APIResource{Name: "deployments", Verbs: []string{"watch"}}, include, exclude) {
		t.Fatal("expected resources without list to be skipped")
	}
	if shouldWatchResource(schema.GroupVersionResource{Version: "v1", Resource: "events"}, v1.APIResource{Name: "events", Verbs: []string{"list", "watch"}}, include, exclude) {
		t.Fatal("expected default excluded events to be skipped")
	}
}
