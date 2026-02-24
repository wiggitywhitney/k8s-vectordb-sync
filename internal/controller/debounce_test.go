package controller

import (
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/wiggitywhitney/k8s-vectordb-sync/internal/config"
	"github.com/wiggitywhitney/k8s-vectordb-sync/internal/metadata"
)

const testDeploymentID = "default/apps/v1/Deployment/nginx"

func testConfig(debounceMs, flushMs, maxBatch int) config.Config {
	return config.Config{
		DebounceWindow:     time.Duration(debounceMs) * time.Millisecond,
		BatchFlushInterval: time.Duration(flushMs) * time.Millisecond,
		BatchMaxSize:       maxBatch,
	}
}

func makeInstance(id, namespace, name, kind string) metadata.ResourceInstance {
	return metadata.ResourceInstance{
		ID:         id,
		Namespace:  namespace,
		Name:       name,
		Kind:       kind,
		APIVersion: "apps/v1",
	}
}

func TestDebounce_DeleteSkipsDebounce(t *testing.T) {
	cfg := testConfig(5000, 5000, 100) // Long windows so upserts won't flush
	db := NewDebounceBuffer(logr.Discard(), cfg)

	events := make(chan ResourceEvent, 10)
	ctx, cancel := t.Context(), func() {} // t.Context() auto-cancels on test end
	_ = cancel

	go db.Run(ctx, events)

	// Send a delete event
	events <- ResourceEvent{
		Type:     EventDelete,
		Instance: makeInstance(testDeploymentID, "default", "nginx", "Deployment"),
	}

	// Delete should arrive immediately (not debounced)
	select {
	case payload := <-db.Payloads:
		if len(payload.Deletes) != 1 {
			t.Fatalf("Expected 1 delete, got %d", len(payload.Deletes))
		}
		if payload.Deletes[0] != testDeploymentID {
			t.Errorf("Delete ID = %q, want nginx", payload.Deletes[0])
		}
		if len(payload.Upserts) != 0 {
			t.Errorf("Expected 0 upserts in delete payload, got %d", len(payload.Upserts))
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Delete was not forwarded immediately")
	}
}

func TestDebounce_UpsertIsDebounced(t *testing.T) {
	cfg := testConfig(100, 5000, 25) // 100ms debounce, long flush interval
	db := NewDebounceBuffer(logr.Discard(), cfg)

	events := make(chan ResourceEvent, 10)
	ctx, cancel := t.Context(), func() {}
	_ = cancel

	go db.Run(ctx, events)

	// Send an add event
	events <- ResourceEvent{
		Type:     EventAdd,
		Instance: makeInstance(testDeploymentID, "default", "nginx", "Deployment"),
	}

	// Should NOT arrive immediately
	select {
	case <-db.Payloads:
		t.Fatal("Upsert should be debounced, not sent immediately")
	case <-time.After(50 * time.Millisecond):
		// Good — still debouncing
	}

	// After debounce window + flush, it should arrive
	// Wait for debounce (100ms) + some margin, then close events to trigger flush
	time.Sleep(150 * time.Millisecond)
	close(events) // triggers channel-closed path in Run which flushes

	// Collect payloads
	upserts := make([]metadata.ResourceInstance, 0, 1)
	for payload := range db.Payloads {
		upserts = append(upserts, payload.Upserts...)
	}

	if len(upserts) != 1 {
		t.Fatalf("Expected 1 upsert after flush, got %d", len(upserts))
	}
	if upserts[0].ID != testDeploymentID {
		t.Errorf("Upsert ID = %q, want nginx", upserts[0].ID)
	}
}

func TestDebounce_LastStateWins(t *testing.T) {
	cfg := testConfig(100, 5000, 10) // 100ms debounce
	db := NewDebounceBuffer(logr.Discard(), cfg)

	events := make(chan ResourceEvent, 10)
	ctx, cancel := t.Context(), func() {}
	_ = cancel

	go db.Run(ctx, events)

	// Send multiple updates to the same resource rapidly
	events <- ResourceEvent{
		Type:     EventAdd,
		Instance: makeInstance(testDeploymentID, "default", "nginx", "Deployment"),
	}
	events <- ResourceEvent{
		Type: EventUpdate,
		Instance: metadata.ResourceInstance{
			ID:     testDeploymentID,
			Name:   "nginx",
			Kind:   "Deployment",
			Labels: map[string]string{"version": "v1"},
		},
	}
	events <- ResourceEvent{
		Type: EventUpdate,
		Instance: metadata.ResourceInstance{
			ID:     testDeploymentID,
			Name:   "nginx",
			Kind:   "Deployment",
			Labels: map[string]string{"version": "v2"},
		},
	}

	// Wait for debounce then close to flush
	time.Sleep(150 * time.Millisecond)
	close(events)

	// Should get exactly 1 upsert with the latest state
	upserts := make([]metadata.ResourceInstance, 0, 1)
	for payload := range db.Payloads {
		upserts = append(upserts, payload.Upserts...)
	}

	if len(upserts) != 1 {
		t.Fatalf("Expected 1 upsert (last-state-wins), got %d", len(upserts))
	}
	if upserts[0].Labels["version"] != "v2" {
		t.Errorf("Labels[version] = %q, want v2 (last state)", upserts[0].Labels["version"])
	}
}

func TestDebounce_DeleteCancelsPendingUpsert(t *testing.T) {
	cfg := testConfig(5000, 5000, 100) // Long debounce
	db := NewDebounceBuffer(logr.Discard(), cfg)

	events := make(chan ResourceEvent, 10)
	ctx, cancel := t.Context(), func() {}
	_ = cancel

	go db.Run(ctx, events)

	// Add then immediately delete
	events <- ResourceEvent{
		Type:     EventAdd,
		Instance: makeInstance(testDeploymentID, "default", "nginx", "Deployment"),
	}

	// Small delay to ensure add is processed
	time.Sleep(10 * time.Millisecond)

	events <- ResourceEvent{
		Type:     EventDelete,
		Instance: makeInstance(testDeploymentID, "default", "nginx", "Deployment"),
	}

	// Delete should arrive immediately
	select {
	case payload := <-db.Payloads:
		if len(payload.Deletes) != 1 {
			t.Fatalf("Expected 1 delete, got %d deletes, %d upserts", len(payload.Deletes), len(payload.Upserts))
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Delete was not forwarded")
	}

	// The pending upsert should have been cancelled
	if db.PendingCountForTesting() != 0 {
		t.Errorf("Expected 0 pending after delete cancelled the add, got %d", db.PendingCountForTesting())
	}
}

func TestDebounce_FlushOnInterval(t *testing.T) {
	cfg := testConfig(10, 200, 100) // 10ms debounce, 200ms flush interval
	db := NewDebounceBuffer(logr.Discard(), cfg)

	events := make(chan ResourceEvent, 10)
	ctx, cancel := t.Context(), func() {}
	_ = cancel

	go db.Run(ctx, events)

	// Send an event
	events <- ResourceEvent{
		Type:     EventAdd,
		Instance: makeInstance("kube-system/v1/ConfigMap/test", "kube-system", "test", "ConfigMap"),
	}

	// Wait for debounce (10ms) + flush interval (200ms) + margin
	select {
	case payload := <-db.Payloads:
		if len(payload.Upserts) != 1 {
			t.Fatalf("Expected 1 upsert on flush interval, got %d", len(payload.Upserts))
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Flush interval did not trigger")
	}
}

func TestDebounce_BatchMultipleResources(t *testing.T) {
	cfg := testConfig(10, 200, 100) // 10ms debounce, 200ms flush
	db := NewDebounceBuffer(logr.Discard(), cfg)

	events := make(chan ResourceEvent, 10)
	ctx, cancel := t.Context(), func() {}
	_ = cancel

	go db.Run(ctx, events)

	// Send events for different resources
	events <- ResourceEvent{
		Type:     EventAdd,
		Instance: makeInstance("default/apps/v1/Deployment/web", "default", "web", "Deployment"),
	}
	events <- ResourceEvent{
		Type:     EventAdd,
		Instance: makeInstance("staging/v1/Service/web-svc", "staging", "web-svc", "Service"),
	}
	events <- ResourceEvent{
		Type:     EventAdd,
		Instance: makeInstance("monitoring/v1/ConfigMap/config", "monitoring", "config", "ConfigMap"),
	}

	// Wait for debounce + flush
	time.Sleep(250 * time.Millisecond)
	close(events)

	// Collect all upserts across payloads
	var totalUpserts int
	for payload := range db.Payloads {
		totalUpserts += len(payload.Upserts)
	}

	if totalUpserts != 3 {
		t.Errorf("Expected 3 total upserts batched, got %d", totalUpserts)
	}
}

func TestDebounce_SeparateUpsertAndDeletePayloads(t *testing.T) {
	cfg := testConfig(10, 200, 100)
	db := NewDebounceBuffer(logr.Discard(), cfg)

	events := make(chan ResourceEvent, 10)
	ctx, cancel := t.Context(), func() {}
	_ = cancel

	go db.Run(ctx, events)

	// Send a mix of adds and deletes
	events <- ResourceEvent{
		Type:     EventAdd,
		Instance: makeInstance("prod/apps/v1/Deployment/new-app", "prod", "new-app", "Deployment"),
	}
	events <- ResourceEvent{
		Type:     EventDelete,
		Instance: makeInstance("prod/apps/v1/Deployment/old-app", "prod", "old-app", "Deployment"),
	}

	// Delete arrives immediately as its own payload
	select {
	case payload := <-db.Payloads:
		if len(payload.Deletes) != 1 || payload.Deletes[0] != "prod/apps/v1/Deployment/old-app" {
			t.Errorf("Expected delete payload for old-app, got deletes=%v upserts=%d", payload.Deletes, len(payload.Upserts))
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Delete payload not received")
	}

	// Upsert arrives after debounce + flush
	time.Sleep(250 * time.Millisecond)
	close(events)

	var gotUpsert bool
	for payload := range db.Payloads {
		if len(payload.Upserts) > 0 {
			gotUpsert = true
			if payload.Upserts[0].ID != "prod/apps/v1/Deployment/new-app" {
				t.Errorf("Expected upsert for new-app, got %s", payload.Upserts[0].ID)
			}
		}
	}

	if !gotUpsert {
		t.Error("Never received upsert payload for new-app")
	}
}

func TestDebounce_DebounceResetsOnNewEvent(t *testing.T) {
	cfg := testConfig(100, 5000, 10) // 100ms debounce, long flush
	db := NewDebounceBuffer(logr.Discard(), cfg)

	events := make(chan ResourceEvent, 10)
	ctx, cancel := t.Context(), func() {}
	_ = cancel

	go db.Run(ctx, events)

	// Send first event
	events <- ResourceEvent{
		Type:     EventAdd,
		Instance: makeInstance(testDeploymentID, "default", "nginx", "Deployment"),
	}

	// Wait 60ms (less than 100ms debounce), send another event
	time.Sleep(60 * time.Millisecond)
	events <- ResourceEvent{
		Type: EventUpdate,
		Instance: metadata.ResourceInstance{
			ID:     testDeploymentID,
			Name:   "nginx",
			Kind:   "Deployment",
			Labels: map[string]string{"updated": "true"},
		},
	}

	// At 60ms the timer was reset. So it should fire at 60+100=160ms from start.
	// At 100ms from start, it should NOT have fired yet.
	time.Sleep(40 * time.Millisecond) // now at ~100ms from start

	if db.PendingCountForTesting() != 1 {
		t.Errorf("Expected 1 pending (debounce reset), got %d", db.PendingCountForTesting())
	}

	// Wait for debounce to fire, then close to flush
	time.Sleep(120 * time.Millisecond) // now at ~220ms, well past 160ms
	close(events)

	upserts := make([]metadata.ResourceInstance, 0, 1)
	for payload := range db.Payloads {
		upserts = append(upserts, payload.Upserts...)
	}

	if len(upserts) != 1 {
		t.Fatalf("Expected 1 upsert, got %d", len(upserts))
	}
	if upserts[0].Labels["updated"] != "true" {
		t.Error("Expected last state (updated=true)")
	}
}

func TestDebounce_ChannelCloseFlushesAllPending(t *testing.T) {
	// Regression: closing the events channel must flush ALL pending entries,
	// including those whose debounce timers haven't fired yet. A previous bug
	// used flushPending() (ready-only) instead of flushAllPending() on channel close.
	cfg := testConfig(10000, 10000, 100) // 10s debounce — timer will not fire during this test
	db := NewDebounceBuffer(logr.Discard(), cfg)

	events := make(chan ResourceEvent, 10)
	ctx, cancel := t.Context(), func() {}
	_ = cancel

	go db.Run(ctx, events)

	// Send an ADD event
	events <- ResourceEvent{
		Type:     EventAdd,
		Instance: makeInstance(testDeploymentID, "default", "nginx", "Deployment"),
	}

	// Small delay to ensure the event is processed before closing
	time.Sleep(10 * time.Millisecond)

	// Close events channel BEFORE the debounce timer fires
	close(events)

	// The Payloads channel should receive the pending upsert and then close
	upserts := make([]metadata.ResourceInstance, 0, 1)
	for payload := range db.Payloads {
		upserts = append(upserts, payload.Upserts...)
	}

	if len(upserts) != 1 {
		t.Fatalf("Expected 1 upsert flushed on channel close, got %d", len(upserts))
	}
	if upserts[0].ID != testDeploymentID {
		t.Errorf("Upsert ID = %q, want %q", upserts[0].ID, testDeploymentID)
	}
}

func TestDebounce_FlushDoesNotRearmTimers(t *testing.T) {
	// Regression: when flushInterval < debounceWindow, flush ticks that fire
	// before the debounce timer must not prevent the entry from eventually being
	// flushed. A previous bug could re-arm or lose timers during frequent flushes.
	cfg := testConfig(200, 50, 100) // 200ms debounce, 50ms flush interval
	db := NewDebounceBuffer(logr.Discard(), cfg)

	events := make(chan ResourceEvent, 10)
	ctx, cancel := t.Context(), func() {}
	_ = cancel

	go db.Run(ctx, events)

	// Send an ADD event
	events <- ResourceEvent{
		Type:     EventAdd,
		Instance: makeInstance("default/v1/ConfigMap/flush-test", "default", "flush-test", "ConfigMap"),
	}

	// Wait longer than the debounce window (200ms) plus margin for flush interval to fire
	select {
	case payload := <-db.Payloads:
		if len(payload.Upserts) != 1 {
			t.Fatalf("Expected 1 upsert, got %d", len(payload.Upserts))
		}
		if payload.Upserts[0].ID != "default/v1/ConfigMap/flush-test" {
			t.Errorf("Upsert ID = %q, want flush-test resource", payload.Upserts[0].ID)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Entry was never flushed — frequent flush ticks prevented debounce timer from working")
	}
}

func TestSyncPayload_JSONStructure(t *testing.T) {
	payload := SyncPayload{
		Upserts: []metadata.ResourceInstance{
			{
				ID:         testDeploymentID,
				Namespace:  "default",
				Name:       "nginx",
				Kind:       "Deployment",
				APIVersion: "apps/v1",
				APIGroup:   "apps",
				Labels:     map[string]string{"app": "nginx"},
			},
		},
		Deletes: []string{"default/apps/v1/Deployment/old-service"},
	}

	if len(payload.Upserts) != 1 {
		t.Errorf("Upserts = %d, want 1", len(payload.Upserts))
	}
	if len(payload.Deletes) != 1 {
		t.Errorf("Deletes = %d, want 1", len(payload.Deletes))
	}
	if payload.Upserts[0].ID != testDeploymentID {
		t.Errorf("Upsert ID = %q, want nginx", payload.Upserts[0].ID)
	}
	if payload.Deletes[0] != "default/apps/v1/Deployment/old-service" {
		t.Errorf("Delete ID = %q, want old-service", payload.Deletes[0])
	}
}
