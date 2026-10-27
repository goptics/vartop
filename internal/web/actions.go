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
	"time"

	"github.com/goptics/vartop/internal/api"
	"github.com/goptics/vartop/internal/config"
	"github.com/goptics/vartop/internal/fleet"
	"github.com/goptics/vartop/internal/store"
)

// The actions a worker route accepts, and the subset bulk accepts (the CLI's
// list; varmq also has start, which is not exposed).
var (
	workerActions = []string{"pause", "resume", "restart", "stop"}
	bulkActions   = []string{"pause", "resume"}
)

// settlePoll is how often a request waiting for a snapshot syncs its feed.
const settlePoll = 50 * time.Millisecond

// target is what an action request acts on, read once for the whole
// request: the server its form names, that server's feed, the newest
// snapshot — fresh — and the unit label the form names, "" for every unit.
type target struct {
	key    string
	srv    *config.Server
	noun   fleet.Noun
	feed   *store.Feed
	latest *fleet.Snapshot
	label  string
}

// bind returns what r acts on (see target), once its form names a
// configured server whose newest snapshot may be acted on — current, under
// a fresh lease of this environment's collector (store.Collector.Acts, R44)
// — and has the unit the form names. A server without one is poked, and
// given up to fleet.SettleWait to take one, unless no collector runs to
// answer; the lease is read again after. Otherwise it answers 409 with the
// notice why — not configured, the lease unreadable, the collector collecting
// in another environment, waiting for the first snapshot, stale (or a
// previous collector's, or no collector collecting), the unit gone or unknown,
// the form out of date — and returns false; nothing may reach varmq then.
//
// The unit parameter is taken as it is, unlike a page's (see labelOf): a
// form from before labels, still open in a tab, names its unit by key,
// under pod= on every server. Its key is no label, and the other noun's
// parameter, set, is no unit of this server: either is refused, where
// reading it as absent would act on every unit. Empty, it is the old
// merged form, which acts on every unit as it says.
func (h *handler) bind(w http.ResponseWriter, r *http.Request) (*target, bool) {
	cfg, err := h.config()
	if err != nil {
		conflict(w, "cannot read the config: "+err.Error())
		return nil, false
	}
	t := &target{key: serverOf(cfg, r.PostFormValue("server"))}
	if t.srv = cfg.Servers[t.key]; t.srv == nil {
		conflict(w, fleet.NotConfiguredMsg(t.key))
		return nil, false
	}
	t.noun = fleet.NounFor(t.srv)
	interval := t.srv.PollInterval()
	t.feed = h.feeds.get(t.key, interval, time.Now())
	if err := t.feed.Sync(); err != nil {
		conflict(w, fleet.SyncErrMsg(t.key, err))
		return nil, false
	}
	// The snapshots, read once (the label is checked in the snapshot acted
	// on), then the lease: whatever they hold was taken under it, or an
	// earlier one (R44).
	snaps := t.feed.Snapshots()
	d := h.collector()
	if refused(w, d) {
		return nil, false
	}
	source := fleet.SourceOf(t.srv)
	if l := latestOf(snaps); !d.Acts(l, interval, time.Now()) || l.Source != source {
		h.refresh(r.Context(), t)
		// Again: the lease may have changed hands while it waited.
		snaps = t.feed.Snapshots()
		if d = h.collector(); refused(w, d) {
			return nil, false
		}
	}
	latest := latestOf(snaps)
	switch now := time.Now(); {
	case latest == nil:
		conflict(w, fleet.WaitingMsg(t.key))
		return nil, false
	case !d.Acts(latest, interval, now):
		// Stale, a previous collector's, or none collects (R44).
		conflict(w, h.staleMsg(t.key, latest, now))
		return nil, false
	case latest.Source != source:
		// Polled under the server's previous endpoints: its units are not
		// the ones the config lists, and the token read now must not go
		// to them.
		conflict(w, fleet.ConfigChangedMsg(t.key))
		return nil, false
	}
	t.latest = latest
	if r.PostFormValue(otherParam(t.noun)) != "" {
		conflict(w, outOfDateMsg)
		return nil, false
	}
	if t.label = r.PostFormValue(t.noun.Param); t.label != "" {
		if msg := missingLabel(snaps, t.label, t.key, t.noun); msg != "" {
			conflict(w, msg)
			return nil, false
		}
	}
	return t, true
}

