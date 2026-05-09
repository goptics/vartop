package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/goptics/varmq/cmd/vartop/internal/config"
)

// Client is a REST API client for interacting with one or more varmq mux server endpoints.
type Client struct {
	endpoints  []string
	token      string
	httpClient *http.Client
}

// WorkerSummary matches the API response of GET /workers or GET /workers/{name}
type WorkerSummary struct {
	Name           string `json:"name"`
	Status         string `json:"status"`
	Concurrency    int    `json:"concurrency"`
	NumProcessing  int    `json:"num_processing"`
	NumPending     int    `json:"num_pending"`
	NumIdleWorkers int    `json:"num_idle_workers"`
	Origin         string `json:"-"` // Endpoint this worker was fetched from (set by ListWorkers)
}

// WorkerDetails matches the extended API response of GET /workers/{name}
type WorkerDetails struct {
	WorkerSummary
	Metrics struct {
		Submitted  uint64 `json:"submitted"`
		Completed  uint64 `json:"completed"`
		Successful uint64 `json:"successful"`
		Failed     uint64 `json:"failed"`
	} `json:"metrics"`
}

// APIError represents an error response from the varmq server.
type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("API error (status %d): %s", e.StatusCode, e.Message)
}

// NewClient establishes a new varmq API client using the given server configuration.
func NewClient(cfg *config.Server) *Client {
	return &Client{
		endpoints: cfg.GetEndpoints(),
		token:     cfg.Token,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// PrimaryEndpoint returns the first endpoint, or empty string if none configured.
func (c *Client) PrimaryEndpoint() string {
	if len(c.endpoints) > 0 {
		return c.endpoints[0]
	}
	return ""
}

// resolveOrigin returns origin if non-empty, otherwise falls back to primary endpoint.
func (c *Client) resolveOrigin(origin string) (string, error) {
	if origin != "" {
		return origin, nil
	}
	primary := c.PrimaryEndpoint()
	if primary == "" {
		return "", fmt.Errorf("no endpoints configured")
	}
	return primary, nil
}

// doRequest sends the HTTP request, adds auth headers if configured, and parses any errors.
func (c *Client) doRequest(req *http.Request) (*http.Response, error) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Content-Type", "application/json")
	return c.httpClient.Do(req)
}

// handleResponse expects a successful JSON body or returns a parsed APIError.
func (c *Client) handleResponse(resp *http.Response, target interface{}) error {
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		if target == nil {
			return nil
		}
		return json.NewDecoder(resp.Body).Decode(target)
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err == nil && len(bodyBytes) > 0 {
		var errData map[string]string
		if json.Unmarshal(bodyBytes, &errData) == nil {
			if msg, ok := errData["error"]; ok {
				return &APIError{StatusCode: resp.StatusCode, Message: msg}
			}
		}
	}

	return &APIError{
		StatusCode: resp.StatusCode,
		Message:    http.StatusText(resp.StatusCode),
	}
}

// ListWorkersAt retrieves workers from a single endpoint and tags them with the origin.
func (c *Client) ListWorkersAt(endpoint string) ([]WorkerSummary, error) {
	req, err := http.NewRequest(http.MethodGet, endpoint+"/workers", nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.doRequest(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch workers from %s: %w", config.ShortOrigin(endpoint), err)
	}

	var workers []WorkerSummary
	if err := c.handleResponse(resp, &workers); err != nil {
		return nil, err
	}

	for i := range workers {
		workers[i].Origin = endpoint
	}

	return workers, nil
}

// ListWorkers fans out to all configured endpoints concurrently and returns merged results.
func (c *Client) ListWorkers() ([]WorkerSummary, error) {
	if len(c.endpoints) == 1 {
		return c.ListWorkersAt(c.endpoints[0])
	}

	type result struct {
		workers []WorkerSummary
		err     error
		origin  string
	}

	var wg sync.WaitGroup
	results := make([]result, len(c.endpoints))

	for i, ep := range c.endpoints {
		wg.Add(1)
		go func(idx int, endpoint string) {
			defer wg.Done()
			w, err := c.ListWorkersAt(endpoint)
			results[idx] = result{workers: w, err: err, origin: endpoint}
		}(i, ep)
	}

	wg.Wait()

	var merged []WorkerSummary
	var errs []error

	for _, r := range results {
		if r.err != nil {
			errs = append(errs, r.err)
			continue
		}
		merged = append(merged, r.workers...)
	}

	if len(merged) == 0 && len(errs) > 0 {
		return nil, fmt.Errorf("all endpoints failed: %v", errs[0])
	}

	return merged, nil
}

// GetWorker fetches detailed info and metrics for a single worker from the given origin endpoint.
func (c *Client) GetWorker(name, origin string) (*WorkerDetails, error) {
	ep, err := c.resolveOrigin(origin)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/workers/%s", ep, name), nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.doRequest(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch worker %s: %w", name, err)
	}

	var worker WorkerDetails
	if err := c.handleResponse(resp, &worker); err != nil {
		return nil, err
	}

	worker.Origin = ep

	return &worker, nil
}

