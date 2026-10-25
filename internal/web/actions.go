package web

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/goptics/vartop/internal/api"
	"github.com/goptics/vartop/internal/config"
)

// The actions a worker route accepts, and the subset bulk accepts (the CLI's
// list; varmq also has start, which is not exposed).
var (
	workerActions = []string{"pause", "resume", "restart", "stop"}
	bulkActions   = []string{"pause", "resume"}
)

// settleWait bounds how long an action response waits for the snapshot that
// shows its effect.
const settleWait = 2 * time.Second

// noChange is what a fanOut fn returns for a pod it skipped, sending
// nothing; it becomes a "no change" result with the error text as Msg.
type noChange string

func (e noChange) Error() string { return string(e) }

// fanOut calls fn concurrently on every client of clients, or only on the
// one whose endpoint is pod when pod is set, and returns one result per
// pod, sorted by endpoint and labelled from labels (endpoint → label). It
// calls nothing and errors when pod is not one of clients' endpoints.
func fanOut(ctx context.Context, clients []*api.Client, pod string, labels map[string]string,
	fn func(context.Context, *api.Client) error) ([]ActionResult, error) {
	targets, err := podScope(clients, pod)
	if err != nil {
		return nil, err
	}
	results := make([]ActionResult, len(targets))
	var wg sync.WaitGroup
	for i, c := range targets {
		wg.Go(func() { results[i] = outcome(c.PrimaryEndpoint(), labels, fn(ctx, c)) })
	}
	wg.Wait()
	return results, nil
}

// podScope is the clients an action on pod reaches, sorted by endpoint: all
// of them when pod is "", else the one whose endpoint is pod.
func podScope(clients []*api.Client, pod string) ([]*api.Client, error) {
	if pod == "" {
		return slices.SortedFunc(slices.Values(clients), func(a, b *api.Client) int {
			return strings.Compare(a.PrimaryEndpoint(), b.PrimaryEndpoint())
		}), nil
	}
	for _, c := range clients {
		if c.PrimaryEndpoint() == pod {
			return []*api.Client{c}, nil
		}
	}
	return nil, errors.New("the selected pod is not a pod of this server; reload")
}

// outcome is the result of err on endpoint: varmq's 409 ("already in that
// state") and a skip are no change, any other error is a failure.
func outcome(endpoint string, labels map[string]string, err error) ActionResult {
	r := ActionResult{Pod: cmp.Or(labels[endpoint], config.ShortOrigin(endpoint)), Endpoint: endpoint, Outcome: "ok"}
	var skip noChange
	var apiErr *api.APIError
	switch {
	case err == nil:
	case errors.As(err, &skip):
		r.Outcome, r.Msg = "no change", string(skip)
	case errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict:
		r.Outcome, r.Msg = "no change", apiErr.Message
	default:
		r.Outcome, r.Msg = "failed", err.Error()
	}
	return r
}

// labelsOf maps each pod endpoint of snap to its label.
func labelsOf(snap *Snapshot) map[string]string {
	labels := make(map[string]string)
	if snap != nil {
		for _, p := range snap.Pods {
			labels[p.Endpoint] = p.Label
		}
	}
	return labels
}

// bind returns the session r acts on: the active one, read once, when r's
// form names its server. Otherwise it answers 409 with the stale-tab notice
// and returns nil; nothing may reach varmq then. The caller must use only
// this session for the whole request, never Current() again: a switch may
// replace the active one meanwhile.
func (h *handler) bind(w http.ResponseWriter, r *http.Request) (string, *Session) {
	key, s := h.m.Active()
	if s != nil && r.PostFormValue("server") == key {
		return key, s
	}
	msg := "Not connected — reload"
	if s != nil {
		msg = serverChanged(key)
	}
	// htmx swaps error responses into #errors; the notice belongs in #banner.
	w.Header().Set("HX-Retarget", "#banner")
	w.Header().Set("HX-Reswap", "innerHTML")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusConflict)
	_, _ = fmt.Fprintf(w, `<p class="banner">%s</p>`, template.HTMLEscapeString(msg))
	return "", nil
}

