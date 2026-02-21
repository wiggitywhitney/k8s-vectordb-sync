package filter

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func makeAPIResource(name string, verbs ...string) metav1.APIResource {
	return metav1.APIResource{
		Name:  name,
		Verbs: metav1.Verbs(verbs),
	}
}

func TestShouldWatch_RequiresListAndWatch(t *testing.T) {
	f := New(nil, nil)

	tests := []struct {
		name     string
		resource metav1.APIResource
		want     bool
	}{
		{"list+watch", makeAPIResource("deployments", "list", "watch", "get", "create"), true},
		{"list only", makeAPIResource("deployments", "list", "get"), false},
		{"watch only", makeAPIResource("deployments", "watch", "get"), false},
		{"no verbs", makeAPIResource("deployments"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ShouldWatch(f, tt.resource)
			if got != tt.want {
				t.Errorf("ShouldWatch() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestShouldWatch_BlocklistMode(t *testing.T) {
	f := New(nil, []string{"events", "leases", "endpointslices"})

	tests := []struct {
		name     string
		resource metav1.APIResource
		want     bool
	}{
		{"allowed resource", makeAPIResource("deployments", "list", "watch"), true},
		{"excluded events", makeAPIResource("events", "list", "watch"), false},
		{"excluded leases", makeAPIResource("leases", "list", "watch"), false},
		{"excluded endpointslices", makeAPIResource("endpointslices", "list", "watch"), false},
		{"allowed services", makeAPIResource("services", "list", "watch"), true},
		{"allowed configmaps", makeAPIResource("configmaps", "list", "watch"), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ShouldWatch(f, tt.resource)
			if got != tt.want {
				t.Errorf("ShouldWatch(%q) = %v, want %v", tt.resource.Name, got, tt.want)
			}
		})
	}
}

func TestShouldWatch_AllowlistMode(t *testing.T) {
	f := New([]string{"deployments", "services", "statefulsets"}, nil)

	tests := []struct {
		name     string
		resource metav1.APIResource
		want     bool
	}{
		{"allowed deployment", makeAPIResource("deployments", "list", "watch"), true},
		{"allowed services", makeAPIResource("services", "list", "watch"), true},
		{"allowed statefulsets", makeAPIResource("statefulsets", "list", "watch"), true},
		{"not in allowlist", makeAPIResource("configmaps", "list", "watch"), false},
		{"not in allowlist pods", makeAPIResource("pods", "list", "watch"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ShouldWatch(f, tt.resource)
			if got != tt.want {
				t.Errorf("ShouldWatch(%q) = %v, want %v", tt.resource.Name, got, tt.want)
			}
		})
	}
}

func TestShouldWatch_CaseInsensitive(t *testing.T) {
	f := New(nil, []string{"Events", "LEASES"})

	if ShouldWatch(f, makeAPIResource("events", "list", "watch")) {
		t.Error("Should exclude 'events' even when exclusion was 'Events'")
	}
	if ShouldWatch(f, makeAPIResource("leases", "list", "watch")) {
		t.Error("Should exclude 'leases' even when exclusion was 'LEASES'")
	}
}

func TestShouldWatch_EmptyExcludeList(t *testing.T) {
	f := New(nil, nil)

	// With no exclusions, everything with list+watch should be watched
	if !ShouldWatch(f, makeAPIResource("events", "list", "watch")) {
		t.Error("With empty exclude list, events should be watched")
	}
}

func TestShouldWatch_AllowlistTakesPrecedenceOverBlocklist(t *testing.T) {
	// When both are provided, allowlist mode is used (blocklist is ignored)
	f := New([]string{"deployments"}, []string{"deployments"})

	if !ShouldWatch(f, makeAPIResource("deployments", "list", "watch")) {
		t.Error("Allowlist should take precedence — deployments should be watched")
	}
}

func TestNew_DefaultExclusions(t *testing.T) {
	f := New(nil, DefaultExclusions)

	for _, excluded := range DefaultExclusions {
		resource := makeAPIResource(excluded, "list", "watch")
		if ShouldWatch(f, resource) {
			t.Errorf("Default exclusion %q should not be watched", excluded)
		}
	}
}
