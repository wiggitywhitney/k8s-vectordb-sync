//go:build integration

package internal_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/wiggitywhitney/k8s-vectordb-sync/internal/client"
	"github.com/wiggitywhitney/k8s-vectordb-sync/internal/config"
	"github.com/wiggitywhitney/k8s-vectordb-sync/internal/controller"
	"github.com/wiggitywhitney/k8s-vectordb-sync/internal/metadata"
)

// receivedPayload records a payload received by the test HTTP server.
type receivedPayload struct {
	Payload    controller.SyncPayload
	ReceivedAt time.Time
}

// payloadCollector is a thread-safe collector for payloads received by httptest servers.
type payloadCollector struct {
	mu       sync.Mutex
	payloads []receivedPayload
}

func (c *payloadCollector) add(p controller.SyncPayload) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.payloads = append(c.payloads, receivedPayload{
		Payload:    p,
		ReceivedAt: time.Now(),
	})
}

func (c *payloadCollector) all() []receivedPayload {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make([]receivedPayload, len(c.payloads))
	copy(result, c.payloads)
	return result
}

func (c *payloadCollector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.payloads)
}

// allUpserts returns all upserted ResourceInstances across all received payloads.
func (c *payloadCollector) allUpserts() []metadata.ResourceInstance {
	c.mu.Lock()
	defer c.mu.Unlock()
	var result []metadata.ResourceInstance
	for _, rp := range c.payloads {
		result = append(result, rp.Payload.Upserts...)
	}
	return result
}

// allDeletes returns all delete IDs across all received payloads.
func (c *payloadCollector) allDeletes() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var result []string
	for _, rp := range c.payloads {
		result = append(result, rp.Payload.Deletes...)
	}
	return result
}

// setupTestServer creates an httptest server that records all received SyncPayloads.
func setupTestServer(t *testing.T) (*httptest.Server, *payloadCollector) {
	t.Helper()
	collector := &payloadCollector{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read error", http.StatusInternalServerError)
			return
		}

		var payload controller.SyncPayload
		if err := json.Unmarshal(body, &payload); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}

		collector.add(payload)
		w.WriteHeader(http.StatusOK)
	}))

	t.Cleanup(func() { server.Close() })
	return server, collector
}

// makeTestConfig creates a config.Config with the given debounce and flush settings.
func makeTestConfig(debounceMs, flushMs, maxBatch int) config.Config {
	return config.Config{
		DebounceWindow:     time.Duration(debounceMs) * time.Millisecond,
		BatchFlushInterval: time.Duration(flushMs) * time.Millisecond,
		BatchMaxSize:       maxBatch,
	}
}

// makeInstance creates a ResourceInstance for testing.
func makeInstance(id, namespace, name, kind string) metadata.ResourceInstance {
	return metadata.ResourceInstance{
		ID:         id,
		Namespace:  namespace,
		Name:       name,
		Kind:       kind,
		APIVersion: "apps/v1",
	}
}

// startSenderLoop starts a goroutine that reads payloads from the debouncer
// and sends them via the REST client, mirroring the sender loop in cmd/main.go.
// Returns a channel that is closed when the sender loop exits.
func startSenderLoop(ctx context.Context, restClient *client.RESTClient, debouncer *controller.DebounceBuffer) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for payload := range debouncer.Payloads {
			sendCtx := ctx
			if ctx.Err() != nil {
				// During shutdown, use a short-lived context for drain
				drainCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				sendCtx = drainCtx
				_ = restClient.Send(sendCtx, payload)
				cancel()
				continue
			}
			_ = restClient.Send(sendCtx, payload)
		}
	}()
	return done
}

func TestIntegration_FullPipeline_EventsFlowThroughDebounceToHTTPServer(t *testing.T) {
	server, collector := setupTestServer(t)

	cfg := makeTestConfig(50, 200, 100) // 50ms debounce, 200ms flush
	debouncer := controller.NewDebounceBuffer(logr.Discard(), cfg)
	restClient := client.New(logr.Discard(), server.URL, client.WithTimeout(5*time.Second))

	events := make(chan controller.ResourceEvent, 10)
	ctx := t.Context()

	// Start the debouncer
	go debouncer.Run(ctx, events)
	// Start the sender loop
	senderDone := startSenderLoop(ctx, restClient, debouncer)

	// Feed events simulating what the watcher would produce
	events <- controller.ResourceEvent{
		Type:     controller.EventAdd,
		Instance: makeInstance("default/apps/v1/Deployment/web", "default", "web", "Deployment"),
	}
	events <- controller.ResourceEvent{
		Type:     controller.EventAdd,
		Instance: makeInstance("staging/v1/Service/api-svc", "staging", "api-svc", "Service"),
	}

	// Wait for debounce (50ms) + flush interval (200ms) + margin
	time.Sleep(400 * time.Millisecond)

	// Close events channel to trigger final flush and shutdown
	close(events)

	// Wait for sender to drain
	select {
	case <-senderDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Sender did not finish within timeout")
	}

	// Verify the HTTP server received the correct upserts
	upserts := collector.allUpserts()
	if len(upserts) != 2 {
		t.Fatalf("Expected 2 upserts at HTTP server, got %d", len(upserts))
	}

	upsertIDs := make(map[string]bool)
	for _, u := range upserts {
		upsertIDs[u.ID] = true
	}
	if !upsertIDs["default/apps/v1/Deployment/web"] {
		t.Error("Missing upsert for default/apps/v1/Deployment/web")
	}
	if !upsertIDs["staging/v1/Service/api-svc"] {
		t.Error("Missing upsert for staging/v1/Service/api-svc")
	}
}

