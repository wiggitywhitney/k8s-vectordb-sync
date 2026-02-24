package controller

import (
	"context"
	"sync"
	"time"

	"github.com/go-logr/logr"

	"github.com/wiggitywhitney/k8s-vectordb-sync/internal/config"
	"github.com/wiggitywhitney/k8s-vectordb-sync/internal/metadata"
)

// SyncPayload is the batched payload ready to be sent to the REST API.
// It separates upserts (full resource metadata) from deletes (just IDs).
type SyncPayload struct {
	Upserts []metadata.ResourceInstance `json:"upserts"`
	Deletes []string                    `json:"deletes"`
}

// DebounceBuffer accumulates ResourceEvents from the watcher, deduplicates
// them per resource ID (last-state-wins), and flushes batched payloads on
// a configurable interval or when the batch size threshold is reached.
//
// Delete events bypass the debounce window and are flushed immediately
// to avoid stale data in search results (per PRD design decision).
type DebounceBuffer struct {
	log            logr.Logger
	debounceWindow time.Duration
	flushInterval  time.Duration
	maxBatchSize   int

	// pending holds debounced upsert events keyed by resource ID.
	// Last-state-wins: newer events overwrite older ones for the same ID.
	pending   map[string]pendingChange
	pendingMu sync.Mutex

	// Payloads channel for downstream consumers (REST client)
	Payloads chan SyncPayload

	// payloadsMu guards payloadsClosed to prevent send-on-closed-channel panics
	// from timer goroutines that race with Run() closing the Payloads channel.
	payloadsMu     sync.Mutex
	payloadsClosed bool
}

// pendingChange tracks a debounced resource change with its timer.
type pendingChange struct {
	instance metadata.ResourceInstance
	timer    *time.Timer
	ready    bool
	gen      uint64 // generation counter to prevent stale timer goroutines from marking replacement entries ready
}

// NewDebounceBuffer creates a DebounceBuffer from the controller configuration.
func NewDebounceBuffer(log logr.Logger, cfg config.Config) *DebounceBuffer {
	return &DebounceBuffer{
		log:            log,
		debounceWindow: cfg.DebounceWindow,
		flushInterval:  cfg.BatchFlushInterval,
		maxBatchSize:   cfg.BatchMaxSize,
		pending:        make(map[string]pendingChange),
		Payloads:       make(chan SyncPayload, 100),
	}
}

// Run reads events from the watcher and manages debouncing and batching.
// It blocks until the context is cancelled, then flushes any remaining changes.
func (d *DebounceBuffer) Run(ctx context.Context, events <-chan ResourceEvent) {
	flushTicker := time.NewTicker(d.flushInterval)
	defer flushTicker.Stop()

	for {
		select {
		case event, ok := <-events:
			if !ok {
				// Channel closed — flush remaining and exit
				d.flushAllPending()
				d.closePayloads()
				return
			}
			d.handleEvent(event)

		case <-flushTicker.C:
			d.flushPending()

		case <-ctx.Done():
			// Context cancelled — flush any pending changes and exit.
			// Don't try to drain the events channel as it may never close.
			d.flushAllPending()
			d.closePayloads()
			return
		}
	}
}

// handleEvent processes a single ResourceEvent. Deletes are forwarded
// immediately; upserts (add/update) are debounced per resource ID.
func (d *DebounceBuffer) handleEvent(event ResourceEvent) {
	if event.Type == EventDelete {
		// Deletes skip debounce — forward immediately
		d.pendingMu.Lock()
		// Cancel any pending upsert for this resource
		if existing, ok := d.pending[event.Instance.ID]; ok {
			existing.timer.Stop()
			delete(d.pending, event.Instance.ID)
		}
		d.pendingMu.Unlock()

		payload := SyncPayload{
			Deletes: []string{event.Instance.ID},
		}
		d.emitPayload(payload)
		d.log.V(1).Info("Delete forwarded immediately", "id", event.Instance.ID)
		return
	}

	// Upsert (add or update) — debounce per resource ID
	d.pendingMu.Lock()
	defer d.pendingMu.Unlock()

	var nextGen uint64
	if existing, ok := d.pending[event.Instance.ID]; ok {
		// Reset the existing timer and update the state (last-state-wins)
		existing.timer.Stop()
		nextGen = existing.gen + 1
	}

	id := event.Instance.ID
	capturedGen := nextGen
	d.pending[id] = pendingChange{
		instance: event.Instance,
		gen:      capturedGen,
		timer: time.AfterFunc(d.debounceWindow, func() {
			d.pendingMu.Lock()
			if p, ok := d.pending[id]; ok && p.gen == capturedGen {
				p.ready = true
				d.pending[id] = p
			}
			d.pendingMu.Unlock()
			d.checkFlush()
		}),
	}
}

