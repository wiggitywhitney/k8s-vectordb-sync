package metadata

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func makeUnstructured(namespace, apiVersion, kind, name string, labels, annotations map[string]string, createdAt time.Time) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetNamespace(namespace)
	obj.SetAPIVersion(apiVersion)
	obj.SetKind(kind)
	obj.SetName(name)
	obj.SetLabels(labels)
	obj.SetAnnotations(annotations)
	obj.SetCreationTimestamp(metav1.NewTime(createdAt))
	return obj
}

func TestExtract_NamespacedResource(t *testing.T) {
	created := time.Date(2026, 2, 20, 10, 0, 0, 0, time.UTC)
	obj := makeUnstructured(
		"default", "apps/v1", "Deployment", "nginx",
		map[string]string{"app": "nginx", "tier": "frontend"},
		map[string]string{"description": "Main web server"},
		created,
	)

	result := Extract(obj)

	if result.ID != "default/apps/v1/Deployment/nginx" {
		t.Errorf("ID = %q, want %q", result.ID, "default/apps/v1/Deployment/nginx")
	}
	if result.Namespace != "default" {
		t.Errorf("Namespace = %q, want %q", result.Namespace, "default")
	}
	if result.Name != "nginx" {
		t.Errorf("Name = %q, want %q", result.Name, "nginx")
	}
	if result.Kind != "Deployment" {
		t.Errorf("Kind = %q, want %q", result.Kind, "Deployment")
	}
	if result.APIVersion != "apps/v1" {
		t.Errorf("APIVersion = %q, want %q", result.APIVersion, "apps/v1")
	}
	if result.APIGroup != "apps" {
		t.Errorf("APIGroup = %q, want %q", result.APIGroup, "apps")
	}
	if result.Labels["app"] != "nginx" {
		t.Errorf("Labels[app] = %q, want %q", result.Labels["app"], "nginx")
	}
	if result.Labels["tier"] != "frontend" {
		t.Errorf("Labels[tier] = %q, want %q", result.Labels["tier"], "frontend")
	}
	if result.Annotations["description"] != "Main web server" {
		t.Errorf("Annotations[description] = %q, want %q", result.Annotations["description"], "Main web server")
	}
	if result.CreatedAt != "2026-02-20T10:00:00Z" {
		t.Errorf("CreatedAt = %q, want %q", result.CreatedAt, "2026-02-20T10:00:00Z")
	}
}

func TestExtract_ClusterScopedResource(t *testing.T) {
	created := time.Date(2026, 1, 15, 8, 30, 0, 0, time.UTC)
	obj := makeUnstructured(
		"", "v1", "Namespace", "kube-system",
		map[string]string{"kubernetes.io/metadata.name": "kube-system"},
		nil,
		created,
	)

	result := Extract(obj)

	if result.Namespace != "_cluster" {
		t.Errorf("Namespace = %q, want %q for cluster-scoped resource", result.Namespace, "_cluster")
	}
	if result.ID != "_cluster/v1/Namespace/kube-system" {
		t.Errorf("ID = %q, want %q", result.ID, "_cluster/v1/Namespace/kube-system")
	}
	if result.APIGroup != "" {
		t.Errorf("APIGroup = %q, want empty string for core API group", result.APIGroup)
	}
}

func TestExtractAPIGroup(t *testing.T) {
	tests := []struct {
		apiVersion string
		want       string
	}{
		{"apps/v1", "apps"},
		{"v1", ""},
		{"networking.k8s.io/v1", "networking.k8s.io"},
		{"rbac.authorization.k8s.io/v1", "rbac.authorization.k8s.io"},
		{"batch/v1", "batch"},
	}

	for _, tt := range tests {
		t.Run(tt.apiVersion, func(t *testing.T) {
			got := extractAPIGroup(tt.apiVersion)
			if got != tt.want {
				t.Errorf("extractAPIGroup(%q) = %q, want %q", tt.apiVersion, got, tt.want)
			}
		})
	}
}

func TestFilterAnnotations_SkipsKubectlAnnotations(t *testing.T) {
	annotations := map[string]string{
		"kubectl.kubernetes.io/last-applied-configuration": `{"big":"json"}`,
		"description": "My service",
	}

	result := filterAnnotations(annotations)

	if _, ok := result["kubectl.kubernetes.io/last-applied-configuration"]; ok {
		t.Error("Should have filtered kubectl annotation")
	}
	if result["description"] != "My service" {
		t.Errorf("description = %q, want %q", result["description"], "My service")
	}
}

func TestFilterAnnotations_SkipsHelmAnnotations(t *testing.T) {
	annotations := map[string]string{
		"meta.helm.sh/release-name":      "my-release",
		"meta.helm.sh/release-namespace": "default",
		"helm.sh/chart":                  "mychart-0.1.0",
		"app.kubernetes.io/managed-by":   "Helm",
	}

	result := filterAnnotations(annotations)

	if _, ok := result["meta.helm.sh/release-name"]; ok {
		t.Error("Should have filtered meta.helm.sh annotation")
	}
	if _, ok := result["helm.sh/chart"]; ok {
		t.Error("Should have filtered helm.sh annotation")
	}
	// app.kubernetes.io/managed-by does NOT have a skipped prefix — it's kept
	if result["app.kubernetes.io/managed-by"] != "Helm" {
		t.Errorf("app.kubernetes.io/managed-by should be kept, got %q", result["app.kubernetes.io/managed-by"])
	}
}

func TestFilterAnnotations_ReturnsNilForEmpty(t *testing.T) {
	if result := filterAnnotations(nil); result != nil {
		t.Errorf("Expected nil for nil input, got %v", result)
	}
	if result := filterAnnotations(map[string]string{}); result != nil {
		t.Errorf("Expected nil for empty input, got %v", result)
	}
}

func TestFilterAnnotations_ReturnsNilWhenAllFiltered(t *testing.T) {
	annotations := map[string]string{
		"kubectl.kubernetes.io/last-applied-configuration": `{"big":"json"}`,
		"kubernetes.io/change-cause":                       "applied update",
	}

	result := filterAnnotations(annotations)

	if result != nil {
		t.Errorf("Expected nil when all annotations filtered, got %v", result)
	}
}

func TestExtract_NilLabelsAndAnnotations(t *testing.T) {
	created := time.Date(2026, 2, 20, 10, 0, 0, 0, time.UTC)
	obj := makeUnstructured("default", "v1", "ConfigMap", "test", nil, nil, created)

	result := Extract(obj)

	if result.Labels != nil {
		t.Errorf("Labels should be nil, got %v", result.Labels)
	}
	if result.Annotations != nil {
		t.Errorf("Annotations should be nil, got %v", result.Annotations)
	}
}

func TestBuildID(t *testing.T) {
	tests := []struct {
		namespace, apiVersion, kind, name string
		want                              string
	}{
		{"default", "apps/v1", "Deployment", "nginx", "default/apps/v1/Deployment/nginx"},
		{"_cluster", "v1", "Namespace", "kube-system", "_cluster/v1/Namespace/kube-system"},
		{"monitoring", "monitoring.coreos.com/v1", "Prometheus", "k8s", "monitoring/monitoring.coreos.com/v1/Prometheus/k8s"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := buildID(tt.namespace, tt.apiVersion, tt.kind, tt.name)
			if got != tt.want {
				t.Errorf("buildID() = %q, want %q", got, tt.want)
			}
		})
	}
}
