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

// JobStates are the Job States in the order the UI lists them. Cancelled
// last: varmq reserves it (spec 1.3.0); its counts have no cancelled field
// and its jobs route may refuse the state.
var JobStates = []contract.JobState{contract.JobPending, contract.JobProcessing, contract.JobCompleted, contract.JobFailed, contract.JobCancelled}

// QueueSummary matches an entry of GET /queues (spec QueueSummary): Counts
// and Totals, or Err when the storage could not be read. The contract has
// no type for it: contract.QueueStats has no ID or error, and its Counts
// and Totals are always present.
//
// Paused is nil when the server does not report it: varmq before API 1.2.0,
// which has no queue pause route either. PausedAt: Unix ms on the storage's
// clock, set only while paused.
type QueueSummary struct {
	ID       string              `json:"id"`
	Counts   *contract.JobCounts `json:"counts,omitempty"`
	Totals   *contract.JobTotals `json:"totals,omitempty"`
	Paused   *bool               `json:"paused,omitempty"`
	PausedAt int64               `json:"paused_at,omitempty"`
	Err      string              `json:"error,omitempty"`
}

// IsPaused reports whether q says it is paused.
func (q QueueSummary) IsPaused() bool { return q.Paused != nil && *q.Paused }

// CanPause reports whether q's server serves the queue pause route: it
// reports paused (API 1.2.0+).
func (q QueueSummary) CanPause() bool { return q.Paused != nil }

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

// QueueAction sends PATCH /queues/{id}/actions/{action} (pause or resume)
// and returns the queue's entry as the server answers it, once the flag is
// stored. Errors are *APIError as sent: 404 (queue not bound here, or no
// such route on an older varmq), 405, 501 (storage cannot pause), 503.
func (c *Client) QueueAction(ctx context.Context, id, action string) (QueueSummary, error) {
	var q QueueSummary
	// PathEscape: one path segment, as ListJobs.
	path := "/queues/" + url.PathEscape(id) + "/actions/" + url.PathEscape(action)
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, c.endpoint+path, nil)
	if err != nil {
		return q, err
	}
	resp, err := c.doRequest(req)
	if err != nil {
		return q, fmt.Errorf("failed to %s queue %s on %s: %w", action, id, config.ShortOrigin(c.endpoint), err)
	}
	return q, c.handleResponse(resp, &q)
}

// QueueActionUnsupported reports whether err is a server saying it cannot
// pause or resume the queue: no such route (an older varmq: the router's
// plain-text 404, or a 405) or storage that cannot pause (501). A JSON 404
// ("queue not found") is not: the server has the route but not the queue.
func QueueActionUnsupported(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.StatusCode {
	case http.StatusMethodNotAllowed, http.StatusNotImplemented:
		return true
	case http.StatusNotFound:
		return apiErr.Message == http.StatusText(http.StatusNotFound) // no {"error":...} body
	}
	return false
}

// jobPath is the path of job jobID of queue id, each one escaped path
// segment. The IDs "." and ".." go as %2E and %2E%2E (spec JobID): as
// they are, the path would be cleaned onto another route.
func jobPath(id, jobID string) string {
	seg := url.PathEscape(jobID)
	switch jobID {
	case ".":
		seg = "%2E"
	case "..":
		seg = "%2E%2E"
	}
	return "/queues/" + url.PathEscape(id) + "/jobs/" + seg
}

// GetJob fetches GET /queues/{id}/jobs/{jobId} (API 1.4.0): one job, its
// result included. ok is false, with no error, when the server does not
// serve it (see JobRouteUnsupported). A JSON 404 (queue or job not found
// on this instance) is an *APIError: another instance may have it.
func (c *Client) GetJob(ctx context.Context, id, jobID string) (job contract.JobRecord, ok bool, err error) {
	if err := c.getJSON(ctx, jobPath(id, jobID), "read job "+jobID+" of "+id, &job); err != nil {
		if JobRouteUnsupported(err) {
			return job, false, nil
		}
		return job, false, err
	}
	return job, true, nil
}

// CancelJob sends PATCH /queues/{id}/jobs/{jobId}/actions/cancel (API
// 1.4.0). nil: 202, cancel requested. ok is false, with no error, when the
// server does not serve it (see JobRouteUnsupported). Other errors are
// *APIError as sent: 404 (queue, or an in-memory queue's job, not here),
// 409 (the job already ended), 503.
func (c *Client) CancelJob(ctx context.Context, id, jobID string) (ok bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, c.endpoint+jobPath(id, jobID)+"/actions/cancel", nil)
	if err != nil {
		return false, err
	}
	resp, err := c.doRequest(req)
	if err != nil {
		return false, fmt.Errorf("failed to cancel job %s of %s on %s: %w", jobID, id, config.ShortOrigin(c.endpoint), err)
	}
	if err := c.handleResponse(resp, nil); err != nil {
		if JobRouteUnsupported(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// JobRouteUnsupported reports whether err says the server cannot serve a
// one-job route: as QueueActionUnsupported, no such route (varmq before
// API 1.4.0) or storage without the interface (501).
func JobRouteUnsupported(err error) bool { return QueueActionUnsupported(err) }

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