// Action dispatches a specific action (pause, resume, restart, stop) to a worker at the given origin.
func (c *Client) Action(workerName, action, origin string) error {
	ep, err := c.resolveOrigin(origin)
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPatch, fmt.Sprintf("%s/workers/%s/actions/%s", ep, workerName, action), nil)
	if err != nil {
		return err
	}

	resp, err := c.doRequest(req)
	if err != nil {
		return fmt.Errorf("failed to apply action %s to worker %s: %w", action, workerName, err)
	}

	return c.handleResponse(resp, nil)
}

// TuneConcurrency updates the concurrency of a given worker at the given origin.
func (c *Client) TuneConcurrency(workerName string, concurrency int, origin string) error {
	ep, err := c.resolveOrigin(origin)
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPatch, fmt.Sprintf("%s/workers/%s/config/concurrency/%d", ep, workerName, concurrency), nil)
	if err != nil {
		return err
	}

	resp, err := c.doRequest(req)
	if err != nil {
		return fmt.Errorf("failed to tune concurrency for worker %s: %w", workerName, err)
	}

	return c.handleResponse(resp, nil)
}

// GetHealth checks the /health endpoint of all configured endpoints.
// Returns nil if at least one endpoint is healthy.
func (c *Client) GetHealth() error {
	var wg sync.WaitGroup
	errs := make([]error, len(c.endpoints))

	for i, ep := range c.endpoints {
		wg.Add(1)
		go func(idx int, endpoint string) {
			defer wg.Done()
			req, err := http.NewRequest(http.MethodGet, endpoint+"/health", nil)
			if err != nil {
				errs[idx] = err
				return
			}
			resp, err := c.doRequest(req)
			if err != nil {
				errs[idx] = err
				return
			}
			errs[idx] = c.handleResponse(resp, nil)
		}(i, ep)
	}

	wg.Wait()

	// Healthy if any endpoint succeeds
	for _, err := range errs {
		if err == nil {
			return nil
		}
	}

	return fmt.Errorf("all endpoints unreachable")
}

// GetHealthStatus returns per-endpoint health status. Used by the TUI to show individual pod health.
func (c *Client) GetHealthStatus() map[string]bool {
	status := make(map[string]bool, len(c.endpoints))

	var wg sync.WaitGroup
	var mu sync.Mutex

	for _, ep := range c.endpoints {
		wg.Add(1)
		go func(endpoint string) {
			defer wg.Done()
			req, err := http.NewRequest(http.MethodGet, endpoint+"/health", nil)
			if err != nil {
				mu.Lock()
				status[endpoint] = false
				mu.Unlock()
				return
			}
			resp, err := c.doRequest(req)
			if err != nil {
				mu.Lock()
				status[endpoint] = false
				mu.Unlock()
				return
			}
			err = c.handleResponse(resp, nil)
			mu.Lock()
			status[endpoint] = err == nil
			mu.Unlock()
		}(ep)
	}

	wg.Wait()
	return status
}
