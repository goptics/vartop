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

// balancerActionDeadline bounds the sends of an action through a server's
// balancers, all its attempts together; sends to direct units are bounded
// by the client's timeout only.
const balancerActionDeadline = 15 * time.Second

// The refusals of what a load balancer cannot carry. oneBalancedMsg is also
// the note shown in place of one balanced unit's controls.
const (
	restartRefused = "restart isn't available through a load balancer"
	oneBalancedMsg = "A load balancer picks which instance answers, so actions here would reach a random one. Use All instances (merged), or list the instances' own addresses in the server config."
)

// Until a snapshot shows what an endpoint is, it may be a balancer, so
// nothing is sent to it: waitingMsg refuses a POST before the session's
// first snapshot, and notRefreshedMsg is the failure of an endpoint found
// since the latest one.
const (
	waitingMsg      = "Waiting for the first refresh — try again"
	notRefreshedMsg = "not refreshed yet — try again"
)

// noChange is what a fanOut fn returns for a unit it skipped, sending
// nothing; it becomes a "no change" result with the error text as Msg.
type noChange string

func (e noChange) Error() string { return string(e) }

// actScope is what an action reaches, as latest shows the server. Clients
// are sorted by endpoint; units are in latest's order.
type actScope struct {
	direct      []*api.Client  // the direct units' clients, each sent the action
	unrefreshed []*api.Client  // clients latest has no unit for, sent nothing
	balanced    []PodSnapshot  // units of instances behind a balancer, reached through balancers
	dead        []PodSnapshot  // units of balancer endpoints no instance answered through, sent nothing
	balancers   []*api.Client  // the balancer endpoints' clients
	behind      int            // how many instances latest counts behind balancers, in all
	place       map[string]int // unit key → its index in latest.Pods, the order the pages list units in
}

// viaBalancer reports whether sc holds a unit behind a load balancer.
func (sc actScope) viaBalancer() bool { return len(sc.balanced) > 0 || len(sc.dead) > 0 }

// listed compares units by key the way the pages list them (see
// Snapshot.Pods), units latest lacks last.
func (sc actScope) listed(a, b string) int {
	at := func(key string) int {
		if i, ok := sc.place[key]; ok {
			return i
		}
		return len(sc.place)
	}
	return cmp.Compare(at(a), at(b))
}

// scopeOf is what an action on pod reaches, as latest shows the server:
// every unit when pod is "", else the direct unit whose endpoint is pod. An
// endpoint is a balancer, never a direct unit, when it is a key of
// latest.Behind or the endpoint a balanced unit was reached through, dead
// ones included; it is a direct unit only when it is the key of an
// unbalanced unit of latest. A client that is neither, found since latest,
// may be a balancer too, so it is unrefreshed. A pod naming a balanced unit
// or a balancer is refused with oneBalancedMsg: the balancer would pick the
// instance. n words the error for a pod that is no unit of the server.
func scopeOf(clients []*api.Client, latest *Snapshot, pod string, n Noun) (actScope, error) {
	sc := actScope{place: make(map[string]int)}
	balancer := make(map[string]bool) // endpoint → it is a balancer
	direct := make(map[string]bool)   // endpoint → it is a direct unit
	one := false                      // pod is a balanced unit
	if latest != nil {
		for ep := range latest.Behind {
			balancer[ep] = true
		}
		for i, u := range latest.Pods {
			sc.place[u.Key] = i
			if !u.Balanced {
				direct[u.Key] = true
				continue
			}
			balancer[u.Via] = true
			one = one || u.Key == pod
			if u.Instance != "" {
				sc.balanced = append(sc.balanced, u)
			} else {
				sc.dead = append(sc.dead, u)
			}
		}
	}
	if pod != "" {
		if one || balancer[pod] {
			return actScope{}, errors.New(oneBalancedMsg)
		}
		for _, c := range clients {
			switch {
			case c.PrimaryEndpoint() != pod:
			case direct[pod]:
				return actScope{direct: []*api.Client{c}}, nil
			default:
				return actScope{unrefreshed: []*api.Client{c}}, nil
			}
		}
		return actScope{}, fmt.Errorf("the selected %s is not %s %s of this server; reload", n.One, n.Article, n.One)
	}
	for _, c := range slices.SortedFunc(slices.Values(clients), func(a, b *api.Client) int {
		return strings.Compare(a.PrimaryEndpoint(), b.PrimaryEndpoint())
	}) {
		switch ep := c.PrimaryEndpoint(); {
		case balancer[ep]:
			sc.balancers = append(sc.balancers, c)
			sc.behind += latest.Behind[ep]
		case direct[ep]:
			sc.direct = append(sc.direct, c)
		default:
			sc.unrefreshed = append(sc.unrefreshed, c)
		}
	}
	return sc, nil
}

// sender is one request act sends about a worker, given its name
// path-escaped: direct on a direct unit's own client, fresh through a
// balancer, on a new connection, reporting the instance that answered.
type sender struct {
	direct func(ctx context.Context, c *api.Client, name string) error
	fresh  func(ctx context.Context, c *api.Client, name string) (string, error)
}

