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
}

// pendingChange tracks a debounced resource change with its timer.
type pendingChange struct {
	instance metadata.ResourceInstance
	timer    *time.Timer
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
				d.flushPending()
				close(d.Payloads)
				return
			}
			d.handleEvent(event)

		case <-flushTicker.C:
			d.flushPending()

		case <-ctx.Done():
			// Context cancelled — flush any pending changes and exit.
			// Don't try to drain the events channel as it may never close.
			d.flushAllPending()
			close(d.Payloads)
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

	if existing, ok := d.pending[event.Instance.ID]; ok {
		// Reset the existing timer and update the state (last-state-wins)
		existing.timer.Stop()
	}

	d.pending[event.Instance.ID] = pendingChange{
		instance: event.Instance,
		timer:    time.AfterFunc(d.debounceWindow, func() { d.promoteToReady() }),
	}
}

// promoteToReady is called when a debounce timer fires. It marks the change
// as ready for the next flush cycle by leaving it in the pending map
// (the timer has fired, so it won't be reset unless a new event arrives).
// The actual flush happens on the flush ticker or when batch size is reached.
func (d *DebounceBuffer) promoteToReady() {
	d.pendingMu.Lock()
	size := d.pendingCount()
	d.pendingMu.Unlock()

	if size >= d.maxBatchSize {
		d.flushPending()
	}
}

// flushPending collects all pending changes whose timers have fired
// and emits them as a single SyncPayload.
func (d *DebounceBuffer) flushPending() {
	d.pendingMu.Lock()

	if len(d.pending) == 0 {
		d.pendingMu.Unlock()
		return
	}

	var upserts []metadata.ResourceInstance
	var stillPending []string

	for id, change := range d.pending {
		// Check if the timer has fired by trying to stop it.
		// If Stop returns false, the timer already fired — the change is ready.
		if !change.timer.Stop() {
			upserts = append(upserts, change.instance)
			delete(d.pending, id)
		} else {
			// Timer was still running — re-arm it (Stop consumed it)
			stillPending = append(stillPending, id)
			d.pending[id] = pendingChange{
				instance: change.instance,
				timer:    time.AfterFunc(d.debounceWindow, func() { d.promoteToReady() }),
			}
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
		"stillPending", len(stillPending),
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

// emitPayload sends a SyncPayload to the payloads channel without blocking.
func (d *DebounceBuffer) emitPayload(payload SyncPayload) {
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
