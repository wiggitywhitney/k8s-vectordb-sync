package controller

import (
	"context"
	"fmt"
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
		CrdEvents:     make(chan CrdEvent, 1000),
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

func TestEmit_AfterStopDoesNotPanic(t *testing.T) {
	// Regression: calling emit() after Stop() must not panic. A previous bug
	// closed the Events channel in Stop(), causing a "send on closed channel"
	// panic when in-flight informer callbacks called emit(). The fix uses
	// stopCh to guard emit instead of closing Events.
	w := &Watcher{
		Events: make(chan ResourceEvent, 10),
		stopCh: make(chan struct{}),
	}

	// Stop the watcher (closes stopCh)
	w.Stop()

	// emit() after Stop() should not panic. Call it multiple times to exercise
	// the path thoroughly — the old code would panic on every call.
	for i := range 100 {
		w.emit(ResourceEvent{
			Type:     EventAdd,
			Instance: testInstance(fmt.Sprintf("after-stop-%d", i)),
		})
	}

	// If we got here without panicking, the test passes.
	// Note: Go's select is non-deterministic when multiple cases are ready,
	// so some events may land in the buffered channel. That's acceptable —
	// the critical invariant is no panic, not that events are dropped.
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

// --- CRD detection and routing tests ---

func makeCRDObj(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "apiextensions.k8s.io/v1",
			"kind":       "CustomResourceDefinition",
			"metadata": map[string]any{
				"name":              name,
				"creationTimestamp": "2026-02-25T10:00:00Z",
			},
		},
	}
}

func TestIsCRD_True(t *testing.T) {
	tests := []struct {
		name       string
		apiVersion string
	}{
		{"v1 CRD", "apiextensions.k8s.io/v1"},
		{"v1beta1 CRD", "apiextensions.k8s.io/v1beta1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obj := &unstructured.Unstructured{
				Object: map[string]any{
					"apiVersion": tt.apiVersion,
					"kind":       "CustomResourceDefinition",
					"metadata":   map[string]any{"name": "test.example.io"},
				},
			}
			if !IsCRD(obj) {
				t.Errorf("IsCRD() = false for apiVersion=%q, want true", tt.apiVersion)
			}
		})
	}
}

func TestIsCRD_False(t *testing.T) {
	tests := []struct {
		name       string
		apiVersion string
		kind       string
	}{
		{"Deployment", "apps/v1", "Deployment"},
		{"Service", "v1", "Service"},
		{"wrong kind same group", "apiextensions.k8s.io/v1", "ConversionReview"},
		{"wrong group same kind", "example.io/v1", "CustomResourceDefinition"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obj := &unstructured.Unstructured{
				Object: map[string]any{
					"apiVersion": tt.apiVersion,
					"kind":       tt.kind,
					"metadata":   map[string]any{"name": "test"},
				},
			}
			if IsCRD(obj) {
				t.Errorf("IsCRD() = true for apiVersion=%q kind=%q, want false", tt.apiVersion, tt.kind)
			}
		})
	}
}

func TestCRDNameExtraction(t *testing.T) {
	tests := []struct {
		crdName string
	}{
		{testCrdName},
		{"issuers.cert-manager.io"},
		{"clusters.postgresql.cnpg.io"},
		{"redis.redis.redis.opstreelabs.in"},
	}

	for _, tt := range tests {
		t.Run(tt.crdName, func(t *testing.T) {
			obj := makeCRDObj(tt.crdName)
			if obj.GetName() != tt.crdName {
				t.Errorf("GetName() = %q, want %q", obj.GetName(), tt.crdName)
			}
		})
	}
}

func newCRDTestWatcher() *Watcher {
	return &Watcher{
		log:       logr.Discard(),
		Events:    make(chan ResourceEvent, 10),
		CrdEvents: make(chan CrdEvent, 10),
		stopCh:    make(chan struct{}),
	}
}

