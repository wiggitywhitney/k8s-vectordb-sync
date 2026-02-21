package controller

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/wiggitywhitney/k8s-vectordb-sync/internal/metadata"
)

func makeTestObj(namespace, name, resourceVersion string, labels, annotations map[string]string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetNamespace(namespace)
	obj.SetName(name)
	obj.SetResourceVersion(resourceVersion)
	obj.SetLabels(labels)
	obj.SetAnnotations(annotations)
	return obj
}

func TestMetadataChanged_SameResourceVersion(t *testing.T) {
	old := makeTestObj("default", "nginx", "100", map[string]string{"app": "nginx"}, nil)
	new := makeTestObj("default", "nginx", "100", map[string]string{"app": "changed"}, nil)

	if MetadataChanged(old, new) {
		t.Error("Should return false when resource version is the same")
	}
}

func TestMetadataChanged_LabelsChanged(t *testing.T) {
	old := makeTestObj("default", "nginx", "100", map[string]string{"app": "nginx"}, nil)
	new := makeTestObj("default", "nginx", "101", map[string]string{"app": "nginx", "version": "v2"}, nil)

	if !MetadataChanged(old, new) {
		t.Error("Should detect label changes")
	}
}

func TestMetadataChanged_AnnotationsChanged(t *testing.T) {
	old := makeTestObj("kube-system", "nginx", "100", nil, map[string]string{"note": "old"})
	new := makeTestObj("kube-system", "nginx", "101", nil, map[string]string{"note": "new"})

	if !MetadataChanged(old, new) {
		t.Error("Should detect annotation changes")
	}
}

func TestMetadataChanged_NoMetadataChange(t *testing.T) {
	labels := map[string]string{"app": "nginx"}
	annotations := map[string]string{"note": "same"}

	old := makeTestObj("default", "web-server", "100", labels, annotations)
	new := makeTestObj("default", "web-server", "101", labels, annotations)

	if MetadataChanged(old, new) {
		t.Error("Should return false when only resource version changed (no metadata change)")
	}
}

func TestMetadataChanged_LabelRemoved(t *testing.T) {
	old := makeTestObj("default", "nginx", "100", map[string]string{"app": "nginx", "tier": "frontend"}, nil)
	new := makeTestObj("default", "nginx", "101", map[string]string{"app": "nginx"}, nil)

	if !MetadataChanged(old, new) {
		t.Error("Should detect label removal")
	}
}

func TestMetadataChanged_NilToEmptyLabels(t *testing.T) {
	old := makeTestObj("default", "nginx", "100", nil, nil)
	new := makeTestObj("default", "nginx", "101", map[string]string{}, nil)

	if MetadataChanged(old, new) {
		t.Error("nil and empty map should be considered equal")
	}
}

