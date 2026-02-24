package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/go-logr/logr"
)

// Resyncer is the interface for triggering ad-hoc resyncs.
type Resyncer interface {
	TriggerResync(ctx context.Context) (int, error)
}

// Server exposes operational HTTP endpoints for the controller, including
// an ad-hoc resync trigger.
type Server struct {
	log         logr.Logger
	bindAddress string
	resyncer    Resyncer
	server      *http.Server
}

// NewServer creates an API server that binds to the given address.
func NewServer(log logr.Logger, bindAddress string, resyncer Resyncer) *Server {
	s := &Server{
		log:         log,
		bindAddress: bindAddress,
		resyncer:    resyncer,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/resync", s.handleResync)

	s.server = &http.Server{
		Addr:              bindAddress,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	return s
}

// Start implements manager.Runnable. It starts the HTTP server and blocks
// until the context is cancelled, then shuts down gracefully.
func (s *Server) Start(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.bindAddress)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", s.bindAddress, err)
	}

	s.log.Info("API server started", "address", ln.Addr().String())

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.server.Shutdown(shutdownCtx)
	}()

	if err := s.server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serving: %w", err)
	}
	return nil
}

// resyncResponse is the JSON response from the resync endpoint.
type resyncResponse struct {
	Status    string `json:"status"`
	Resources int    `json:"resources"`
	Message   string `json:"message"`
}

func (s *Server) handleResync(w http.ResponseWriter, r *http.Request) {
	s.log.Info("Ad-hoc resync triggered via API")

	count, err := s.resyncer.TriggerResync(r.Context())
	if err != nil {
		s.log.Error(err, "Resync failed")
		writeJSON(w, http.StatusInternalServerError, resyncResponse{
			Status:  "error",
			Message: fmt.Sprintf("resync failed: %v", err),
		})
		return
	}

	writeJSON(w, http.StatusOK, resyncResponse{
		Status:    "ok",
		Resources: count,
		Message:   fmt.Sprintf("resynced %d resources", count),
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
