package filter

import (
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DefaultExclusions are resource types excluded by default because they are
// high-churn and low-value for semantic search.
var DefaultExclusions = []string{
	"events",
	"leases",
	"endpointslices",
	"componentstatuses",
	"customresourcedefinitions",
}

// ResourceFilter determines which discovered API resources should be watched.
type ResourceFilter struct {
	// If non-empty, only these resource types are watched (allowlist mode)
	watchTypes map[string]bool

	// Resource types to exclude (blocklist mode, used when watchTypes is empty)
	excludeTypes map[string]bool
}

// New creates a ResourceFilter from the given watch and exclude lists.
// If watchTypes is non-empty, it acts as an allowlist (only those types are watched).
// If watchTypes is empty, excludeTypes acts as a blocklist.
func New(watchTypes, excludeTypes []string) *ResourceFilter {
	f := &ResourceFilter{
		watchTypes:   toSet(watchTypes),
		excludeTypes: toSet(excludeTypes),
	}
	return f
}

// ShouldWatch returns true if the given API resource should be watched.
// It checks:
// 1. The resource supports the "list" verb (required for informers)
// 2. The resource passes the allowlist/blocklist filter
func ShouldWatch(f *ResourceFilter, resource metav1.APIResource) bool {
	if !supportsListWatch(resource) {
		return false
	}

	name := strings.ToLower(resource.Name)

	// Allowlist mode: only watch explicitly listed types
	if len(f.watchTypes) > 0 {
		return f.watchTypes[name]
	}

	// Blocklist mode: watch everything except excluded types
	return !f.excludeTypes[name]
}

// supportsListWatch checks if the resource supports both list and watch verbs,
// which are required for informers.
func supportsListWatch(resource metav1.APIResource) bool {
	hasList := false
	hasWatch := false
	for _, verb := range resource.Verbs {
		switch verb {
		case "list":
			hasList = true
		case "watch":
			hasWatch = true
		}
	}
	return hasList && hasWatch
}

func toSet(items []string) map[string]bool {
	if len(items) == 0 {
		return nil
	}
	set := make(map[string]bool, len(items))
	for _, item := range items {
		set[strings.ToLower(strings.TrimSpace(item))] = true
	}
	return set
}