// actionSender sends action (pause, resume, restart or stop).
func actionSender(action string) sender {
	return sender{
		direct: func(ctx context.Context, c *api.Client, name string) error { return c.ActionCtx(ctx, name, action) },
		fresh: func(ctx context.Context, c *api.Client, name string) (string, error) {
			return c.ActionFresh(ctx, name, action)
		},
	}
}

// act sends send's request about each worker of names to every unit of sc
// and returns, per name, one result per unit, listed as the pages list the
// units (see actScope.listed): the direct units' outcomes, the balanced
// units' (see reachUnits), the dead units', failed with their error, and
// the unrefreshed ones', failed with notRefreshedMsg. Results are labelled
// from units (see unitsOf); n words their messages.
func act(ctx context.Context, sc actScope, units map[string]PodSnapshot, n Noun, names []string, send sender) [][]ActionResult {
	results := make([][]ActionResult, len(names))
	var reached [][]ActionResult
	var wg sync.WaitGroup
	for i, name := range names {
		escaped := url.PathEscape(name)
		wg.Go(func() {
			results[i] = fanOut(ctx, sc.direct, units, n, func(ctx context.Context, c *api.Client) error {
				return send.direct(ctx, c, escaped)
			})
		})
	}
	if len(sc.balanced) > 0 {
		wg.Go(func() { reached = reachUnits(ctx, sc, units, n, names, send.fresh) })
	}
	wg.Wait()
	for i := range results {
		if reached != nil {
			results[i] = append(results[i], reached[i]...)
		}
		for _, u := range sc.dead {
			results[i] = append(results[i], outcome(u.Key, units, n, errors.New(u.Err)))
		}
		for _, c := range sc.unrefreshed {
			results[i] = append(results[i], outcome(c.PrimaryEndpoint(), units, n, errors.New(notRefreshedMsg)))
		}
		slices.SortStableFunc(results[i], func(a, b ActionResult) int { return sc.listed(a.Key, b.Key) })
	}
	return results
}

// fanOut calls fn concurrently on every client of targets and returns one
// result per client, in targets' order, labelled from units (see unitsOf).
// n words the results' messages.
func fanOut(ctx context.Context, targets []*api.Client, units map[string]PodSnapshot, n Noun,
	fn func(context.Context, *api.Client) error) []ActionResult {
	results := make([]ActionResult, len(targets))
	var wg sync.WaitGroup
	for i, c := range targets {
		wg.Go(func() { results[i] = outcome(c.PrimaryEndpoint(), units, n, fn(ctx, c)) })
	}
	wg.Wait()
	return results
}

// reachUnits sends fresh about each of names through sc's balancers, a
// reacher job per name, until the job has a definitive answer (a 2xx, a 409
// or a 404) from the instance of every balanced unit of sc, has spent its
// budget of attemptCap(max(units, instances behind, balancers)) attempts
// (the balancers pick among every instance behind them, not only the units
// wanted there), or balancerActionDeadline has passed. It returns, per
// name, one result per balanced unit: the outcome of its instance's
// definitive answer, else of its latest other answer, else a failure saying
// it was not reached. Answers from other instances are not results; an
// instance sent the request twice is unchanged by the repeat. One reacher
// carries every name, so jobs side by side do not fall into step behind a
// round-robin balancer.
func reachUnits(ctx context.Context, sc actScope, units map[string]PodSnapshot, n Noun, names []string,
	fresh func(context.Context, *api.Client, string) (string, error)) [][]ActionResult {
	ctx, cancel := context.WithTimeout(ctx, balancerActionDeadline)
	defer cancel()
	ids := make([]string, len(sc.balanced))
	for i, u := range sc.balanced {
		ids[i] = u.Instance
	}
	r := newReacher(sc.balancers, attemptCap(max(len(ids), sc.behind, len(sc.balancers))))
	jobs := make([]*job[struct{}], len(names))
	for i, name := range names {
		escaped := url.PathEscape(name)
		jobs[i] = addJob(r, func(ctx context.Context, c *api.Client) (struct{}, string, error) {
			instance, err := fresh(ctx, c, escaped)
			return struct{}{}, instance, err
		}, isUnchanged)
		jobs[i].want(ids...)
	}
	r.run(ctx)

	results := make([][]ActionResult, len(names))
	for i, j := range jobs {
		for _, u := range sc.balanced {
			a, ok := j.got[u.Instance]
			if !ok {
				a, ok = j.failed[u.Instance]
			}
			if !ok {
				a.Err = fmt.Errorf("not reached through the load balancer after %d tries", j.sent)
			}
			results[i] = append(results[i], outcome(u.Key, units, n, a.Err))
		}
	}
	return results
}

// isUnchanged reports whether err is varmq saying an action changes nothing
// on that instance: a 409 (already so) or a 404 (it lacks the worker).
func isUnchanged(err error) bool {
	var apiErr *api.APIError
	return errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusConflict || apiErr.StatusCode == http.StatusNotFound)
}