func TestIntegration_DeleteEventsArriveImmediately(t *testing.T) {
	server, collector := setupTestServer(t)

	// Long debounce window — upserts would take 10s+ to arrive
	cfg := makeTestConfig(10000, 10000, 100)
	debouncer := controller.NewDebounceBuffer(logr.Discard(), cfg)
	restClient := client.New(logr.Discard(), server.URL, client.WithTimeout(5*time.Second))

	events := make(chan controller.ResourceEvent, 10)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	go debouncer.Run(ctx, events)
	_ = startSenderLoop(ctx, restClient, debouncer)

	sentAt := time.Now()

	// Send a DELETE event
	events <- controller.ResourceEvent{
		Type:     controller.EventDelete,
		Instance: makeInstance("default/apps/v1/Deployment/old-app", "default", "old-app", "Deployment"),
	}

	// Poll for the delete to arrive at the HTTP server — should be almost immediate
	deadline := time.After(1 * time.Second)
	for {
		deletes := collector.allDeletes()
		if len(deletes) > 0 {
			elapsed := time.Since(sentAt)
			if elapsed > 1*time.Second {
				t.Errorf("Delete took %v to arrive, expected < 1s", elapsed)
			}
			if deletes[0] != "default/apps/v1/Deployment/old-app" {
				t.Errorf("Delete ID = %q, want default/apps/v1/Deployment/old-app", deletes[0])
			}
			break
		}
		select {
		case <-deadline:
			t.Fatal("Delete event did not arrive at HTTP server within 1s (debounce window is 10s)")
		case <-time.After(10 * time.Millisecond):
			// Poll again
		}
	}
}

func TestIntegration_RapidUpdates_LastStateWinsEndToEnd(t *testing.T) {
	server, collector := setupTestServer(t)

	cfg := makeTestConfig(50, 200, 100) // 50ms debounce, 200ms flush
	debouncer := controller.NewDebounceBuffer(logr.Discard(), cfg)
	restClient := client.New(logr.Discard(), server.URL, client.WithTimeout(5*time.Second))

	events := make(chan controller.ResourceEvent, 10)
	ctx := t.Context()

	go debouncer.Run(ctx, events)
	senderDone := startSenderLoop(ctx, restClient, debouncer)

	resourceID := "default/apps/v1/Deployment/rapid-app"

	// Send 5 rapid updates to the same resource with different labels
	for i := range 5 {
		labels := map[string]string{
			"version": labelForIteration(i),
		}
		events <- controller.ResourceEvent{
			Type: controller.EventUpdate,
			Instance: metadata.ResourceInstance{
				ID:         resourceID,
				Namespace:  "default",
				Name:       "rapid-app",
				Kind:       "Deployment",
				APIVersion: "apps/v1",
				Labels:     labels,
			},
		}
	}

	// Wait for debounce + flush + margin
	time.Sleep(400 * time.Millisecond)
	close(events)

	select {
	case <-senderDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Sender did not finish within timeout")
	}

	// The HTTP server should have received exactly ONE upsert for this resource
	// with the LAST state (version=v5)
	upserts := collector.allUpserts()
	if len(upserts) != 1 {
		t.Fatalf("Expected exactly 1 upsert (last-state-wins), got %d", len(upserts))
	}
	if upserts[0].ID != resourceID {
		t.Errorf("Upsert ID = %q, want %q", upserts[0].ID, resourceID)
	}
	if upserts[0].Labels["version"] != "v5" {
		t.Errorf("Labels[version] = %q, want v5 (last state)", upserts[0].Labels["version"])
	}
}

func TestIntegration_GracefulShutdownDrainsPendingPayloads(t *testing.T) {
	server, collector := setupTestServer(t)

	// Long debounce/flush — events would normally sit for a long time
	cfg := makeTestConfig(10000, 10000, 100)
	debouncer := controller.NewDebounceBuffer(logr.Discard(), cfg)
	restClient := client.New(logr.Discard(), server.URL, client.WithTimeout(5*time.Second))

	events := make(chan controller.ResourceEvent, 10)
	ctx, cancel := context.WithCancel(t.Context())

	go debouncer.Run(ctx, events)
	senderDone := startSenderLoop(ctx, restClient, debouncer)

	// Send events that would normally be stuck in debounce
	events <- controller.ResourceEvent{
		Type:     controller.EventAdd,
		Instance: makeInstance("default/apps/v1/Deployment/drain-test", "default", "drain-test", "Deployment"),
	}
	events <- controller.ResourceEvent{
		Type:     controller.EventAdd,
		Instance: makeInstance("prod/v1/Service/drain-svc", "prod", "drain-svc", "Service"),
	}

	// Give events time to be ingested by the debouncer
	time.Sleep(50 * time.Millisecond)

	// Cancel context — simulating graceful shutdown.
	// The debouncer's context cancellation path calls flushAllPending() and closes Payloads.
	cancel()

	// Wait for the sender to finish draining
	select {
	case <-senderDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Sender did not drain within timeout after context cancellation")
	}

	// The HTTP server should have received the pending payloads
	upserts := collector.allUpserts()
	if len(upserts) != 2 {
		t.Fatalf("Expected 2 upserts drained during shutdown, got %d", len(upserts))
	}

	upsertIDs := make(map[string]bool)
	for _, u := range upserts {
		upsertIDs[u.ID] = true
	}
	if !upsertIDs["default/apps/v1/Deployment/drain-test"] {
		t.Error("Missing drained upsert for default/apps/v1/Deployment/drain-test")
	}
	if !upsertIDs["prod/v1/Service/drain-svc"] {
		t.Error("Missing drained upsert for prod/v1/Service/drain-svc")
	}
}

// labelForIteration returns a version label string for the given 0-based iteration.
func labelForIteration(i int) string {
	return "v" + strconv.Itoa(i+1)
}
