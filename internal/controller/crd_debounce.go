package controller

import (
	"context"
	"sync"
	"time"

	"github.com/go-logr/logr"

	"github.com/wiggitywhitney/k8s-vectordb-sync/internal/config"
)

// CrdSyncPayload is the batched payload for CRD events sent to the capabilities endpoint.
// It contains CRD fully-qualified names (e.g., "certificates.cert-manager.io").
type CrdSyncPayload struct {
	Added   []string `json:"added,omitempty"`
	Deleted []string `json:"deleted,omitempty"`
}

// IsEmpty reports whether the payload contains no CRD events.
func (p CrdSyncPayload) IsEmpty() bool {
	return len(p.Added) == 0 && len(p.Deleted) == 0
}

// CrdDebounceBuffer accumulates CrdEvents from the watcher, deduplicates
// them per CRD name (last-state-wins), and flushes batched payloads on
// a configurable interval or when the batch size threshold is reached.
//
// Delete events bypass the debounce window and are flushed immediately
// to avoid stale capabilities in search results.
type CrdDebounceBuffer struct {
	log            logr.Logger
	debounceWindow time.Duration
	flushInterval  time.Duration
	maxBatchSize   int

	// pending holds debounced CRD add events keyed by CRD name.
	pending   map[string]pendingCrdChange
	pendingMu sync.Mutex

	// Payloads channel for downstream consumers (REST client)
	Payloads chan CrdSyncPayload

	// payloadsMu guards payloadsClosed to prevent send-on-closed-channel panics
	payloadsMu     sync.Mutex
	payloadsClosed bool
}

// pendingCrdChange tracks a debounced CRD add event with its timer.
type pendingCrdChange struct {
	crdName string
	timer   *time.Timer
	ready   bool
	gen     uint64 // generation counter to prevent stale timer goroutines
}

// NewCrdDebounceBuffer creates a CrdDebounceBuffer from the controller configuration.
func NewCrdDebounceBuffer(log logr.Logger, cfg config.Config) *CrdDebounceBuffer {
	return &CrdDebounceBuffer{
		log:            log,
		debounceWindow: cfg.DebounceWindow,
		flushInterval:  cfg.BatchFlushInterval,
		maxBatchSize:   cfg.BatchMaxSize,
		pending:        make(map[string]pendingCrdChange),
		Payloads:       make(chan CrdSyncPayload, 100),
	}
}

// Run reads CRD events from the watcher and manages debouncing and batching.
// It blocks until the context is cancelled, then flushes any remaining changes.
func (d *CrdDebounceBuffer) Run(ctx context.Context, events <-chan CrdEvent) {
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
			// Context cancelled — flush any pending changes and exit
			d.flushAllPending()
			d.closePayloads()
			return
		}
	}
}

// handleEvent processes a single CrdEvent. Deletes are forwarded
// immediately; adds are debounced per CRD name.
func (d *CrdDebounceBuffer) handleEvent(event CrdEvent) {
	if event.Type == EventDelete {
		// Deletes skip debounce — forward immediately
		d.pendingMu.Lock()
		// Cancel any pending add for this CRD
		if existing, ok := d.pending[event.CrdName]; ok {
			existing.timer.Stop()
			delete(d.pending, event.CrdName)
		}
		d.pendingMu.Unlock()

		payload := CrdSyncPayload{
			Deleted: []string{event.CrdName},
		}
		d.emitPayload(payload)
		d.log.V(1).Info("CRD delete forwarded immediately", "crd", event.CrdName)
		return
	}

	// Add — debounce per CRD name
	d.pendingMu.Lock()
	defer d.pendingMu.Unlock()

	var nextGen uint64
	if existing, ok := d.pending[event.CrdName]; ok {
		// Reset the existing timer (last-state-wins / dedup)
		existing.timer.Stop()
		nextGen = existing.gen + 1
	}

	crdName := event.CrdName
	capturedGen := nextGen
	d.pending[crdName] = pendingCrdChange{
		crdName: crdName,
		gen:     capturedGen,
		timer: time.AfterFunc(d.debounceWindow, func() {
			d.pendingMu.Lock()
			if p, ok := d.pending[crdName]; ok && p.gen == capturedGen {
				p.ready = true
				d.pending[crdName] = p
			}
			d.pendingMu.Unlock()
			d.checkFlush()
		}),
	}
}

// checkFlush is called when a debounce timer fires. It checks if enough
// ready entries have accumulated to trigger an early batch flush.
func (d *CrdDebounceBuffer) checkFlush() {
	d.pendingMu.Lock()
	size := d.readyCount()
	d.pendingMu.Unlock()

	if size >= d.maxBatchSize {
		d.flushPending()
	}
}

// readyCount returns the number of pending changes whose debounce timers
// have fired (must hold pendingMu).
func (d *CrdDebounceBuffer) readyCount() int {
	count := 0
	for _, change := range d.pending {
		if change.ready {
			count++
		}
	}
	return count
}

// flushPending collects all pending CRD adds whose debounce timers have fired
// (ready == true) and emits them as a single CrdSyncPayload.
func (d *CrdDebounceBuffer) flushPending() {
	d.pendingMu.Lock()

	if len(d.pending) == 0 {
		d.pendingMu.Unlock()
		return
	}

	var added []string

	for name, change := range d.pending {
		if change.ready {
			added = append(added, change.crdName)
			delete(d.pending, name)
		}
	}

	d.pendingMu.Unlock()

	if len(added) == 0 {
		return
	}

	payload := CrdSyncPayload{
		Added: added,
	}
	d.emitPayload(payload)
	d.log.Info("CRD batch flushed", "added", len(added))
}

// flushAllPending forces a flush of all pending CRD adds regardless of
// whether their debounce timers have fired. Used during shutdown.
func (d *CrdDebounceBuffer) flushAllPending() {
	d.pendingMu.Lock()

	if len(d.pending) == 0 {
		d.pendingMu.Unlock()
		return
	}

	var added []string
	for name, change := range d.pending {
		change.timer.Stop()
		added = append(added, change.crdName)
		delete(d.pending, name)
	}

	d.pendingMu.Unlock()

	if len(added) > 0 {
		d.emitPayload(CrdSyncPayload{Added: added})
		d.log.Info("CRD final flush on shutdown", "added", len(added))
	}
}

// closePayloads closes the Payloads channel exactly once, guarded by payloadsMu
// to prevent send-on-closed-channel panics from concurrent timer goroutines.
func (d *CrdDebounceBuffer) closePayloads() {
	d.payloadsMu.Lock()
	defer d.payloadsMu.Unlock()
	if !d.payloadsClosed {
		d.payloadsClosed = true
		close(d.Payloads)
	}
}

// emitPayload sends a CrdSyncPayload to the payloads channel without blocking.
// Guards against send-on-closed-channel from timer goroutines racing with shutdown.
func (d *CrdDebounceBuffer) emitPayload(payload CrdSyncPayload) {
	d.payloadsMu.Lock()
	defer d.payloadsMu.Unlock()
	if d.payloadsClosed {
		return
	}
	select {
	case d.Payloads <- payload:
	default:
		d.log.Info("CRD payload channel full, dropping batch",
			"added", len(payload.Added),
			"deleted", len(payload.Deleted),
		)
	}
}

// PendingCountForTesting returns the number of pending CRD changes (for testing).
func (d *CrdDebounceBuffer) PendingCountForTesting() int {
	d.pendingMu.Lock()
	defer d.pendingMu.Unlock()
	return len(d.pending)
}
