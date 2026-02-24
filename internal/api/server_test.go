package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-logr/logr"
)

// fakeResyncer implements Resyncer for testing.
type fakeResyncer struct {
	count int
	err   error
}

func (f *fakeResyncer) TriggerResync(_ context.Context) (int, error) {
	return f.count, f.err
}

func TestHandleResync_Success(t *testing.T) {
	s := NewServer(logr.Discard(), ":0", &fakeResyncer{count: 42})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/resync", nil)
	rec := httptest.NewRecorder()

	s.server.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}

	var resp resyncResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if resp.Status != "ok" {
		t.Errorf("Status = %q, want ok", resp.Status)
	}
	if resp.Resources != 42 {
		t.Errorf("Resources = %d, want 42", resp.Resources)
	}
}

func TestHandleResync_Error(t *testing.T) {
	s := NewServer(logr.Discard(), ":0", &fakeResyncer{err: fmt.Errorf("connection refused")})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/resync", nil)
	rec := httptest.NewRecorder()

	s.server.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("Expected 500, got %d", rec.Code)
	}

	var resp resyncResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if resp.Status != "error" {
		t.Errorf("Status = %q, want error", resp.Status)
	}
}

func TestHandleResync_MethodNotAllowed(t *testing.T) {
	s := NewServer(logr.Discard(), ":0", &fakeResyncer{count: 0})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/resync", nil)
	rec := httptest.NewRecorder()

	s.server.Handler.ServeHTTP(rec, req)

	// Go 1.22+ ServeMux returns 405 for method mismatch
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("Expected 405 for GET, got %d", rec.Code)
	}
}

func TestHandleResync_ZeroResources(t *testing.T) {
	s := NewServer(logr.Discard(), ":0", &fakeResyncer{count: 0})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/resync", nil)
	rec := httptest.NewRecorder()

	s.server.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}

	var resp resyncResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if resp.Resources != 0 {
		t.Errorf("Resources = %d, want 0", resp.Resources)
	}
	if resp.Status != "ok" {
		t.Errorf("Status = %q, want ok", resp.Status)
	}
}

func TestHandleResync_ContentType(t *testing.T) {
	s := NewServer(logr.Discard(), ":0", &fakeResyncer{count: 5})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/resync", nil)
	rec := httptest.NewRecorder()

	s.server.Handler.ServeHTTP(rec, req)

	ct := rec.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}
