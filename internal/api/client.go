package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/goptics/vartop/internal/config"
)

// InstanceHeader is the varmq response header naming the process that
// answered (varmq.InstanceHeader).
const InstanceHeader = "X-Varmq-Instance"

// maxSeenInstances caps how many distinct instance IDs a Client remembers.
const maxSeenInstances = 16

// Client is a REST API client for a single varmq mux server endpoint.
// Multi-pod servers get one Client per endpoint through a Pool.
type Client struct {
	endpoint    string
	token       string
	httpClient  *http.Client // pooled: keep-alive connections are reused
	freshClient *http.Client // a new TCP connection for every request

	mu           sync.Mutex
	lastInstance string              // instance that answered the latest request
	seen         map[string]struct{} // distinct instances seen (capped)
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
	Instance       string `json:"-"` // varmq instance that served it (InstanceHeader), if reported
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
	fresh := http.DefaultTransport.(*http.Transport).Clone()
	fresh.DisableKeepAlives = true
	return &Client{
		endpoint: endpoint,
		token:    token,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		freshClient: &http.Client{
			Timeout:   10 * time.Second,
			Transport: fresh,
		},
	}
}

// PrimaryEndpoint returns the endpoint this client talks to.
func (c *Client) PrimaryEndpoint() string {
	return c.endpoint
}

// doRequest sends the HTTP request, adds auth headers if configured, and parses any errors.
func (c *Client) doRequest(req *http.Request) (*http.Response, error) {
	return c.send(c.httpClient, req)
}

// send is doRequest over the given http.Client: pooled or fresh.
func (c *Client) send(hc *http.Client, req *http.Request) (*http.Response, error) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err == nil {
		c.recordInstance(resp.Header.Get(InstanceHeader))
	}
	return resp, err
}

// recordInstance remembers which varmq instance answered. More than one
// distinct instance behind a single endpoint means it is load-balanced.
func (c *Client) recordInstance(id string) {
	if id == "" {
		return // older varmq without the header
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastInstance = id
	if c.seen == nil {
		c.seen = make(map[string]struct{})
	}
	if len(c.seen) < maxSeenInstances {
		c.seen[id] = struct{}{}
	}
}

// LastInstance returns the instance ID that answered the latest request, or
// "" if the server does not report one.
func (c *Client) LastInstance() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastInstance
}

// SeenInstances returns how many distinct instances have answered this
// client's endpoint (capped at maxSeenInstances). Above 1 means the endpoint
// is load-balanced: each request reaches one instance, not all of them.
func (c *Client) SeenInstances() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.seen)
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
	return c.ListWorkersCtx(context.Background())
}

// ListWorkersCtx is ListWorkers with a cancellable request context.
func (c *Client) ListWorkersCtx(ctx context.Context) ([]WorkerSummary, error) {
	workers, _, err := c.listWorkers(ctx, c.httpClient)
	return workers, err
}

// ListWorkersFresh is ListWorkersCtx over a new connection, so a load balancer
// that picks per connection may route it to a different instance. It also
// returns the instance that answered ("" if none did or the server does not
// report one).
func (c *Client) ListWorkersFresh(ctx context.Context) ([]WorkerSummary, string, error) {
	return c.listWorkers(ctx, c.freshClient)
}

func (c *Client) listWorkers(ctx context.Context, hc *http.Client) ([]WorkerSummary, string, error) {
	endpoint := c.endpoint
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/workers", nil)
	if err != nil {
		return nil, "", err
	}

	resp, err := c.send(hc, req)
	if err != nil {
		return nil, "", fmt.Errorf("failed to fetch workers from %s: %w", config.ShortOrigin(endpoint), err)
	}

	instance := resp.Header.Get(InstanceHeader)
	var workers []WorkerSummary
	if err := c.handleResponse(resp, &workers); err != nil {
		return nil, instance, err
	}

	for i := range workers {
		workers[i].Origin = endpoint
		workers[i].Instance = instance
	}

	return workers, instance, nil
}

// GetWorker fetches detailed info and metrics for a single worker.
func (c *Client) GetWorker(name string) (*WorkerDetails, error) {
	return c.GetWorkerCtx(context.Background(), name)
}