// mutationCtx is the context varmq calls run under: r's values without its
// cancellation. htmx aborts a request when another control fires (hx-sync
// replace), which cancels r's context; PATCHes cut off midway would leave an
// action applied on some pods and not others, with no results shown. Each
// call is still bounded by the client's timeout.
func mutationCtx(r *http.Request) context.Context { return context.WithoutCancel(r.Context()) }

// badRequest answers 400 with msg, escaped: htmx inserts it into #errors as
// HTML. Callers never put request input in msg.
func badRequest(w http.ResponseWriter, msg string) {
	http.Error(w, template.HTMLEscapeString(msg), http.StatusBadRequest)
}

// action runs pause, resume, restart or stop on one worker across the pod
// scope of the form.
func (h *handler) action(w http.ResponseWriter, r *http.Request) {
	name, action := r.PathValue("name"), r.PathValue("action")
	if !slices.Contains(workerActions, action) {
		badRequest(w, "unknown action: want pause, resume, restart or stop")
		return
	}
	key, s := h.bind(w, r)
	if s == nil {
		return
	}
	pod := r.PostFormValue("pod")
	results, err := fanOut(mutationCtx(r), s.Clients(), pod, labelsOf(s.History.Latest()), func(ctx context.Context, c *api.Client) error {
		return c.ActionCtx(ctx, url.PathEscape(name), action)
	})
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	h.respond(w, r, key, s, pod, name, results)
}

// concurrency sets one worker's per-pod concurrency to n across the pod
// scope of the form, skipping pods whose latest sample already has n.
func (h *handler) concurrency(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	n, err := strconv.Atoi(r.PostFormValue("n"))
	if err != nil || n < 1 {
		badRequest(w, "concurrency must be a whole number of at least 1")
		return
	}
	key, s := h.bind(w, r)
	if s == nil {
		return
	}
	latest := s.History.Latest()
	current := make(map[string]int) // endpoint → the worker's concurrency there
	if latest != nil {
		for _, p := range latest.Pods {
			if smp := p.Workers[name]; smp != nil {
				current[p.Endpoint] = smp.Concurrency
			}
		}
	}
	pod := r.PostFormValue("pod")
	results, err := fanOut(mutationCtx(r), s.Clients(), pod, labelsOf(latest), func(ctx context.Context, c *api.Client) error {
		if cur, ok := current[c.PrimaryEndpoint()]; ok && cur == n {
			return noChange(fmt.Sprintf("already %d", n))
		}
		return c.TuneConcurrencyCtx(ctx, url.PathEscape(name), n)
	})
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	h.respond(w, r, key, s, pod, name, results)
}

// bulk pauses or resumes the workers named in the form across its pod
// scope. Without confirm=1 it only answers the confirmation panel, names ×
// pods on the server, and calls nothing.
func (h *handler) bulk(w http.ResponseWriter, r *http.Request) {
	action := r.PathValue("action")
	if !slices.Contains(bulkActions, action) {
		badRequest(w, "unknown bulk action: want pause or resume")
		return
	}
	if err := r.ParseForm(); err != nil {
		badRequest(w, "unreadable form")
		return
	}
	names := slices.DeleteFunc(slices.Compact(slices.Sorted(slices.Values(r.PostForm["names"]))), func(n string) bool { return n == "" })
	if len(names) == 0 {
		badRequest(w, "select at least one worker")
		return
	}
	key, s := h.bind(w, r)
	if s == nil {
		return
	}
	pod := r.PostFormValue("pod")
	latest := s.History.Latest()
	labels := labelsOf(latest)
	targets, err := podScope(s.Clients(), pod)
	if err != nil {
		badRequest(w, err.Error())
		return
	}

	if r.PostFormValue("confirm") != "1" {
		cv := ConfirmView{Action: action, Server: key, Pod: pod, Names: names}
		for _, c := range targets {
			ep := c.PrimaryEndpoint()
			col := PodColumn{Endpoint: ep, Label: cmp.Or(labels[ep], config.ShortOrigin(ep))}
			if latest != nil {
				if i := slices.IndexFunc(latest.Pods, func(p PodSnapshot) bool { return p.Endpoint == ep }); i >= 0 {
					col.Err = latest.Pods[i].Err
				}
			}
			cv.Pods = append(cv.Pods, col)
		}
		h.fragments(w, fragment{"confirm", "confirm", cv})
		return
	}

	ctx := mutationCtx(r)
	perName := make([][]ActionResult, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Go(func() {
			// targets is already scoped, so this cannot fail.
			res, _ := fanOut(ctx, targets, "", labels, func(ctx context.Context, c *api.Client) error {
				return c.ActionCtx(ctx, url.PathEscape(name), action)
			})
			for j := range res {
				res[j].Worker = name
			}
			perName[i] = res
		})
	}
	wg.Wait()
	h.respond(w, r, key, s, pod, "", slices.Concat(perName...))
}

