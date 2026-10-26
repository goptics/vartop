package web

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/goptics/vartop/internal/api"
	"github.com/goptics/vartop/internal/config"
	"github.com/goptics/vartop/internal/fleet"
)

// The actions a worker route accepts, and the subset bulk accepts (the CLI's
// list; varmq also has start, which is not exposed).
var (
	workerActions = []string{"pause", "resume", "restart", "stop"}
	bulkActions   = []string{"pause", "resume"}
)

// bind returns the session r acts on, the active one, read once, and its
// latest snapshot, when r's form names its server and the session has
// polled. Otherwise it answers 409 with the stale-tab notice, or
// fleet.WaitingMsg before the first snapshot, and returns a nil session;
// nothing may reach varmq then. The caller must use only this session and snapshot for the
// whole request, never Current() again: a switch may replace the active one
// meanwhile.
func (h *handler) bind(w http.ResponseWriter, r *http.Request) (string, *Session, *fleet.Snapshot) {
	key, s := h.m.Active()
	switch {
	case s == nil:
		conflict(w, "Not connected — reload")
	case r.PostFormValue("server") != key:
		conflict(w, serverChanged(key))
	default:
		if latest := s.History.Latest(); latest != nil {
			return key, s, latest
		}
		conflict(w, fleet.WaitingMsg)
	}
	return "", nil, nil
}

// conflict answers 409 with msg as the notice in the page's banner.
func conflict(w http.ResponseWriter, msg string) {
	// htmx swaps error responses into #errors; the notice belongs in #banner.
	w.Header().Set("HX-Retarget", "#banner")
	w.Header().Set("HX-Reswap", "innerHTML")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusConflict)
	_, _ = fmt.Fprintf(w, `<p class="banner">%s</p>`, template.HTMLEscapeString(msg))
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
// scope of the form. Restart is refused when the scope holds a unit behind
// a load balancer: it would reach whichever instance the balancer picks.
func (h *handler) action(w http.ResponseWriter, r *http.Request) {
	name, action := r.PathValue("name"), r.PathValue("action")
	if !slices.Contains(workerActions, action) {
		badRequest(w, "unknown action: want pause, resume, restart or stop")
		return
	}
	key, s, latest := h.bind(w, r)
	if s == nil {
		return
	}
	noun := fleet.NounFor(s.Server)
	pod := r.PostFormValue("pod")
	sc, err := fleet.ScopeOf(s.Clients(), latest, pod, noun)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	if action == "restart" && sc.ViaBalancer() {
		badRequest(w, fleet.RestartRefusedMsg)
		return
	}
	results := fleet.Act(mutationCtx(r), sc, fleet.UnitsOf(latest), noun, []string{name}, fleet.ActionSender(action))
	h.respond(w, r, key, s, pod, name, results[0])
}

// concurrency sets one worker's per-pod concurrency to n across the pod
// scope of the form, skipping direct units whose latest sample already has
// n. Units behind a load balancer are always sent it: varmq answers 409 on
// one already at n, which is no change.
func (h *handler) concurrency(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	n, err := strconv.Atoi(r.PostFormValue("n"))
	if err != nil || n < 1 {
		badRequest(w, "concurrency must be a whole number of at least 1")
		return
	}
	key, s, latest := h.bind(w, r)
	if s == nil {
		return
	}
	noun := fleet.NounFor(s.Server)
	pod := r.PostFormValue("pod")
	sc, err := fleet.ScopeOf(s.Clients(), latest, pod, noun)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	current := make(map[string]int) // direct unit's endpoint (its key) → the worker's concurrency there
	for _, p := range latest.Pods {
		if smp := p.Workers[name]; smp != nil && !p.Balanced {
			current[p.Key] = smp.Concurrency
		}
	}
	results := fleet.Act(mutationCtx(r), sc, fleet.UnitsOf(latest), noun, []string{name}, fleet.Sender{
		Direct: func(ctx context.Context, c *api.Client, name string) error {
			if cur, ok := current[c.PrimaryEndpoint()]; ok && cur == n {
				return fleet.NoChange(fmt.Sprintf("already %d", n))
			}
			return c.TuneConcurrencyCtx(ctx, name, n)
		},
		Fresh: func(ctx context.Context, c *api.Client, name string) (string, error) {
			return c.TuneConcurrencyFresh(ctx, name, n)
		},
	})
	h.respond(w, r, key, s, pod, name, results[0])
}

