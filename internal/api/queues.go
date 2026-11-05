package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/goptics/varmq/contract"
	"github.com/goptics/vartop/internal/config"
)

// JobStates are the Job States in the order the UI lists them.
var JobStates = []contract.JobState{contract.JobPending, contract.JobProcessing, contract.JobCompleted, contract.JobFailed}

// QueueSummary matches an entry of GET /queues (spec QueueSummary): Counts
// and Totals, or Err when the storage could not be read. The contract has
// no type for it: contract.QueueStats has no ID or error, and its Counts
// and Totals are always present.
type QueueSummary struct {
	ID     string              `json:"id"`
	Counts *contract.JobCounts `json:"counts,omitempty"`
	Totals *contract.JobTotals `json:"totals,omitempty"`
	Err    string              `json:"error,omitempty"`
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
func (c *Client) ListJobs(ctx context.Context, id string, state contract.JobState, offset, limit int) (jobs []contract.JobRecord, ok bool, err error) {
	q := url.Values{"state": {string(state)}, "offset": {strconv.Itoa(offset)}, "limit": {strconv.Itoa(limit)}}
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