// refused answers 409 when d lets no action out, whatever the snapshot,
// and reports whether it did: the lease cannot be read (R44: fail closed),
// or the collector collects in another environment (R31) — its snapshots name
// units of that one, which this config's token must not reach. Nothing is
// poked then either.
func refused(w http.ResponseWriter, d store.Collector) bool {
	switch {
	case d.Err != nil:
		conflict(w, fleet.LeaseErrMsg(d.Err))
	case d.Other:
		conflict(w, fleet.OtherConfigMsg(d.Lease.PID))
	default:
		return false
	}
	return true
}

// outOfDateMsg refuses a form that names a unit under the other noun's
// parameter: one from before labels.
const outOfDateMsg = "This page is out of date — reload"

// otherParam is the unit parameter of the other kind of server than n's.
func otherParam(n fleet.Noun) string {
	if n.Param == "pod" {
		return "instance"
	}
	return "pod"
}

// refresh pokes t's server and syncs its feed until it holds a snapshot
// taken after the poke, for up to fleet.SettleWait or until ctx ends. With
// no collector running, which would answer the poke, it returns at once.
func (h *handler) refresh(ctx context.Context, t *target) {
	if !h.collecting() {
		return
	}
	at, err := h.src.Control.Poke(t.key)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, fleet.SettleWait)
	defer cancel()
	tick := time.NewTicker(settlePoll)
	defer tick.Stop()
	for {
		if t.feed.Sync() == nil {
			if s := t.feed.Latest(); s != nil && s.At.After(at) {
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// conflict answers 409 with msg as the notice in the page's banner.
func conflict(w http.ResponseWriter, msg string) { notice(w, http.StatusConflict, msg) }

// notice answers status with msg as the notice in the page's banner.
func notice(w http.ResponseWriter, status int, msg string) {
	// htmx swaps error responses into #errors; the notice belongs in #banner.
	w.Header().Set("HX-Retarget", "#banner")
	w.Header().Set("HX-Reswap", "innerHTML")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
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

// scope is what an action on t reaches: its clients are made from t's
// newest snapshot, with the token of the config read for this request.
func (t *target) scope() (fleet.ActScope, error) {
	return fleet.ScopeOf(fleet.ClientsFor(t.latest, t.srv.Token), t.latest, t.label, t.noun)
}

// action runs pause, resume, restart or stop on one worker across the unit
// scope of the form. Restart is refused when the scope holds a unit behind
// a load balancer: it would reach whichever instance the balancer picks.
func (h *handler) action(w http.ResponseWriter, r *http.Request) {
	name, action := r.PathValue("name"), r.PathValue("action")
	if !slices.Contains(workerActions, action) {
		badRequest(w, "unknown action: want pause, resume, restart or stop")
		return
	}
	t, ok := h.bind(w, r)
	if !ok {
		return
	}
	sc, err := t.scope()
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	if action == "restart" && sc.ViaBalancer() {
		badRequest(w, fleet.RestartRefusedMsg)
		return
	}
	results := fleet.Act(mutationCtx(r), sc, fleet.UnitsOf(t.latest), t.noun, []string{name}, fleet.ActionSender(action))
	h.respond(w, r, t, name, results[0])
}

// concurrency sets one worker's per-unit concurrency to n across the unit
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
	t, ok := h.bind(w, r)
	if !ok {
		return
	}
	sc, err := t.scope()
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	current := make(map[string]int) // direct unit's endpoint (its key) → the worker's concurrency there
	for _, p := range t.latest.Pods {
		if smp := p.Workers[name]; smp != nil && !p.Balanced {
			current[p.Key] = smp.Concurrency
		}
	}
	results := fleet.Act(mutationCtx(r), sc, fleet.UnitsOf(t.latest), t.noun, []string{name}, fleet.Sender{
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
	h.respond(w, r, t, name, results[0])
}

// bulk pauses or resumes the workers named in the form across its unit
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
	t, ok := h.bind(w, r)
	if !ok {
		return
	}
	units := fleet.UnitsOf(t.latest)
	sc, err := t.scope()
	if err != nil {
		badRequest(w, err.Error())
		return
	}

	if r.PostFormValue("confirm") != "1" {
		cv := ConfirmView{Action: action, Server: t.key, Pod: t.label, Noun: t.noun, Rows: make([]ConfirmRow, len(names))}
		for i, name := range names {
			cv.Rows[i].Name = name
		}
		cols := slices.Concat(sc.Balanced, sc.Dead) // the units in scope, as act lists their results
		for _, c := range sc.Direct {
			cols = append(cols, units[c.PrimaryEndpoint()])
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

	perName := fleet.Act(mutationCtx(r), sc, units, t.noun, names, fleet.ActionSender(action))
	for i, res := range perName {
		for j := range res {
			res[j].Worker = names[i]
		}
	}
	h.respond(w, r, t, "", slices.Concat(perName...))
}

// respond answers an executed action: results, after poking the server
// and waiting up to fleet.SettleWait (less if the browser gives up) for a
// snapshot taken after the poke, which shows them, then the #live region
// of the page the request came from as an out-of-band swap.
//
// A refresh of a server whose newest snapshot is not current by then (no
// snapshot came, and the one held aged past the threshold, say; see
// store.Collector.Current) shows no current value, as a page does.
func (h *handler) respond(w http.ResponseWriter, r *http.Request, t *target, worker string, results []fleet.ActionResult) {
	if slices.ContainsFunc(results, func(res fleet.ActionResult) bool { return res.Outcome != "no change" }) {
		// The snapshot held is still the best there is if none comes.
		h.refresh(r.Context(), t)
	}
	snaps := t.feed.Snapshots()
	var stale string
	latest, now := latestOf(snaps), time.Now()
	switch d := h.collector(); {
	case d.Other:
		stale = fleet.OtherConfigMsg(d.Lease.PID)
	case latest != nil && !d.Current(latest, t.srv.PollInterval(), now):
		stale = h.staleMsg(t.key, latest, now)
	}
	frags := []fragment{{"results", "results", ResultsView{Results: results, Noun: t.noun}}}
	if set, p, ok := livePage(r, t.key, t.noun, t.srv.PollInterval(), snaps, t.label, worker, stale); ok {
		if p.Detail != nil {
			h.dayCharts(&p, t.feed, p.Detail.Name, latestOf(snaps).At)
		}
		frags = append(frags, fragment{set, "live_oob", p})
	}
	h.fragments(w, frags...)
}

// livePage is the page whose #live an action response refreshes, from
// snaps of the server keyed key, polled every interval: the one htmx says
// the request came from (HX-Current-URL), "/" being the dashboard and
// "/workers/{name}" that worker's detail; without the header, the page
// that carries the route's controls (detail of worker, or the dashboard).
// Other pages, a worker no unit reports, and a label the newest snapshot
// lacks get no refresh. stale is the page's Stale notice: with one, it
// shows no current value.
func livePage(r *http.Request, key string, n fleet.Noun, interval time.Duration, snaps []*fleet.Snapshot, label, worker, stale string) (string, Page, bool) {
	set := "dashboard"
	if worker != "" {
		set = "detail"
	}
	var ranges ChartRanges
	if from := r.Header.Get("HX-Current-URL"); from != "" {
		u, err := url.Parse(from)
		if err != nil {
			return "", Page{}, false
		}
		ranges = rangesOf(u.Query()) // the charts keep showing the span they did
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

	p := Page{Server: key, Pod: label, Noun: n, IntervalSec: int(interval / time.Second), Stale: stale, Ranges: ranges}
	fill(&p, snaps)
	if podGone(&p, snaps) {
		return "", Page{}, false // the next poll shows why
	}
	switch set {
	case "dashboard":
		d := fleet.BuildDashboard(snaps, p.Pod, 0, p.Noun)
		p.Dashboard = &d
	case "detail":
		d, ok := fleet.BuildDetail(snaps, p.Pod, worker, 0, p.Noun)
		if !ok {
			return "", Page{}, false
		}
		p.Detail = &d
		p.SelfURL = ranges.addTo(pageURL("/workers/"+url.PathEscape(worker), key, n.Param, label)) // for the charts' switches
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
