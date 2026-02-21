package controller

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

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
