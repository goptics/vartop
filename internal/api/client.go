package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/goptics/vartop/internal/config"
)

// Client is a REST API client for a single varmq mux server endpoint.
// Multi-pod servers get one Client per endpoint through a Pool.
type Client struct {
	endpoint   string
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

// NewClient establishes a new varmq API client for the given endpoint.
func NewClient(endpoint, token string) *Client {
	return &Client{
		endpoint: endpoint,
		token:    token,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// PrimaryEndpoint returns the endpoint this client talks to.
func (c *Client) PrimaryEndpoint() string {
	return c.endpoint
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

// ListWorkers retrieves workers from the client endpoint and tags them with the origin.
func (c *Client) ListWorkers() ([]WorkerSummary, error) {
	endpoint := c.endpoint
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

// GetHealth checks the /health endpoint of the client endpoint.
func (c *Client) GetHealth() error {
	req, err := http.NewRequest(http.MethodGet, c.endpoint+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := c.doRequest(req)
	if err != nil {
		return err
	}
	return c.handleResponse(resp, nil)
}