// bulk pauses or resumes the workers named in the form across its pod
// scope. Without confirm=1 it only answers the confirmation panel, names ×
// units in scope, and calls nothing.
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
	key, s, latest := h.bind(w, r)
	if s == nil {
		return
	}
	noun := fleet.NounFor(s.Server)
	pod := r.PostFormValue("pod")
	units := fleet.UnitsOf(latest)
	sc, err := fleet.ScopeOf(s.Clients(), latest, pod, noun)
	if err != nil {
		badRequest(w, err.Error())
		return
	}

	if r.PostFormValue("confirm") != "1" {
		cv := ConfirmView{Action: action, Server: key, Pod: pod, Noun: noun, Rows: make([]ConfirmRow, len(names))}
		for i, name := range names {
			cv.Rows[i].Name = name
		}
		cols := slices.Concat(sc.Balanced, sc.Dead) // the units in scope, as act lists their results
		for _, c := range sc.Direct {
			cols = append(cols, units[c.PrimaryEndpoint()])
		}
		for _, c := range sc.Unrefreshed {
			cols = append(cols, fleet.PodSnapshot{Key: c.PrimaryEndpoint(), Err: fleet.NotRefreshedMsg})
		}
		slices.SortStableFunc(cols, func(a, b fleet.PodSnapshot) int { return sc.Listed(a.Key, b.Key) })
		for _, u := range cols {
			cv.Pods = append(cv.Pods, fleet.PodColumn{Key: u.Key, Label: cmp.Or(u.Label, config.ShortOrigin(u.Key)), Err: u.Err, Tip: u.Tip()})
			for i := range cv.Rows {
				name := cv.Rows[i].Name
				absent := u.Err == "" && u.Workers[name] == nil && !u.IsUnreached(name)
				cv.Rows[i].Absent = append(cv.Rows[i].Absent, absent)
			}
		}
		h.fragments(w, fragment{"confirm", "confirm", cv})
		return
	}

	perName := fleet.Act(mutationCtx(r), sc, units, noun, names, fleet.ActionSender(action))
	for i, res := range perName {
		for j := range res {
			res[j].Worker = names[i]
		}
	}
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
// fleet.SettleWait (less if the browser gives up) for a snapshot that shows
// them, then the #live region of the page the request came from as an
// out-of-band swap.
func (h *handler) respond(w http.ResponseWriter, r *http.Request, key string, s *Session, pod, worker string, results []fleet.ActionResult) {
	if slices.ContainsFunc(results, func(res fleet.ActionResult) bool { return res.Outcome != "no change" }) {
		ctx, cancel := context.WithTimeout(r.Context(), fleet.SettleWait)
		// false: the session was replaced or the poll is slow; the latest
		// snapshot is still the best there is.
		s.PollNow(ctx)
		cancel()
	}
	frags := []fragment{{"results", "results", ResultsView{Results: results, Noun: fleet.NounFor(s.Server)}}}
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

	p := Page{Server: key, Pod: pod, Noun: fleet.NounFor(s.Server)}
	snaps := fill(&p, s.History)
	if podGone(&p, snaps) {
		return "", Page{}, false // the next poll shows why
	}
	switch set {
	case "dashboard":
		d := fleet.BuildDashboard(snaps, p.Pod, s.History.Interval(), p.Noun)
		p.Dashboard = &d
	case "detail":
		d, ok := fleet.BuildDetail(snaps, p.Pod, worker, s.History.Interval(), p.Noun)
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

// fragments answers 200 with frags rendered in order, then #errors out of
// band: empty, so an earlier failure does not outlive this response. Each
// fragment is rendered into a buffer first so a template error never leaves
// half of one. A failing first fragment (what the request did) is a 500; a
// later one (the #live refresh, after varmq was called) is left out and
// named in #errors, so the results still show.
func (h *handler) fragments(w http.ResponseWriter, frags ...fragment) {
	var buf, part bytes.Buffer
	var failed string
	for i, f := range frags {
		part.Reset()
		if err := h.tmpl[f.set].ExecuteTemplate(&part, f.entry, f.data); err != nil {
			msg := "render " + f.set + "/" + f.entry + ": " + err.Error()
			if i == 0 {
				http.Error(w, template.HTMLEscapeString(msg), http.StatusInternalServerError)
				return
			}
			failed = cmp.Or(failed, "could not refresh the page: "+msg)
			continue
		}
		buf.Write(part.Bytes())
	}
	buf.WriteString(`<div id="errors" hx-swap-oob="innerHTML">`)
	if failed != "" {
		fmt.Fprintf(&buf, `<p class="err">%s</p>`, template.HTMLEscapeString(failed))
	}
	buf.WriteString(`</div>`)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}
