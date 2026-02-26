package controller

import (
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/wiggitywhitney/k8s-vectordb-sync/internal/config"
)

const testCrdName = "certificates.cert-manager.io"

func crdTestConfig(debounceMs, flushMs, maxBatch int) config.Config {
	return config.Config{
		DebounceWindow:     time.Duration(debounceMs) * time.Millisecond,
		BatchFlushInterval: time.Duration(flushMs) * time.Millisecond,
		BatchMaxSize:       maxBatch,
	}
}

func TestCrdSyncPayload_JSONStructure(t *testing.T) {
	payload := CrdSyncPayload{
		Upserts: []string{"certificates.cert-manager.io", "issuers.cert-manager.io"},
		Deletes: []string{"challenges.cert-manager.io"},
	}

	if len(payload.Upserts) != 2 {
		t.Errorf("Upserts = %d, want 2", len(payload.Upserts))
	}
	if len(payload.Deletes) != 1 {
		t.Errorf("Deletes = %d, want 1", len(payload.Deletes))
	}
	if payload.Upserts[0] != "certificates.cert-manager.io" {
		t.Errorf("Upserts[0] = %q, want certificates.cert-manager.io", payload.Upserts[0])
	}
	if payload.Deletes[0] != "challenges.cert-manager.io" {
		t.Errorf("Deletes[0] = %q, want challenges.cert-manager.io", payload.Deletes[0])
	}
}

func TestCrdSyncPayload_IsEmpty(t *testing.T) {
	empty := CrdSyncPayload{}
	if !empty.IsEmpty() {
		t.Error("Empty CrdSyncPayload should report IsEmpty() = true")
	}

	withAdded := CrdSyncPayload{Upserts: []string{"test.example.io"}}
	if withAdded.IsEmpty() {
		t.Error("CrdSyncPayload with Upserts should not be empty")
	}

	withDeleted := CrdSyncPayload{Deletes: []string{"test.example.io"}}
	if withDeleted.IsEmpty() {
		t.Error("CrdSyncPayload with Deletes should not be empty")
	}
}