// outcome is the result of err on the unit key: varmq's 409 ("already in that
// state"), its 404 (the pod lacks the worker: actions fan out to every pod
// in scope) and a skip are no change, any other error is a failure.
func outcome(key string, units map[string]PodSnapshot, n Noun, err error) ActionResult {
	u := units[key]
	u.Key = key // a pod the snapshot lacks is still labelled and tipped by its key
	r := ActionResult{Pod: cmp.Or(u.Label, config.ShortOrigin(key)), Key: key, Tip: u.Tip(), Outcome: "ok"}
	var skip noChange
	var apiErr *api.APIError
	switch {
	case err == nil:
	case errors.As(err, &skip):
		r.Outcome, r.Msg = "no change", string(skip)
	case errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict:
		r.Outcome, r.Msg = "no change", apiErr.Message
	case errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound:
		r.Outcome, r.Msg = "no change", "not on this "+n.One
	default:
		r.Outcome, r.Msg = "failed", err.Error()
	}
	return r
}

// unitsOf maps each unit key of snap to the unit, for its label and tip.
func unitsOf(snap *Snapshot) map[string]PodSnapshot {
	units := make(map[string]PodSnapshot)
	if snap != nil {
		for _, p := range snap.Pods {
			units[p.Key] = p
		}
	}
	return units
}

// bind returns the session r acts on, the active one, read once, and its
// latest snapshot, when r's form names its server and the session has
// polled. Otherwise it answers 409 with the stale-tab notice, or waitingMsg
// before the first snapshot, and returns a nil session; nothing may reach
// varmq then. The caller must use only this session and snapshot for the
// whole request, never Current() again: a switch may replace the active one
// meanwhile.
func (h *handler) bind(w http.ResponseWriter, r *http.Request) (string, *Session, *Snapshot) {
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
		conflict(w, waitingMsg)
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
	noun := NounFor(s.Server)
	pod := r.PostFormValue("pod")
	sc, err := scopeOf(s.Clients(), latest, pod, noun)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	if action == "restart" && sc.viaBalancer() {
		badRequest(w, restartRefused)
		return
	}
	results := act(mutationCtx(r), sc, unitsOf(latest), noun, []string{name}, actionSender(action))
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
	noun := NounFor(s.Server)
	pod := r.PostFormValue("pod")
	sc, err := scopeOf(s.Clients(), latest, pod, noun)
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
	results := act(mutationCtx(r), sc, unitsOf(latest), noun, []string{name}, sender{
		direct: func(ctx context.Context, c *api.Client, name string) error {
			if cur, ok := current[c.PrimaryEndpoint()]; ok && cur == n {
				return noChange(fmt.Sprintf("already %d", n))
			}
			return c.TuneConcurrencyCtx(ctx, name, n)
		},
		fresh: func(ctx context.Context, c *api.Client, name string) (string, error) {
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
	noun := NounFor(s.Server)
	pod := r.PostFormValue("pod")
	units := unitsOf(latest)
	sc, err := scopeOf(s.Clients(), latest, pod, noun)
	if err != nil {
		badRequest(w, err.Error())
		return
	}

	if r.PostFormValue("confirm") != "1" {
		cv := ConfirmView{Action: action, Server: key, Pod: pod, Noun: noun, Rows: make([]ConfirmRow, len(names))}
		for i, name := range names {
			cv.Rows[i].Name = name
		}
		cols := slices.Concat(sc.balanced, sc.dead) // the units in scope, as act lists their results
		for _, c := range sc.direct {
			cols = append(cols, units[c.PrimaryEndpoint()])
		}
		for _, c := range sc.unrefreshed {
			cols = append(cols, PodSnapshot{Key: c.PrimaryEndpoint(), Err: notRefreshedMsg})
		}
		slices.SortStableFunc(cols, func(a, b PodSnapshot) int { return sc.listed(a.Key, b.Key) })
		for _, u := range cols {
			cv.Pods = append(cv.Pods, PodColumn{Key: u.Key, Label: cmp.Or(u.Label, config.ShortOrigin(u.Key)), Err: u.Err, Tip: u.Tip()})
			for i := range cv.Rows {
				name := cv.Rows[i].Name
				absent := u.Err == "" && u.Workers[name] == nil && !u.unreached(name)
				cv.Rows[i].Absent = append(cv.Rows[i].Absent, absent)
			}
		}
		h.fragments(w, fragment{"confirm", "confirm", cv})
		return
	}

	perName := act(mutationCtx(r), sc, units, noun, names, actionSender(action))
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
	frags := []fragment{{"results", "results", ResultsView{Results: results, Noun: NounFor(s.Server)}}}
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

	p := Page{Server: key, Pod: pod, Noun: NounFor(s.Server)}
	snaps := fill(&p, s.History)
	if podGone(&p, snaps) {
		return "", Page{}, false // the next poll shows why
	}
	switch set {
	case "dashboard":
		d := BuildDashboard(snaps, p.Pod, s.History.Interval(), p.Noun)
		p.Dashboard = &d
	case "detail":
		d, ok := BuildDetail(snaps, p.Pod, worker, s.History.Interval(), p.Noun)
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
