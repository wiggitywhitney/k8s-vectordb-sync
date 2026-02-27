package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/wiggitywhitney/k8s-vectordb-sync/internal/controller"
	"github.com/wiggitywhitney/k8s-vectordb-sync/internal/metadata"
)

func TestRESTClient_PostsPayloadAsJSON(t *testing.T) {
	var received controller.SyncPayload
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("Expected POST, got %s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("Failed to read body: %v", err)
		}
		if err := json.Unmarshal(body, &received); err != nil {
			t.Fatalf("Failed to unmarshal body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := New(logr.Discard(), server.URL, WithTimeout(5*time.Second))

	payload := controller.SyncPayload{
		Upserts: []metadata.ResourceInstance{
			{
				ID:         "default/apps/v1/Deployment/nginx",
				Namespace:  "default",
				Name:       "nginx",
				Kind:       "Deployment",
				APIVersion: "apps/v1",
				APIGroup:   "apps",
				Labels:     map[string]string{"app": "nginx"},
			},
		},
		Deletes: []string{"default/apps/v1/Deployment/old-app"},
	}

	err := client.Send(context.Background(), payload)
	if err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	if len(received.Upserts) != 1 {
		t.Fatalf("Expected 1 upsert, got %d", len(received.Upserts))
	}
	if received.Upserts[0].ID != "default/apps/v1/Deployment/nginx" {
		t.Errorf("Upsert ID = %q, want nginx deployment", received.Upserts[0].ID)
	}
	if len(received.Deletes) != 1 || received.Deletes[0] != "default/apps/v1/Deployment/old-app" {
		t.Errorf("Deletes = %v, want [old-app]", received.Deletes)
	}
}

func TestRESTClient_RetriesOnServerError(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempt := attempts.Add(1)
		if attempt <= 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := New(logr.Discard(), server.URL,
		WithTimeout(5*time.Second),
		WithRetry(3, 10*time.Millisecond, 50*time.Millisecond),
	)

	payload := controller.SyncPayload{
		Upserts: []metadata.ResourceInstance{
			{ID: "default/v1/ConfigMap/test", Name: "test", Kind: "ConfigMap"},
		},
	}

	err := client.Send(context.Background(), payload)
	if err != nil {
		t.Fatalf("Send should have succeeded after retries: %v", err)
	}

	if got := attempts.Load(); got != 3 {
		t.Errorf("Expected 3 attempts (2 failures + 1 success), got %d", got)
	}
}

func TestRESTClient_ExhaustsRetriesAndFails(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	client := New(logr.Discard(), server.URL,
		WithTimeout(5*time.Second),
		WithRetry(3, 10*time.Millisecond, 50*time.Millisecond),
	)

	payload := controller.SyncPayload{
		Upserts: []metadata.ResourceInstance{
			{ID: "default/v1/ConfigMap/test", Name: "test", Kind: "ConfigMap"},
		},
	}

	err := client.Send(context.Background(), payload)
	if err == nil {
		t.Fatal("Expected error after exhausting retries")
	}

	// 1 initial + 3 retries = 4 total attempts
	if got := attempts.Load(); got != 4 {
		t.Errorf("Expected 4 total attempts, got %d", got)
	}
}

func TestRESTClient_RespectsContextCancellation(t *testing.T) {
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-done:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() {
		close(done)    // Unblock the handler first
		server.Close() // Then close the server
	})

	client := New(logr.Discard(), server.URL, WithTimeout(10*time.Second))

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	payload := controller.SyncPayload{
		Upserts: []metadata.ResourceInstance{
			{ID: "default/v1/ConfigMap/test", Name: "test", Kind: "ConfigMap"},
		},
	}

	err := client.Send(ctx, payload)
	if err == nil {
		t.Fatal("Expected error from cancelled context")
	}
}

func TestRESTClient_DoesNotRetryOn4xx(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()

	client := New(logr.Discard(), server.URL,
		WithTimeout(5*time.Second),
		WithRetry(3, 10*time.Millisecond, 50*time.Millisecond),
	)

	payload := controller.SyncPayload{
		Upserts: []metadata.ResourceInstance{
			{ID: "default/v1/ConfigMap/test", Name: "test", Kind: "ConfigMap"},
		},
	}

	err := client.Send(context.Background(), payload)
	if err == nil {
		t.Fatal("Expected error from 400 response")
	}

	// Should NOT retry on 4xx — only 1 attempt
	if got := attempts.Load(); got != 1 {
		t.Errorf("Expected 1 attempt (no retry on 4xx), got %d", got)
	}
}

func TestRESTClient_HandlesConnectionRefused(t *testing.T) {
	// Use a URL that will refuse connections
	client := New(logr.Discard(), "http://127.0.0.1:1",
		WithTimeout(1*time.Second),
		WithRetry(2, 10*time.Millisecond, 50*time.Millisecond),
	)

	payload := controller.SyncPayload{
		Upserts: []metadata.ResourceInstance{
			{ID: "default/v1/ConfigMap/test", Name: "test", Kind: "ConfigMap"},
		},
	}

	err := client.Send(context.Background(), payload)
	if err == nil {
		t.Fatal("Expected error from connection refused")
	}
}

func TestRESTClient_SkipsEmptyPayload(t *testing.T) {
	var called atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := New(logr.Discard(), server.URL, WithTimeout(5*time.Second))

	// Empty payload — no upserts or deletes
	err := client.Send(context.Background(), controller.SyncPayload{})
	if err != nil {
		t.Fatalf("Send should not error on empty payload: %v", err)
	}

	if called.Load() {
		t.Error("Should not have made HTTP request for empty payload")
	}
}

func TestRESTClient_RetriesOnTimeout(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempt := attempts.Add(1)
		if attempt <= 1 {
			// First attempt times out
			time.Sleep(200 * time.Millisecond)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := New(logr.Discard(), server.URL,
		WithTimeout(50*time.Millisecond),
		WithRetry(2, 10*time.Millisecond, 50*time.Millisecond),
	)

	payload := controller.SyncPayload{
		Upserts: []metadata.ResourceInstance{
			{ID: "default/v1/ConfigMap/test", Name: "test", Kind: "ConfigMap"},
		},
	}

	err := client.Send(context.Background(), payload)
	if err != nil {
		t.Fatalf("Send should have succeeded after retry: %v", err)
	}

	if got := attempts.Load(); got != 2 {
		t.Errorf("Expected 2 attempts, got %d", got)
	}
}

func TestRESTClient_SendsCrdSyncPayload(t *testing.T) {
	var received controller.CrdSyncPayload
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("Failed to read body: %v", err)
		}
		if err := json.Unmarshal(body, &received); err != nil {
			t.Fatalf("Failed to unmarshal body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := New(logr.Discard(), server.URL, WithTimeout(5*time.Second))

	payload := controller.CrdSyncPayload{
		Upserts: []string{"certificates.cert-manager.io", "issuers.cert-manager.io"},
		Deletes: []string{"challenges.cert-manager.io"},
	}

	err := client.Send(context.Background(), payload)
	if err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	if len(received.Upserts) != 2 {
		t.Fatalf("Expected 2 upserts, got %d", len(received.Upserts))
	}
	if received.Upserts[0] != "certificates.cert-manager.io" {
		t.Errorf("Upserts[0] = %q, want certificates.cert-manager.io", received.Upserts[0])
	}
	if len(received.Deletes) != 1 || received.Deletes[0] != "challenges.cert-manager.io" {
		t.Errorf("Deletes = %v, want [challenges.cert-manager.io]", received.Deletes)
	}
}

func TestRESTClient_SkipsNilPayload(t *testing.T) {
	var called atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := New(logr.Discard(), server.URL, WithTimeout(5*time.Second))

	err := client.Send(context.Background(), nil)
	if err != nil {
		t.Fatalf("Send should not error on nil payload: %v", err)
	}

	if called.Load() {
		t.Error("Should not have made HTTP request for nil payload")
	}
}

func TestRESTClient_SkipsEmptyCrdPayload(t *testing.T) {
	var called atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := New(logr.Discard(), server.URL, WithTimeout(5*time.Second))

	// Empty CRD payload — no added or deleted
	err := client.Send(context.Background(), controller.CrdSyncPayload{})
	if err != nil {
		t.Fatalf("Send should not error on empty CRD payload: %v", err)
	}

	if called.Load() {
		t.Error("Should not have made HTTP request for empty CRD payload")
	}
}
