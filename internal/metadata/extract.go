package metadata

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// ResourceInstance holds the extracted metadata for a single Kubernetes resource.
// This matches the JSON payload schema defined in the PRD.
type ResourceInstance struct {
	// Unique identifier: namespace/apiVersion/kind/name
	ID string `json:"id"`

	Namespace  string            `json:"namespace"`
	Name       string            `json:"name"`
	Kind       string            `json:"kind"`
	APIVersion string            `json:"apiVersion"`
	APIGroup   string            `json:"apiGroup"`
	Labels     map[string]string `json:"labels"`

	// Only description-relevant annotations, not kubectl internals
	Annotations map[string]string `json:"annotations"`

	CreatedAt string `json:"createdAt"`
}

// Extract pulls instance metadata from an unstructured Kubernetes object.
// It extracts only the fields needed for semantic search — no spec, status, or managedFields.
func Extract(obj *unstructured.Unstructured) ResourceInstance {
	namespace := obj.GetNamespace()
	if namespace == "" {
		namespace = "_cluster"
	}

	apiVersion := obj.GetAPIVersion()
	kind := obj.GetKind()
	name := obj.GetName()

	return ResourceInstance{
		ID:          buildID(namespace, apiVersion, kind, name),
		Namespace:   namespace,
		Name:        name,
		Kind:        kind,
		APIVersion:  apiVersion,
		APIGroup:    extractAPIGroup(apiVersion),
		Labels:      obj.GetLabels(),
		Annotations: filterAnnotations(obj.GetAnnotations()),
		CreatedAt:   obj.GetCreationTimestamp().UTC().Format("2006-01-02T15:04:05Z"),
	}
}

// buildID constructs the unique resource identifier.
// Format: namespace/apiVersion/kind/name
func buildID(namespace, apiVersion, kind, name string) string {
	return fmt.Sprintf("%s/%s/%s/%s", namespace, apiVersion, kind, name)
}

// extractAPIGroup extracts the API group from an apiVersion string.
// "apps/v1" → "apps", "v1" → "" (core group)
func extractAPIGroup(apiVersion string) string {
	parts := strings.SplitN(apiVersion, "/", 2)
	if len(parts) == 2 {
		return parts[0]
	}
	return ""
}

// skipAnnotationPrefixes lists annotation prefixes that are high-volume internal metadata
// not useful for semantic search.
var skipAnnotationPrefixes = []string{
	"kubectl.kubernetes.io/",
	"meta.helm.sh/",
	"helm.sh/",
	"deployment.kubernetes.io/",
	"control-plane.alpha.kubernetes.io/",
	"kubernetes.io/",
}

// filterAnnotations returns only annotations useful for semantic search.
// It skips kubectl configuration annotations, helm metadata, and other
// high-volume internal annotations that add noise without search value.
func filterAnnotations(annotations map[string]string) map[string]string {
	if len(annotations) == 0 {
		return nil
	}

	filtered := make(map[string]string)
	for key, value := range annotations {
		if shouldSkipAnnotation(key) {
			continue
		}
		filtered[key] = value
	}

	if len(filtered) == 0 {
		return nil
	}
	return filtered
}

func shouldSkipAnnotation(key string) bool {
	for _, prefix := range skipAnnotationPrefixes {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}
