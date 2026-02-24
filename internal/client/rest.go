package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"net/http"
	"time"

	"github.com/go-logr/logr"

	"github.com/wiggitywhitney/k8s-vectordb-sync/internal/controller"
)

// RESTClient sends SyncPayloads to the cluster-whisperer REST API.
type RESTClient struct {
	log        logr.Logger
	endpoint   string
	httpClient *http.Client

	// Retry configuration
	maxRetries   int
	initialDelay time.Duration
	maxDelay     time.Duration
}

// Option configures the RESTClient.
type Option func(*RESTClient)

// WithTimeout sets the HTTP client timeout for each request attempt.
func WithTimeout(d time.Duration) Option {
	return func(c *RESTClient) {
		c.httpClient.Timeout = d
	}
}

// WithRetry configures retry behavior with exponential backoff.
func WithRetry(maxRetries int, initialDelay, maxDelay time.Duration) Option {
	return func(c *RESTClient) {
		if maxRetries >= 0 {
			c.maxRetries = maxRetries
		}
		if initialDelay > 0 {
			c.initialDelay = initialDelay
		}
		if maxDelay > 0 {
			c.maxDelay = maxDelay
		}
		if c.initialDelay > c.maxDelay {
			c.initialDelay = c.maxDelay
		}
	}
}

// New creates a RESTClient that POSTs SyncPayloads to the given endpoint.
func New(log logr.Logger, endpoint string, opts ...Option) *RESTClient {
	c := &RESTClient{
		log:          log,
		endpoint:     endpoint,
		httpClient:   &http.Client{Timeout: 30 * time.Second},
		maxRetries:   3,
		initialDelay: 1 * time.Second,
		maxDelay:     30 * time.Second,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Send POSTs a SyncPayload to the configured endpoint with retry logic.
// Empty payloads (no upserts and no deletes) are skipped.
func (c *RESTClient) Send(ctx context.Context, payload controller.SyncPayload) error {
	if len(payload.Upserts) == 0 && len(payload.Deletes) == 0 {
		return nil
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshalling payload: %w", err)
	}

	var lastErr error
	for attempt := range c.maxRetries + 1 {
		if attempt > 0 {
			delay := c.backoffDelay(attempt)
			c.log.V(1).Info("Retrying after error",
				"attempt", attempt+1,
				"delay", delay,
				"error", lastErr,
			)
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return fmt.Errorf("context cancelled during retry backoff: %w", ctx.Err())
			}
		}

		lastErr = c.doPost(ctx, body)
		if lastErr == nil {
			if attempt > 0 {
				c.log.Info("Request succeeded after retry", "attempts", attempt+1)
			}
			return nil
		}

		// Don't retry on client errors (4xx) — only on server errors and network issues
		if isClientError(lastErr) {
			return lastErr
		}
	}

	return fmt.Errorf("exhausted %d retries: %w", c.maxRetries, lastErr)
}

// doPost performs a single HTTP POST request.
func (c *RESTClient) doPost(ctx context.Context, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("sending request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		return &clientError{statusCode: resp.StatusCode}
	}
	if resp.StatusCode >= 500 {
		return &serverError{statusCode: resp.StatusCode}
	}

	return nil
}

// backoffDelay calculates exponential backoff with jitter for the given attempt.
func (c *RESTClient) backoffDelay(attempt int) time.Duration {
	delay := float64(c.initialDelay) * math.Pow(2, float64(attempt-1))
	if delay > float64(c.maxDelay) {
		delay = float64(c.maxDelay)
	}
	// Add jitter: 75%-100% of calculated delay
	jitter := 0.75 + rand.Float64()*0.25
	return time.Duration(delay * jitter)
}

// clientError represents a 4xx HTTP error (not retryable).
type clientError struct {
	statusCode int
}

func (e *clientError) Error() string {
	return fmt.Sprintf("client error: HTTP %d", e.statusCode)
}

// serverError represents a 5xx HTTP error (retryable).
type serverError struct {
	statusCode int
}

func (e *serverError) Error() string {
	return fmt.Sprintf("server error: HTTP %d", e.statusCode)
}

// isClientError checks if the error is a non-retryable client error (4xx).
func isClientError(err error) bool {
	_, ok := err.(*clientError)
	return ok
}
