package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/goptics/varmq/cmd/vartop/internal/config"
)

// Client is a REST API client for interacting with a varmq mux server.
type Client struct {
	config     *config.Server
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
		config: cfg,
		httpClient: &http.Client{
			Timeout: 10 * time.Second, // Sane default timeout
		},
	}
}

// doRequest sends the HTTP request, adds auth headers if configured, and parses any errors.
func (c *Client) doRequest(req *http.Request) (*http.Response, error) {
	if c.config.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.config.Token)
	}
	req.Header.Set("Content-Type", "application/json")
	return c.httpClient.Do(req)
}

// handleResponse expects a successful JSON body or returns a parsed APIError.
func (c *Client) handleResponse(resp *http.Response, target interface{}) error {
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		if target == nil {
			return nil // No body expected
		}
		return json.NewDecoder(resp.Body).Decode(target)
	}

	// Try to read custom error JSON from the server
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

// GetHealth checks the /health endpoint of the server.
func (c *Client) GetHealth() error {
	req, err := http.NewRequest(http.MethodGet, c.config.Endpoint+"/health", nil)
	if err != nil {
		return err
	}

	resp, err := c.doRequest(req)
	if err != nil {
		return err
	}

	return c.handleResponse(resp, nil)
}

// ListWorkers fetches the array of worker summaries.
func (c *Client) ListWorkers() ([]WorkerSummary, error) {
	req, err := http.NewRequest(http.MethodGet, c.config.Endpoint+"/workers", nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.doRequest(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch workers: %w", err)
	}

	var workers []WorkerSummary
	if err := c.handleResponse(resp, &workers); err != nil {
		return nil, err
	}

	return workers, nil
}

// GetWorker fetches detailed info and metrics for a single worker.
func (c *Client) GetWorker(name string) (*WorkerDetails, error) {
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/workers/%s", c.config.Endpoint, name), nil)
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

	return &worker, nil
}

// Action dispatches a specific action (pause, resume, restart, stop) to a worker.
func (c *Client) Action(workerName, action string) error {
	req, err := http.NewRequest(http.MethodPatch, fmt.Sprintf("%s/workers/%s/actions/%s", c.config.Endpoint, workerName, action), nil)
	if err != nil {
		return err
	}

	resp, err := c.doRequest(req)
	if err != nil {
		return fmt.Errorf("failed to apply action %s to worker %s: %w", action, workerName, err)
	}

	return c.handleResponse(resp, nil)
}

// TuneConcurrency updates the concurrency of a given worker.
func (c *Client) TuneConcurrency(workerName string, concurrency int) error {
	req, err := http.NewRequest(http.MethodPatch, fmt.Sprintf("%s/workers/%s/config/concurrency/%d", c.config.Endpoint, workerName, concurrency), nil)
	if err != nil {
		return err
	}

	resp, err := c.doRequest(req)
	if err != nil {
		return fmt.Errorf("failed to tune concurrency for worker %s: %w", workerName, err)
	}

	return c.handleResponse(resp, nil)
}
