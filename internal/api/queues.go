package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/goptics/vartop/internal/config"
)

// Job States of varmq spec/openapi.yaml JobState, in the order the UI lists them.
var JobStates = []string{"pending", "processing", "completed", "failed"}

// QueueSummary matches an entry of GET /queues (spec QueueSummary): Counts
// and Totals, or Err when the storage could not be read.
type QueueSummary struct {
	ID     string       `json:"id"`
	Counts *QueueCounts `json:"counts,omitempty"`
	Totals *QueueTotals `json:"totals,omitempty"`
	Err    string       `json:"error,omitempty"`
}

// QueueCounts are the jobs a queue holds in each Job State now.
type QueueCounts struct {
	Pending    int `json:"pending"`
	Processing int `json:"processing"`
	Completed  int `json:"completed"`
	Failed     int `json:"failed"`
}

// QueueTotals are the jobs counted since the storage began counting.
type QueueTotals struct {
	Submitted uint64 `json:"submitted"`
	Completed uint64 `json:"completed"`
	Failed    uint64 `json:"failed"`
}

// Job matches an entry of GET /queues/{id}/jobs (spec Job). Times are Unix
// ms; 0 when absent.
type Job struct {
	ID         string          `json:"id"`
	State      string          `json:"state"`
	Payload    json.RawMessage `json:"payload"`
	EnqueuedAt int64           `json:"enqueued_at,omitempty"`
	StartedAt  int64           `json:"started_at,omitempty"`
	FinishedAt int64           `json:"finished_at,omitempty"`
	Attempts   int             `json:"attempts"`
	Errors     []JobError      `json:"errors,omitempty"` // oldest first; the last is the final error
}

// JobError is one failed attempt of a job (spec JobError).
type JobError struct {
	Attempt int    `json:"attempt"`
	At      int64  `json:"at"` // Unix ms
	Error   string `json:"error"`
}

// unsupported reports whether err says the server does not serve the route:
// an older varmq, or a backend with no QueueInspector (404 or 405 from the
// router).
func unsupported(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusNotFound || apiErr.StatusCode == http.StatusMethodNotAllowed)
}

// ListQueues fetches GET /queues. ok is false, with no error, when the
// server does not serve it: no queue data, not a failure.
func (c *Client) ListQueues(ctx context.Context) (queues []QueueSummary, ok bool, err error) {
	if err := c.getJSON(ctx, "/queues", "list queues", &queues); err != nil {
		if unsupported(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return queues, true, nil
}

// ListJobs fetches one page of queue id's jobs in state. ok is false, with
// no error, on a 404: the queue is not bound on this instance, or the
// server serves no queue routes; another instance may have it.
func (c *Client) ListJobs(ctx context.Context, id, state string, offset, limit int) (jobs []Job, ok bool, err error) {
	q := url.Values{"state": {state}, "offset": {strconv.Itoa(offset)}, "limit": {strconv.Itoa(limit)}}
	// PathEscape keeps ':' and escapes '{', '}' and '/': one path segment, as the spec asks.
	if err := c.getJSON(ctx, "/queues/"+url.PathEscape(id)+"/jobs?"+q.Encode(), "list jobs of "+id, &jobs); err != nil {
		if unsupported(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return jobs, true, nil
}

// getJSON GETs path and decodes the JSON answer into target. what names the
// call in a transport error ("failed to <what>").
func (c *Client) getJSON(ctx context.Context, path, what string, target any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.doRequest(req)
	if err != nil {
		return fmt.Errorf("failed to %s from %s: %w", what, config.ShortOrigin(c.endpoint), err)
	}
	return c.handleResponse(resp, target)
}