// switchServer starts switching to the configured server named in the form
// and sends the tab to it. It skips the stale-tab check: it changes only
// vartop's own session, and a tab shown a failed switch posts that server.
func (h *handler) switchServer(w http.ResponseWriter, r *http.Request) {
	name := r.PostFormValue("name")
	if !slices.Contains(h.m.Servers(), name) {
		badRequest(w, "unknown server: pick one from the list")
		return
	}
	if err := h.m.Switch(name); err != nil {
		http.Error(w, template.HTMLEscapeString(err.Error()), http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("HX-Redirect", "/?server="+url.QueryEscape(name))
	w.WriteHeader(http.StatusNoContent)
}

// respond answers an executed action: results, after waiting up to
// settleWait (less if the browser gives up) for a snapshot that shows them, then the #live region of the
// page the request came from as an out-of-band swap.
func (h *handler) respond(w http.ResponseWriter, r *http.Request, key string, s *Session, pod, worker string, results []ActionResult) {
	if slices.ContainsFunc(results, func(res ActionResult) bool { return res.Outcome != "no change" }) {
		ctx, cancel := context.WithTimeout(r.Context(), settleWait)
		// false: the session was replaced or the poll is slow; the latest
		// snapshot is still the best there is.
		s.PollNow(ctx)
		cancel()
	}
	frags := []fragment{{"results", "results", results}}
	if set, p, ok := livePage(r, key, s, pod, worker); ok {
		frags = append(frags, fragment{set, "live_oob", p})
	}
	h.fragments(w, frags...)
}

// livePage is the page whose #live an action response refreshes: the one
// htmx says the request came from (HX-Current-URL), "/" being the dashboard
// and "/workers/{name}" that worker's detail; without the header, the page
// that carries the route's controls (detail of worker, or the dashboard).
// Other pages, a worker no pod reports, and a pod the newest snapshot lacks
// get no refresh.
func livePage(r *http.Request, key string, s *Session, pod, worker string) (string, Page, bool) {
	set := "dashboard"
	if worker != "" {
		set = "detail"
	}
	if from := r.Header.Get("HX-Current-URL"); from != "" {
		u, err := url.Parse(from)
		if err != nil {
			return "", Page{}, false
		}
		set, worker = "", ""
		path := u.EscapedPath()
		if path == "/" || path == "" {
			set = "dashboard"
		} else if esc, ok := strings.CutPrefix(path, "/workers/"); ok && !strings.Contains(esc, "/") {
			if name, err := url.PathUnescape(esc); err == nil && name != "" {
				set, worker = "detail", name
			}
		}
	}

	p := Page{Server: key, Pod: pod}
	snaps := fill(&p, s.History)
	if podGone(&p, snaps) {
		return "", Page{}, false // the next poll shows why
	}
	switch set {
	case "dashboard":
		d := BuildDashboard(snaps, p.Pod, s.History.Interval())
		p.Dashboard = &d
	case "detail":
		d, ok := BuildDetail(snaps, p.Pod, worker, s.History.Interval())
		if !ok {
			return "", Page{}, false
		}
		p.Detail = &d
	default:
		return "", Page{}, false
	}
	return set, p, true
}

// fragment is one template entry of a page set, with its data.
type fragment struct {
	set, entry string
	data       any
}

// fragments answers 200 with frags rendered in order. It renders into a
// buffer first so a template error never leaves half a response.
func (h *handler) fragments(w http.ResponseWriter, frags ...fragment) {
	var buf bytes.Buffer
	for _, f := range frags {
		if err := h.tmpl[f.set].ExecuteTemplate(&buf, f.entry, f.data); err != nil {
			http.Error(w, template.HTMLEscapeString("render "+f.set+"/"+f.entry+": "+err.Error()), http.StatusInternalServerError)
			return
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}