func TestMapsEqual(t *testing.T) {
	tests := []struct {
		name string
		a, b map[string]string
		want bool
	}{
		{"both nil", nil, nil, true},
		{"nil and empty", nil, map[string]string{}, true},
		{"same contents", map[string]string{"a": "1"}, map[string]string{"a": "1"}, true},
		{"different values", map[string]string{"a": "1"}, map[string]string{"a": "2"}, false},
		{"different keys", map[string]string{"a": "1"}, map[string]string{"b": "1"}, false},
		{"different lengths", map[string]string{"a": "1"}, map[string]string{"a": "1", "b": "2"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mapsEqual(tt.a, tt.b)
			if got != tt.want {
				t.Errorf("mapsEqual() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEmit_NonBlocking(t *testing.T) {
	// Create a watcher with a small channel buffer
	w := &Watcher{
		Events: make(chan ResourceEvent, 1),
	}

	event1 := ResourceEvent{Type: EventAdd, Instance: testInstance("test-1")}
	event2 := ResourceEvent{Type: EventAdd, Instance: testInstance("test-2")}

	// First emit should succeed
	w.emit(event1)

	// Second emit should not block (channel is full, event is dropped)
	done := make(chan bool, 1)
	go func() {
		w.emit(event2)
		done <- true
	}()

	select {
	case <-done:
		// Good — emit didn't block
	case <-time.After(1 * time.Second):
		t.Fatal("emit() blocked when channel was full")
	}

	// Verify first event is in channel
	received := <-w.Events
	if received.Instance.ID != "test-1" {
		t.Errorf("Expected test-1, got %s", received.Instance.ID)
	}
}

func testInstance(id string) metadata.ResourceInstance {
	return metadata.ResourceInstance{ID: id}
}

// makeFakeWatcher creates a Watcher backed by a fake dynamic client for testing
// TriggerResync without a real cluster.
func makeFakeWatcher(objects []runtime.Object, gvrs []schema.GroupVersionResource) *Watcher {
	scheme := runtime.NewScheme()
	fakeClient := dynamicfake.NewSimpleDynamicClient(scheme, objects...)

	return &Watcher{
		log:           logr.Discard(),
		dynamicClient: fakeClient,
		Events:        make(chan ResourceEvent, 1000),
		watchedGVRs:   gvrs,
		stopCh:        make(chan struct{}),
	}
}

func makeUnstructured(apiVersion, kind, namespace, name string, labels map[string]string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": apiVersion,
			"kind":       kind,
			"metadata": map[string]any{
				"name":              name,
				"namespace":         namespace,
				"creationTimestamp": "2026-02-21T10:00:00Z",
			},
		},
	}
	if labels != nil {
		obj.SetLabels(labels)
	}
	return obj
}

func TestTriggerResync_EmitsEventsForAllResources(t *testing.T) {
	dep1 := makeUnstructured("apps/v1", "Deployment", "default", "nginx", map[string]string{"app": "nginx"})
	dep2 := makeUnstructured("apps/v1", "Deployment", "staging", "web", map[string]string{"app": "web"})

	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	w := makeFakeWatcher([]runtime.Object{dep1, dep2}, []schema.GroupVersionResource{gvr})

	count, err := w.TriggerResync(context.Background())
	if err != nil {
		t.Fatalf("TriggerResync failed: %v", err)
	}

	if count != 2 {
		t.Errorf("Expected 2 resources resynced, got %d", count)
	}

	// Drain events and verify
	var events []ResourceEvent
	for range count {
		select {
		case e := <-w.Events:
			events = append(events, e)
		case <-time.After(1 * time.Second):
			t.Fatal("Timed out waiting for resync events")
		}
	}

	for _, e := range events {
		if e.Type != EventAdd {
			t.Errorf("Expected ADD event, got %s", e.Type)
		}
	}
}

func TestTriggerResync_MultipleGVRs(t *testing.T) {
	dep := makeUnstructured("apps/v1", "Deployment", "default", "nginx", nil)
	svc := makeUnstructured("v1", "Service", "default", "nginx-svc", nil)

	depGVR := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	svcGVR := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "services"}
	w := makeFakeWatcher([]runtime.Object{dep, svc}, []schema.GroupVersionResource{depGVR, svcGVR})

	count, err := w.TriggerResync(context.Background())
	if err != nil {
		t.Fatalf("TriggerResync failed: %v", err)
	}

	if count != 2 {
		t.Errorf("Expected 2 resources across GVRs, got %d", count)
	}
}

func TestTriggerResync_NoWatchedGVRs(t *testing.T) {
	w := makeFakeWatcher(nil, nil)

	count, err := w.TriggerResync(context.Background())
	if err != nil {
		t.Fatalf("TriggerResync failed: %v", err)
	}

	if count != 0 {
		t.Errorf("Expected 0 resources with no GVRs, got %d", count)
	}
}

func TestTriggerResync_RespectsContextCancellation(t *testing.T) {
	dep := makeUnstructured("apps/v1", "Deployment", "default", "nginx", nil)
	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}

	w := makeFakeWatcher([]runtime.Object{dep}, []schema.GroupVersionResource{gvr})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	// Should not hang — the cancelled context may cause list to fail or return empty
	count, _ := w.TriggerResync(ctx)
	_ = count // We don't assert on count — just verify it doesn't hang
}

func TestWatchedGVRCount(t *testing.T) {
	gvrs := []schema.GroupVersionResource{
		{Group: "apps", Version: "v1", Resource: "deployments"},
		{Group: "", Version: "v1", Resource: "services"},
	}
	w := makeFakeWatcher(nil, gvrs)

	if w.WatchedGVRCount() != 2 {
		t.Errorf("Expected 2, got %d", w.WatchedGVRCount())
	}
}

func TestWatchedGVRCount_Empty(t *testing.T) {
	w := makeFakeWatcher(nil, nil)

	if w.WatchedGVRCount() != 0 {
		t.Errorf("Expected 0, got %d", w.WatchedGVRCount())
	}
}