// GetWorkerCtx is GetWorker with a cancellable request context. The name is
// used as given: callers pass it already path-escaped.
func (c *Client) GetWorkerCtx(ctx context.Context, name string) (*WorkerDetails, error) {
	worker, _, err := c.getWorker(ctx, c.httpClient, name)
	return worker, err
}

// GetWorkerFresh is GetWorkerCtx over a new connection, returning the instance
// that answered, even when the answer is an error status.
func (c *Client) GetWorkerFresh(ctx context.Context, name string) (*WorkerDetails, string, error) {
	return c.getWorker(ctx, c.freshClient, name)
}

func (c *Client) getWorker(ctx context.Context, hc *http.Client, name string) (*WorkerDetails, string, error) {
	ep := c.endpoint
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/workers/%s", ep, name), nil)
	if err != nil {
		return nil, "", err
	}

	resp, err := c.send(hc, req)
	if err != nil {
		return nil, "", fmt.Errorf("failed to fetch worker %s: %w", name, err)
	}

	instance := resp.Header.Get(InstanceHeader)
	var worker WorkerDetails
	if err := c.handleResponse(resp, &worker); err != nil {
		return nil, instance, err
	}

	worker.Origin = ep
	worker.Instance = instance

	return &worker, instance, nil
}

// Action dispatches a specific action (pause, resume, restart, stop) to a worker.
func (c *Client) Action(workerName, action string) error {
	return c.ActionCtx(context.Background(), workerName, action)
}

// ActionCtx is Action with a cancellable request context. The worker name is
// used as given: callers pass it already path-escaped.
func (c *Client) ActionCtx(ctx context.Context, workerName, action string) error {
	_, err := c.applyAction(ctx, c.httpClient, workerName, action)
	return err
}

// ActionFresh is ActionCtx over a new connection, returning the instance that
// answered, even when the answer is an error status.
func (c *Client) ActionFresh(ctx context.Context, workerName, action string) (string, error) {
	return c.applyAction(ctx, c.freshClient, workerName, action)
}

func (c *Client) applyAction(ctx context.Context, hc *http.Client, workerName, action string) (string, error) {
	return c.patch(ctx, hc, fmt.Sprintf("/workers/%s/actions/%s", workerName, action),
		fmt.Sprintf("apply action %s to worker %s", action, workerName))
}

// TuneConcurrency updates the concurrency of a given worker.
func (c *Client) TuneConcurrency(workerName string, concurrency int) error {
	return c.TuneConcurrencyCtx(context.Background(), workerName, concurrency)
}

// TuneConcurrencyCtx is TuneConcurrency with a cancellable request context.
// The worker name is used as given: callers pass it already path-escaped.
func (c *Client) TuneConcurrencyCtx(ctx context.Context, workerName string, concurrency int) error {
	_, err := c.tuneConcurrency(ctx, c.httpClient, workerName, concurrency)
	return err
}

// TuneConcurrencyFresh is TuneConcurrencyCtx over a new connection, returning
// the instance that answered, even when the answer is an error status.
func (c *Client) TuneConcurrencyFresh(ctx context.Context, workerName string, concurrency int) (string, error) {
	return c.tuneConcurrency(ctx, c.freshClient, workerName, concurrency)
}

func (c *Client) tuneConcurrency(ctx context.Context, hc *http.Client, workerName string, concurrency int) (string, error) {
	return c.patch(ctx, hc, fmt.Sprintf("/workers/%s/config/concurrency/%d", workerName, concurrency),
		fmt.Sprintf("tune concurrency for worker %s", workerName))
}

// patch sends a body-less PATCH to path and returns the answering instance.
// what names the call in a transport error ("failed to <what>").
func (c *Client) patch(ctx context.Context, hc *http.Client, path, what string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, c.endpoint+path, nil)
	if err != nil {
		return "", err
	}

	resp, err := c.send(hc, req)
	if err != nil {
		return "", fmt.Errorf("failed to %s: %w", what, err)
	}

	return resp.Header.Get(InstanceHeader), c.handleResponse(resp, nil)
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
