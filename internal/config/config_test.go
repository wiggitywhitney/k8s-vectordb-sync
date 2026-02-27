package config

import (
	"testing"
	"time"
)

func TestLoad_Defaults(t *testing.T) {
	// Clear any env vars that might be set
	for _, key := range []string{
		"INSTANCES_ENDPOINT", "CAPABILITIES_ENDPOINT",
		"DEBOUNCE_WINDOW_MS", "BATCH_FLUSH_INTERVAL_MS",
		"BATCH_MAX_SIZE", "RESYNC_INTERVAL_MIN", "WATCH_RESOURCE_TYPES",
		"EXCLUDE_RESOURCE_TYPES", "LOG_LEVEL",
	} {
		t.Setenv(key, "")
	}

	cfg := Load()

	if cfg.InstancesEndpoint != "http://localhost:3000/api/v1/instances/sync" {
		t.Errorf("InstancesEndpoint = %q, want default", cfg.InstancesEndpoint)
	}
	if cfg.CapabilitiesEndpoint != "" {
		t.Errorf("CapabilitiesEndpoint = %q, want empty (disabled by default)", cfg.CapabilitiesEndpoint)
	}
	if cfg.DebounceWindow != 10*time.Second {
		t.Errorf("DebounceWindow = %v, want 10s", cfg.DebounceWindow)
	}
	if cfg.BatchFlushInterval != 5*time.Second {
		t.Errorf("BatchFlushInterval = %v, want 5s", cfg.BatchFlushInterval)
	}
	if cfg.BatchMaxSize != 50 {
		t.Errorf("BatchMaxSize = %d, want 50", cfg.BatchMaxSize)
	}
	if cfg.ResyncInterval != 1440*time.Minute {
		t.Errorf("ResyncInterval = %v, want 1440m (24h)", cfg.ResyncInterval)
	}
	if len(cfg.WatchResourceTypes) != 0 {
		t.Errorf("WatchResourceTypes = %v, want empty", cfg.WatchResourceTypes)
	}
	expectedExclusions := map[string]bool{
		"events":                    true,
		"leases":                    true,
		"endpointslices":            true,
		"componentstatuses":         true,
		"customresourcedefinitions": true,
	}
	if len(cfg.ExcludeResourceTypes) != len(expectedExclusions) {
		t.Fatalf("ExcludeResourceTypes = %v, want %d defaults", cfg.ExcludeResourceTypes, len(expectedExclusions))
	}
	for _, rt := range cfg.ExcludeResourceTypes {
		if !expectedExclusions[rt] {
			t.Errorf("Unexpected default excluded resource type: %q", rt)
		}
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, "info")
	}
}

func TestLoad_CustomValues(t *testing.T) {
	t.Setenv("INSTANCES_ENDPOINT", "http://custom:8080/sync")
	t.Setenv("CAPABILITIES_ENDPOINT", "http://custom:8080/capabilities/scan")
	t.Setenv("DEBOUNCE_WINDOW_MS", "5000")
	t.Setenv("BATCH_FLUSH_INTERVAL_MS", "2000")
	t.Setenv("BATCH_MAX_SIZE", "100")
	t.Setenv("RESYNC_INTERVAL_MIN", "30")
	t.Setenv("WATCH_RESOURCE_TYPES", "deployments,services,statefulsets")
	t.Setenv("EXCLUDE_RESOURCE_TYPES", "events,pods")
	t.Setenv("LOG_LEVEL", "debug")

	cfg := Load()

	if cfg.InstancesEndpoint != "http://custom:8080/sync" {
		t.Errorf("InstancesEndpoint = %q, want custom", cfg.InstancesEndpoint)
	}
	if cfg.CapabilitiesEndpoint != "http://custom:8080/capabilities/scan" {
		t.Errorf("CapabilitiesEndpoint = %q, want custom", cfg.CapabilitiesEndpoint)
	}
	if cfg.DebounceWindow != 5*time.Second {
		t.Errorf("DebounceWindow = %v, want 5s", cfg.DebounceWindow)
	}
	if cfg.BatchFlushInterval != 2*time.Second {
		t.Errorf("BatchFlushInterval = %v, want 2s", cfg.BatchFlushInterval)
	}
	if cfg.BatchMaxSize != 100 {
		t.Errorf("BatchMaxSize = %d, want 100", cfg.BatchMaxSize)
	}
	if cfg.ResyncInterval != 30*time.Minute {
		t.Errorf("ResyncInterval = %v, want 30m", cfg.ResyncInterval)
	}
	if len(cfg.WatchResourceTypes) != 3 {
		t.Errorf("WatchResourceTypes = %v, want 3 items", cfg.WatchResourceTypes)
	}
	if cfg.WatchResourceTypes[0] != "deployments" {
		t.Errorf("WatchResourceTypes[0] = %q, want deployments", cfg.WatchResourceTypes[0])
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want debug", cfg.LogLevel)
	}
}

func TestEnvCSVOrDefault_TrimsWhitespace(t *testing.T) {
	t.Setenv("TEST_CSV", " deployments , services , statefulsets ")

	result := envCSVOrDefault("TEST_CSV", nil)

	if len(result) != 3 {
		t.Fatalf("Expected 3 items, got %d: %v", len(result), result)
	}
	for _, item := range result {
		if item != "deployments" && item != "services" && item != "statefulsets" {
			t.Errorf("Unexpected item %q — should be trimmed and lowercased", item)
		}
	}
}

func TestEnvCSVOrDefault_EmptyString(t *testing.T) {
	t.Setenv("TEST_CSV", "")

	result := envCSVOrDefault("TEST_CSV", []string{"default"})

	if len(result) != 1 || result[0] != "default" {
		t.Errorf("Expected default value, got %v", result)
	}
}

func TestEnvIntOrDefault_InvalidNumber(t *testing.T) {
	t.Setenv("TEST_INT", "not-a-number")

	result := envIntOrDefault("TEST_INT", 42)

	if result != 42 {
		t.Errorf("Expected default 42 for invalid input, got %d", result)
	}
}