// checkFlush is called when a debounce timer fires. It checks if enough
// ready entries have accumulated to trigger an early batch flush.
func (d *DebounceBuffer) checkFlush() {
	d.pendingMu.Lock()
	size := d.readyCount()
	d.pendingMu.Unlock()

	if size >= d.maxBatchSize {
		d.flushPending()
	}
}

// readyCount returns the number of pending changes whose debounce timers
// have fired (must hold pendingMu).
func (d *DebounceBuffer) readyCount() int {
	count := 0
	for _, change := range d.pending {
		if change.ready {
			count++
		}
	}
	return count
}

// flushPending collects all pending changes whose debounce timers have fired
// (ready == true) and emits them as a single SyncPayload.
func (d *DebounceBuffer) flushPending() {
	d.pendingMu.Lock()

	if len(d.pending) == 0 {
		d.pendingMu.Unlock()
		return
	}

	var upserts []metadata.ResourceInstance

	for id, change := range d.pending {
		if change.ready {
			upserts = append(upserts, change.instance)
			delete(d.pending, id)
		}
	}

	d.pendingMu.Unlock()

	if len(upserts) == 0 {
		return
	}

	payload := SyncPayload{
		Upserts: upserts,
	}
	d.emitPayload(payload)
	d.log.Info("Batch flushed",
		"upserts", len(upserts),
	)
}

// flushAllPending forces a flush of all pending changes regardless of
// whether their debounce timers have fired. Used during shutdown.
func (d *DebounceBuffer) flushAllPending() {
	d.pendingMu.Lock()

	if len(d.pending) == 0 {
		d.pendingMu.Unlock()
		return
	}

	var upserts []metadata.ResourceInstance
	for id, change := range d.pending {
		change.timer.Stop()
		upserts = append(upserts, change.instance)
		delete(d.pending, id)
	}

	d.pendingMu.Unlock()

	if len(upserts) > 0 {
		d.emitPayload(SyncPayload{Upserts: upserts})
		d.log.Info("Final flush on shutdown", "upserts", len(upserts))
	}
}

// pendingCount returns the number of pending changes (must hold pendingMu).
func (d *DebounceBuffer) pendingCount() int {
	return len(d.pending)
}

// closePayloads closes the Payloads channel exactly once, guarded by payloadsMu
// to prevent send-on-closed-channel panics from concurrent timer goroutines.
func (d *DebounceBuffer) closePayloads() {
	d.payloadsMu.Lock()
	defer d.payloadsMu.Unlock()
	if !d.payloadsClosed {
		d.payloadsClosed = true
		close(d.Payloads)
	}
}

// emitPayload sends a SyncPayload to the payloads channel without blocking.
// Guards against send-on-closed-channel from timer goroutines racing with shutdown.
func (d *DebounceBuffer) emitPayload(payload SyncPayload) {
	d.payloadsMu.Lock()
	defer d.payloadsMu.Unlock()
	if d.payloadsClosed {
		return
	}
	select {
	case d.Payloads <- payload:
	default:
		d.log.Info("Payload channel full, dropping batch",
			"upserts", len(payload.Upserts),
			"deletes", len(payload.Deletes),
		)
	}
}

// PendingCountForTesting returns the number of pending changes (for testing).
func (d *DebounceBuffer) PendingCountForTesting() int {
	d.pendingMu.Lock()
	defer d.pendingMu.Unlock()
	return d.pendingCount()
}