func TestEventRouting_CRDAdd_GoesToCrdEvents(t *testing.T) {
	w := newCRDTestWatcher()

	handler := w.makeEventHandler()
	crd := makeCRDObj(testCrdName)
	handler.OnAdd(crd, false)

	// Should appear in CrdEvents, not Events
	select {
	case event := <-w.CrdEvents:
		if event.Type != EventAdd {
			t.Errorf("CrdEvent.Type = %q, want ADD", event.Type)
		}
		if event.CrdName != testCrdName {
			t.Errorf("CrdEvent.CrdName = %q, want certificates.cert-manager.io", event.CrdName)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Timed out waiting for CRD event on CrdEvents channel")
	}

	// Events channel should be empty
	select {
	case event := <-w.Events:
		t.Errorf("CRD event should not appear in Events channel, got %+v", event)
	default:
		// Good — nothing in Events
	}
}

func TestEventRouting_CRDDelete_GoesToCrdEvents(t *testing.T) {
	w := newCRDTestWatcher()

	handler := w.makeEventHandler()
	crd := makeCRDObj("issuers.cert-manager.io")
	handler.OnDelete(crd)

	select {
	case event := <-w.CrdEvents:
		if event.Type != EventDelete {
			t.Errorf("CrdEvent.Type = %q, want DELETE", event.Type)
		}
		if event.CrdName != "issuers.cert-manager.io" {
			t.Errorf("CrdEvent.CrdName = %q, want issuers.cert-manager.io", event.CrdName)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Timed out waiting for CRD delete event on CrdEvents channel")
	}

	select {
	case event := <-w.Events:
		t.Errorf("CRD delete should not appear in Events channel, got %+v", event)
	default:
	}
}

func TestEventRouting_CRDUpdate_IsIgnored(t *testing.T) {
	w := newCRDTestWatcher()

	handler := w.makeEventHandler()
	oldCrd := makeCRDObj(testCrdName)
	oldCrd.SetResourceVersion("100")
	newCrd := makeCRDObj(testCrdName)
	newCrd.SetResourceVersion("101")

	handler.OnUpdate(oldCrd, newCrd)

	// Neither channel should have events
	select {
	case event := <-w.CrdEvents:
		t.Errorf("CRD update should be ignored, got CrdEvent: %+v", event)
	default:
	}

	select {
	case event := <-w.Events:
		t.Errorf("CRD update should not go to Events, got: %+v", event)
	default:
	}
}

func TestEventRouting_RegularResource_GoesToEvents(t *testing.T) {
	w := newCRDTestWatcher()

	handler := w.makeEventHandler()
	dep := makeUnstructured("apps/v1", "Deployment", "default", "nginx", map[string]string{"app": "nginx"})
	handler.OnAdd(dep, false)

	select {
	case event := <-w.Events:
		if event.Type != EventAdd {
			t.Errorf("ResourceEvent.Type = %q, want ADD", event.Type)
		}
		if event.Instance.Kind != "Deployment" {
			t.Errorf("ResourceEvent.Instance.Kind = %q, want Deployment", event.Instance.Kind)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Timed out waiting for resource event on Events channel")
	}

	select {
	case event := <-w.CrdEvents:
		t.Errorf("Regular resource should not appear in CrdEvents channel, got %+v", event)
	default:
	}
}

func TestEmitCrd_AddNonBlocking(t *testing.T) {
	w := &Watcher{
		CrdEvents: make(chan CrdEvent, 1),
		stopCh:    make(chan struct{}),
		log:       logr.Discard(),
	}

	event1 := CrdEvent{Type: EventAdd, CrdName: "test1.example.io"}
	event2 := CrdEvent{Type: EventAdd, CrdName: "test2.example.io"}

	w.emitCrd(event1)

	// Second should not block (channel full, event dropped)
	done := make(chan bool, 1)
	go func() {
		w.emitCrd(event2)
		done <- true
	}()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("emitCrd() blocked when channel was full")
	}

	received := <-w.CrdEvents
	if received.CrdName != "test1.example.io" {
		t.Errorf("Expected test1.example.io, got %s", received.CrdName)
	}
}

func TestEmitCrd_DeleteBlocks(t *testing.T) {
	w := &Watcher{
		CrdEvents: make(chan CrdEvent, 1),
		stopCh:    make(chan struct{}),
		log:       logr.Discard(),
	}

	// Fill the channel
	w.emitCrd(CrdEvent{Type: EventAdd, CrdName: "filler.example.io"})

	// Delete should block until channel has space
	done := make(chan bool, 1)
	go func() {
		w.emitCrd(CrdEvent{Type: EventDelete, CrdName: "delete.example.io"})
		done <- true
	}()

	// Should NOT complete yet (channel full, delete blocks)
	select {
	case <-done:
		t.Fatal("emitCrd() for delete should block when channel is full")
	case <-time.After(50 * time.Millisecond):
		// Good — blocked as expected
	}

	// Drain the filler to unblock
	<-w.CrdEvents

	// Now the delete should complete
	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("emitCrd() for delete did not unblock after channel drained")
	}

	received := <-w.CrdEvents
	if received.CrdName != "delete.example.io" {
		t.Errorf("Expected delete.example.io, got %s", received.CrdName)
	}
	if received.Type != EventDelete {
		t.Errorf("Expected DELETE, got %s", received.Type)
	}
}

func TestEmitCrd_AfterStopDoesNotPanic(t *testing.T) {
	w := &Watcher{
		CrdEvents: make(chan CrdEvent, 10),
		stopCh:    make(chan struct{}),
		log:       logr.Discard(),
	}

	w.Stop()

	for i := range 100 {
		w.emitCrd(CrdEvent{
			Type:    EventAdd,
			CrdName: fmt.Sprintf("test-%d.example.io", i),
		})
	}
}
