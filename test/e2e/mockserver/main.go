package main

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"
)

var (
	mu       sync.Mutex
	payloads []json.RawMessage

	crdMu       sync.Mutex
	crdPayloads []json.RawMessage
)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/instances/sync", handleSync)
	mux.HandleFunc("GET /payloads", handleGetPayloads)
	mux.HandleFunc("DELETE /payloads", handleClearPayloads)
	mux.HandleFunc("POST /api/v1/capabilities/scan", handleCapabilitiesScan)
	mux.HandleFunc("GET /crd-payloads", handleGetCrdPayloads)
	mux.HandleFunc("DELETE /crd-payloads", handleClearCrdPayloads)
	mux.HandleFunc("GET /healthz", handleHealthz)

	log.Println("Mock server listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", mux))
}

func handleSync(w http.ResponseWriter, r *http.Request) {
	var raw json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	mu.Lock()
	payloads = append(payloads, raw)
	mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func handleGetPayloads(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	defer mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(payloads)
}

func handleClearPayloads(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	payloads = nil
	mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func handleCapabilitiesScan(w http.ResponseWriter, r *http.Request) {
	var raw json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	crdMu.Lock()
	crdPayloads = append(crdPayloads, raw)
	crdMu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func handleGetCrdPayloads(w http.ResponseWriter, r *http.Request) {
	crdMu.Lock()
	defer crdMu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(crdPayloads)
}

func handleClearCrdPayloads(w http.ResponseWriter, r *http.Request) {
	crdMu.Lock()
	crdPayloads = nil
	crdMu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}
