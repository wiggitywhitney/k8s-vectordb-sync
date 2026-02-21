package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all controller configuration loaded from environment variables.
type Config struct {
	// REST endpoint URL for cluster-whisperer
	RESTEndpoint string

	// Debounce window per resource before flushing changes
	DebounceWindow time.Duration

	// Maximum time before flushing a batch
	BatchFlushInterval time.Duration

	// Maximum batch size before flush
	BatchMaxSize int

	// Full resync interval for eventual consistency
	ResyncInterval time.Duration

	// Resource types to watch (empty = all discoverable types)
	WatchResourceTypes []string

	// Resource types to exclude from watching
	ExcludeResourceTypes []string

	// Logging verbosity level
	LogLevel string
}

// Load reads configuration from environment variables with defaults per the PRD.
func Load() Config {
	return Config{
		RESTEndpoint:         envOrDefault("REST_ENDPOINT", "http://localhost:3000/api/v1/instances/sync"),
		DebounceWindow:       envDurationMsOrDefault("DEBOUNCE_WINDOW_MS", 10000),
		BatchFlushInterval:   envDurationMsOrDefault("BATCH_FLUSH_INTERVAL_MS", 5000),
		BatchMaxSize:         envIntOrDefault("BATCH_MAX_SIZE", 50),
		ResyncInterval:       envDurationMinOrDefault("RESYNC_INTERVAL_MIN", 60),
		WatchResourceTypes:   envCSVOrDefault("WATCH_RESOURCE_TYPES", nil),
		ExcludeResourceTypes: envCSVOrDefault("EXCLUDE_RESOURCE_TYPES", []string{"events", "leases", "endpointslices"}),
		LogLevel:             envOrDefault("LOG_LEVEL", "info"),
	}
}

func envOrDefault(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}

func envIntOrDefault(key string, defaultVal int) int {
	if val := os.Getenv(key); val != "" {
		if i, err := strconv.Atoi(val); err == nil {
			return i
		}
	}
	return defaultVal
}

func envDurationMsOrDefault(key string, defaultMs int) time.Duration {
	ms := envIntOrDefault(key, defaultMs)
	return time.Duration(ms) * time.Millisecond
}

func envDurationMinOrDefault(key string, defaultMin int) time.Duration {
	min := envIntOrDefault(key, defaultMin)
	return time.Duration(min) * time.Minute
}

func envCSVOrDefault(key string, defaultVal []string) []string {
	val := os.Getenv(key)
	if val == "" {
		return defaultVal
	}
	parts := strings.Split(val, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		trimmed := strings.TrimSpace(strings.ToLower(p))
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}
