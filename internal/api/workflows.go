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

// WorkflowStates are the Workflow States in the order the UI lists them
// (spec 1.3.0 WorkflowState). A record may carry a state a later varmq adds:
// show it as it is.
var WorkflowStates = []contract.WorkflowState{
	contract.WorkflowRunning, contract.WorkflowCompleted, contract.WorkflowFailed,
	contract.WorkflowCompensating, contract.WorkflowCompensated, contract.WorkflowCompensationFailed,
	contract.WorkflowCancelled,
}

// Workflow route caps (spec 1.3.0): the largest page, the largest offset.
const (
	MaxWorkflowLimit  = 1000
	MaxWorkflowOffset = 10000
)

// ListWorkflows fetches GET /workflows: one page, newest created_at first,
// state "" for every state. ok is false, with no error, when the server
// does not serve the route (varmq before API 1.3.0: the router's 404 or
// 405). 503: every workflow storage failed, an error.
func (c *Client) ListWorkflows(ctx context.Context, state contract.WorkflowState, offset, limit int) (wfs []contract.WorkflowRecord, ok bool, err error) {
	q := url.Values{"offset": {strconv.Itoa(offset)}, "limit": {strconv.Itoa(limit)}}
	if state != "" {
		q.Set("state", string(state))
	}
	if err := c.getJSON(ctx, "/workflows?"+q.Encode(), "list workflows", &wfs); err != nil {
		if RouteMissing(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return wfs, true, nil
}

// GetWorkflow fetches GET /workflows/{id}. Errors are *APIError as sent:
// JSON 404 (no such workflow here), 409 (several storages here hold the
// ID: see WorkflowAmbiguous), 503; or the router's 404 (RouteMissing).
func (c *Client) GetWorkflow(ctx context.Context, id string) (contract.WorkflowRecord, error) {
	var w contract.WorkflowRecord
	if dotSegment(id) {
		return w, errNoSuchWorkflow
	}
	err := c.getJSON(ctx, "/workflows/"+url.PathEscape(id), "read workflow "+id, &w)
	return w, err
}

// CancelWorkflow sends PATCH /workflows/{id}/actions/cancel and returns the
// record as it is after. noUndo adds undo=false: the undo jobs of completed
// tasks do not run. Without it they run as the producer chose
// (undo_on_cancel); the spec refuses undo=true. Errors as GetWorkflow.
func (c *Client) CancelWorkflow(ctx context.Context, id string, noUndo bool) (contract.WorkflowRecord, error) {
	var w contract.WorkflowRecord
	if dotSegment(id) {
		return w, errNoSuchWorkflow
	}
	path := "/workflows/" + url.PathEscape(id) + "/actions/cancel"
	if noUndo {
		path += "?undo=false"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, c.endpoint+path, nil)
	if err != nil {
		return w, err
	}
	resp, err := c.doRequest(req)
	if err != nil {
		return w, fmt.Errorf("failed to cancel workflow %s on %s: %w", id, config.ShortOrigin(c.endpoint), err)
	}
	return w, c.handleResponse(resp, &w)
}

// dotSegment reports whether id is "." or "..": PathEscape keeps them, and
// as a path segment they would name another route. No workflow has one
// (spec: such an ID is not found, no storage asked), so none is sent.
func dotSegment(id string) bool { return id == "." || id == ".." }

// errNoSuchWorkflow is varmq's answer to an ID no workflow can have.
var errNoSuchWorkflow = &APIError{StatusCode: http.StatusNotFound, Message: "workflow not found"}

// RouteMissing reports whether err is the router saying it has no such
// route: its plain-text 404 (no {"error":...} body), or a 405. A JSON 404
// ("workflow not found") is not: the server has the route.
func RouteMissing(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.StatusCode == http.StatusMethodNotAllowed ||
		apiErr.StatusCode == http.StatusNotFound && apiErr.Message == http.StatusText(http.StatusNotFound)
}

// WorkflowAmbiguous reports whether err is a 409: more than one workflow
// store registered on that instance holds a workflow with the ID; nothing
// was read or cancelled.
func WorkflowAmbiguous(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict
}

// WorkflowNotFound reports whether err is a JSON 404: the instance serves
// the route but holds no workflow with the ID.
func WorkflowNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound && !RouteMissing(err)
}

// WorkflowFinal reports whether s is a state a workflow does not leave: it
// ended. An unknown state (a later varmq's) is not: it is shown, and
// polled, as running.
func WorkflowFinal(s contract.WorkflowState) bool {
	switch s {
	case contract.WorkflowCompleted, contract.WorkflowFailed, contract.WorkflowCompensated,
		contract.WorkflowCompensationFailed, contract.WorkflowCancelled:
		return true
	}
	return false
}