func TestCrdDebounce_DeleteSkipsDebounce(t *testing.T) {
	cfg := crdTestConfig(5000, 5000, 100) // Long windows so adds won't flush
	db := NewCrdDebounceBuffer(logr.Discard(), cfg)

	events := make(chan CrdEvent, 10)
	go db.Run(t.Context(), events)

	// Send a delete event
	events <- CrdEvent{
		Type:    EventDelete,
		CrdName: testCrdName,
	}

	// Delete should arrive immediately (not debounced)
	select {
	case payload := <-db.Payloads:
		if len(payload.Deletes) != 1 {
			t.Fatalf("Expected 1 delete, got %d", len(payload.Deletes))
		}
		if payload.Deletes[0] != testCrdName {
			t.Errorf("Delete name = %q, want %q", payload.Deletes[0], testCrdName)
		}
		if len(payload.Upserts) != 0 {
			t.Errorf("Expected 0 upserts in delete payload, got %d", len(payload.Upserts))
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Delete was not forwarded immediately")
	}
}

func TestCrdDebounce_AddIsDebounced(t *testing.T) {
	cfg := crdTestConfig(100, 5000, 25) // 100ms debounce, long flush interval
	db := NewCrdDebounceBuffer(logr.Discard(), cfg)

	events := make(chan CrdEvent, 10)
	go db.Run(t.Context(), events)

	// Send an add event
	events <- CrdEvent{
		Type:    EventAdd,
		CrdName: testCrdName,
	}

	// Should NOT arrive immediately
	select {
	case <-db.Payloads:
		t.Fatal("Add should be debounced, not sent immediately")
	case <-time.After(50 * time.Millisecond):
		// Good — still debouncing
	}

	// After debounce window, close events to trigger flush
	time.Sleep(150 * time.Millisecond)
	close(events)

	// Collect payloads
	added := make([]string, 0, 1)
	for payload := range db.Payloads {
		added = append(added, payload.Upserts...)
	}

	if len(added) != 1 {
		t.Fatalf("Expected 1 added after flush, got %d", len(added))
	}
	if added[0] != testCrdName {
		t.Errorf("Upserts[0] = %q, want %q", added[0], testCrdName)
	}
}

func TestCrdDebounce_DeduplicatesRepeatedAdds(t *testing.T) {
	cfg := crdTestConfig(100, 5000, 10) // 100ms debounce
	db := NewCrdDebounceBuffer(logr.Discard(), cfg)

	events := make(chan CrdEvent, 10)
	go db.Run(t.Context(), events)

	// Send multiple adds for the same CRD
	events <- CrdEvent{Type: EventAdd, CrdName: testCrdName}
	events <- CrdEvent{Type: EventAdd, CrdName: testCrdName}
	events <- CrdEvent{Type: EventAdd, CrdName: testCrdName}

	// Wait for debounce then close to flush
	time.Sleep(150 * time.Millisecond)
	close(events)

	// Should get exactly 1 add (deduplicated)
	added := make([]string, 0, 1)
	for payload := range db.Payloads {
		added = append(added, payload.Upserts...)
	}

	if len(added) != 1 {
		t.Fatalf("Expected 1 add (deduplicated), got %d", len(added))
	}
	if added[0] != testCrdName {
		t.Errorf("Upserts[0] = %q, want %q", added[0], testCrdName)
	}
}

func TestCrdDebounce_DeleteCancelsPendingAdd(t *testing.T) {
	cfg := crdTestConfig(5000, 5000, 100) // Long debounce
	db := NewCrdDebounceBuffer(logr.Discard(), cfg)

	events := make(chan CrdEvent, 10)
	go db.Run(t.Context(), events)

	// Add then immediately delete
	events <- CrdEvent{Type: EventAdd, CrdName: testCrdName}

	// Small delay to ensure add is processed
	time.Sleep(10 * time.Millisecond)

	events <- CrdEvent{Type: EventDelete, CrdName: testCrdName}

	// Delete should arrive immediately
	select {
	case payload := <-db.Payloads:
		if len(payload.Deletes) != 1 {
			t.Fatalf("Expected 1 delete, got %d deletes, %d added", len(payload.Deletes), len(payload.Upserts))
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Delete was not forwarded")
	}

	// The pending add should have been cancelled
	if db.PendingCountForTesting() != 0 {
		t.Errorf("Expected 0 pending after delete cancelled the add, got %d", db.PendingCountForTesting())
	}
}

func TestCrdDebounce_BatchMultipleCrds(t *testing.T) {
	cfg := crdTestConfig(10, 200, 100) // 10ms debounce, 200ms flush
	db := NewCrdDebounceBuffer(logr.Discard(), cfg)

	events := make(chan CrdEvent, 10)
	go db.Run(t.Context(), events)

	// Send events for different CRDs (simulating operator install)
	events <- CrdEvent{Type: EventAdd, CrdName: "certificates.cert-manager.io"}
	events <- CrdEvent{Type: EventAdd, CrdName: "issuers.cert-manager.io"}
	events <- CrdEvent{Type: EventAdd, CrdName: "clusterissuers.cert-manager.io"}

	// Wait for debounce + flush
	time.Sleep(250 * time.Millisecond)
	close(events)

	// Collect all adds across payloads
	var totalAdded int
	for payload := range db.Payloads {
		totalAdded += len(payload.Upserts)
	}

	if totalAdded != 3 {
		t.Errorf("Expected 3 total CRDs batched, got %d", totalAdded)
	}
}

func TestCrdDebounce_FlushOnInterval(t *testing.T) {
	cfg := crdTestConfig(10, 200, 100) // 10ms debounce, 200ms flush interval
	db := NewCrdDebounceBuffer(logr.Discard(), cfg)

	events := make(chan CrdEvent, 10)
	go db.Run(t.Context(), events)

	// Send an event
	events <- CrdEvent{Type: EventAdd, CrdName: testCrdName}

	// Wait for debounce (10ms) + flush interval (200ms) + margin
	select {
	case payload := <-db.Payloads:
		if len(payload.Upserts) != 1 {
			t.Fatalf("Expected 1 add on flush interval, got %d", len(payload.Upserts))
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Flush interval did not trigger")
	}
}

func TestCrdDebounce_ChannelCloseFlushesAllPending(t *testing.T) {
	cfg := crdTestConfig(10000, 10000, 100) // 10s debounce — timer will not fire
	db := NewCrdDebounceBuffer(logr.Discard(), cfg)

	events := make(chan CrdEvent, 10)
	go db.Run(t.Context(), events)

	// Send an ADD event
	events <- CrdEvent{Type: EventAdd, CrdName: testCrdName}

	// Small delay to ensure the event is processed before closing
	time.Sleep(10 * time.Millisecond)

	// Close events channel BEFORE the debounce timer fires
	close(events)

	// The Payloads channel should receive the pending add and then close
	added := make([]string, 0, 1)
	for payload := range db.Payloads {
		added = append(added, payload.Upserts...)
	}

	if len(added) != 1 {
		t.Fatalf("Expected 1 add flushed on channel close, got %d", len(added))
	}
	if added[0] != testCrdName {
		t.Errorf("Upserts[0] = %q, want %q", added[0], testCrdName)
	}
}

func TestCrdDebounce_SeparateAddAndDeletePayloads(t *testing.T) {
	cfg := crdTestConfig(10, 200, 100)
	db := NewCrdDebounceBuffer(logr.Discard(), cfg)

	events := make(chan CrdEvent, 10)
	go db.Run(t.Context(), events)

	// Send a mix of adds and deletes
	events <- CrdEvent{Type: EventAdd, CrdName: "certificates.cert-manager.io"}
	events <- CrdEvent{Type: EventDelete, CrdName: "challenges.cert-manager.io"}

	// Delete arrives immediately as its own payload
	select {
	case payload := <-db.Payloads:
		if len(payload.Deletes) != 1 || payload.Deletes[0] != "challenges.cert-manager.io" {
			t.Errorf("Expected delete payload for challenges, got deletes=%v upserts=%d", payload.Deletes, len(payload.Upserts))
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Delete payload not received")
	}

	// Add arrives after debounce + flush
	time.Sleep(250 * time.Millisecond)
	close(events)

	var gotAdd bool
	for payload := range db.Payloads {
		if len(payload.Upserts) > 0 {
			gotAdd = true
			if payload.Upserts[0] != "certificates.cert-manager.io" {
				t.Errorf("Expected add for certificates, got %s", payload.Upserts[0])
			}
		}
	}

	if !gotAdd {
		t.Error("Never received add payload for certificates.cert-manager.io")
	}
}
