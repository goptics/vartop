// Package web serves vartop's browser UI: the HTTP handlers and templates
// that render the active server's history, which a fleet.Collector fills,
// and send worker actions to its units.
package web

import (
	"bytes"
	"cmp"
	"fmt"
	"html/template"
	"io/fs"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/goptics/vartop/internal/fleet"
)

// Listen listens on addr, which must be loopback only: its host must be
// given and every IP it resolves to must be a loopback address. It listens
// on the literal IP (IPv4 preferred), so the name is never resolved again.
// Errors name addr.
func Listen(addr string) (net.Listener, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", addr, err)
	}
	if host == "" {
		return nil, fmt.Errorf("listen on %s: no host given; vartop web listens on loopback only, e.g. 127.0.0.1:7070", addr)
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", addr, err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("listen on %s: %s resolves to no address", addr, host)
	}
	for _, ip := range ips {
		if !ip.IsLoopback() {
			return nil, fmt.Errorf("listen on %s: %s is not a loopback address; vartop web listens on loopback only", addr, ip)
		}
	}
	ip := ips[0]
	if i := slices.IndexFunc(ips, func(c net.IP) bool { return c.To4() != nil }); i >= 0 {
		ip = ips[i]
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(ip.String(), port))
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", addr, err)
	}
	return ln, nil
}

// AllowedHosts is the Host allowlist for a listener from Listen:
// 127.0.0.1, localhost and the bound IP ([::1] for IPv6), each with its port.
func AllowedHosts(ln net.Listener) []string {
	ip, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		return nil
	}
	hosts := []string{net.JoinHostPort("127.0.0.1", port), net.JoinHostPort("localhost", port)}
	if self := net.JoinHostPort(ip, port); !slices.Contains(hosts, self) {
		hosts = append(hosts, self)
	}
	return hosts
}

// handler serves the pages of the active session. Pages read only the
// session's History; only the POST routes in actions.go call varmq.
type handler struct {
	m    *Manager
	tmpl map[string]*template.Template
}

// NewHandler returns the web UI for m. Guards, outermost first: the Host
// allowlist (every method), cross-origin protection, and "HX-Request: true"
// on every method but GET and HEAD; each refuses with 403. The POST routes
// that change state are registered here so every guard wraps them.
func NewHandler(m *Manager, hosts []string) (http.Handler, error) {
	tmpl, err := parseTemplates()
	if err != nil {
		return nil, err
	}
	static, err := fs.Sub(assets, "static")
	if err != nil {
		return nil, err
	}
	h := &handler{m: m, tmpl: tmpl}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", h.dashboard)
	mux.HandleFunc("GET /grid", h.grid)
	mux.HandleFunc("GET /workers/{name}", h.detail)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("POST /workers/{name}/actions/{action}", h.action)
	mux.HandleFunc("POST /workers/{name}/concurrency", h.concurrency)
	mux.HandleFunc("POST /bulk/{action}", h.bulk)
	mux.HandleFunc("POST /server", h.switchServer)

	return denyFraming(allowHosts(hosts, http.NewCrossOriginProtection().Handler(requireHTMX(mux)))), nil
}

// allowHosts refuses a request whose Host is not one of hosts, ignoring
// case. It guards GETs too: a DNS-rebinding page reaches loopback under its
// own host name and could otherwise read the dashboard.
func allowHosts(hosts []string, next http.Handler) http.Handler {
	allowed := make(map[string]bool, len(hosts))
	for _, h := range hosts {
		allowed[strings.ToLower(h)] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := strings.ToLower(r.Host)
		if _, _, err := net.SplitHostPort(host); err != nil {
			host += ":80" // browsers leave the default port out
		}
		if !allowed[host] {
			http.Error(w, "forbidden: unexpected Host", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireHTMX refuses any request but a GET or HEAD without "HX-Request:
// true". A form on another site cannot set that header, and a script would
// need a CORS preflight, which this server never answers.
func requireHTMX(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !isHTMX(r) {
			http.Error(w, "forbidden: not an htmx request", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// denyFraming forbids other sites from framing the UI, where they could
// trick clicks onto its controls.
func denyFraming(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

func isHTMX(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

// serverChanged is the notice for a request bound to a server other than
// the active one.
func serverChanged(active string) string { return "Server changed to " + active + " — reload" }

func (h *handler) dashboard(w http.ResponseWriter, r *http.Request) {
	p, s := h.page(r)
	p.Title = pageTitle("vartop", p.Server)
	if s != nil {
		if snaps := fill(&p, s.History); !podGone(&p, snaps) {
			d := fleet.BuildDashboard(snaps, p.Pod, s.History.Interval(), p.Noun)
			p.Dashboard = &d
			podDown(&p, snaps) // its banner stands in for "No workers reported yet."
		}
	}
	h.render(w, "dashboard", http.StatusOK, p)
}

func (h *handler) grid(w http.ResponseWriter, r *http.Request) {
	p, s := h.page(r)
	p.Title = pageTitle(p.Noun.Cap()+" grid", p.Server)
	if s != nil {
		g := fleet.BuildGrid(latestOf(fill(&p, s.History)))
		p.Grid = &g
	}
	p.Pods = nil // the grid shows every pod: no pod select
	h.render(w, "grid", http.StatusOK, p)
}

// detail is 404 when no selected pod reports the worker, except to htmx: a
// poll gets a 200 so the notice reaches the banner (htmx would swap an error
// response into #errors, through the poller's #live filter, as nothing). A
// selected pod that is down is not a missing worker: its error is the
// banner, with a 200.
func (h *handler) detail(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	p, s := h.page(r)
	p.Title = pageTitle(name, p.Server)
	status := http.StatusOK
	if s != nil {
		snaps := fill(&p, s.History)
		if podGone(&p, snaps) || podDown(&p, snaps) {
			// p says why; no pod's data is shown under the gone one's name.
		} else if d, ok := fleet.BuildDetail(snaps, p.Pod, name, s.History.Interval(), p.Noun); ok {
			p.Detail = &d
		} else {
			where := "any " + p.Noun.One
			if p.Pod != "" {
				where = p.Scope()
			}
			p.Banner = fmt.Sprintf("Worker %q not found on %s", name, where)
			if !isHTMX(r) {
				status = http.StatusNotFound
			}
		}
	}
	h.render(w, "detail", status, p)
}

// pageTitle is a page's document title: what it shows, then its server.
func pageTitle(what, server string) string {
	if server == "" {
		return what
	}
	return what + " · " + server
}

// page starts the Page for r. It returns the active session when the page
// should show its data; otherwise the Page already says why not:
//
//   - r's server is being connected, or nothing is active yet and a server
//     is being connected: Connecting.
//   - there is no session, or r's server is the one whose build failed: Err,
//     with a Retry form for that server.
//   - r's server is not the active one: the stale-tab Banner, with the
//     header (health, pods, interval) of the active server.
//
// A poll swaps in only #live, #banner and #status, so to htmx the
// connecting or error message is sent as the Banner instead.
func (h *handler) page(r *http.Request) (Page, *Session) {
	p := Page{Servers: h.m.Servers(), SelfURL: r.URL.RequestURI(), Health: fleet.Health(nil)}
	want := r.FormValue("server") // a configuration key, like every server name here
	// Pending before Active: connect clears pending and swaps the session
	// under one lock, so this order never sees the old session with nothing
	// pending mid-switch, which would show a stale banner for a moment.
	pending := h.m.Pending()
	active, cur := h.m.Active()

	switch {
	case pending != "" && (want == pending || cur == nil):
		p.Server = pending
		p.Connecting = "Connecting to " + ConnectionLabel(h.m.cfg.Servers[pending]) + "…"
	case cur == nil:
		h.failure(&p, want)
	case want == "" || want == active:
		p.Server = active
		p.Pod = r.FormValue("pod")
		if want == "" {
			// Bind the poller to the server shown; the path stays escaped.
			u := *r.URL
			q := u.Query()
			q.Set("server", p.Server)
			u.RawQuery = q.Encode()
			p.SelfURL = u.RequestURI()
		}
		p.Noun = fleet.NounFor(cur.Server)
		return p, cur
	case want == h.m.Failed():
		h.failure(&p, want)
	default:
		p.Server = active
		p.Banner = serverChanged(active)
		fill(&p, cur.History)
	}

	p.Noun = fleet.NounFor(h.m.cfg.Servers[p.Server])
	if isHTMX(r) && (p.Connecting != "" || p.Err != "") {
		p.Banner = cmp.Or(p.Connecting, p.Err)
		p.Connecting, p.Err = "", ""
	}
	return p, nil
}

// failure sets p to the last build failure: its error, and the server to
// retry (want if no build has failed).
func (h *handler) failure(p *Page, want string) {
	p.Server = cmp.Or(h.m.Failed(), want)
	p.Err = "not connected"
	if err := h.m.Err(); err != nil {
		p.Err = err.Error()
	}
}

// fill sets p's header, and whether its pod scope holds a balanced unit,
// from hist and returns hist's snapshots, oldest first. It keeps p.Pod even
// when the newest snapshot lacks it: see podGone.
func fill(p *Page, hist *fleet.History) []*fleet.Snapshot {
	snaps := hist.Snapshots()
	latest := latestOf(snaps)
	p.IntervalSec = int(hist.Interval() / time.Second)
	p.Health = fleet.Health(latest)
	p.Note = fleet.BalancerNote(latest)

	p.Pods, p.Updated = nil, ""
	if latest != nil {
		p.Updated = fleet.PointTime(latest)
		for _, ps := range latest.Pods {
			p.Pods = append(p.Pods, PodOption{Key: ps.Key, Label: ps.Label, Tip: ps.Tip()})
		}
	}
	p.ScopeBalanced = slices.ContainsFunc(fleet.PodsOf(latest, p.Pod), func(ps fleet.PodSnapshot) bool { return ps.Balanced })
	p.OneBalanced = p.Pod != "" && p.ScopeBalanced
	return snaps
}

// podGone reports whether p.Pod names a pod the newest of snaps lacks: one
// that left discovery, or one never seen (a stale bookmark). p then gets the
// banner saying so, naming the pod by the label of the last snapshot that
// had it, and that label as Gone. Callers must then show no data: another
// pod's, under this pod's name, would mislead. SelfURL keeps the pod, so
// every poll says the same, and shows the pod's data again should it come
// back. With no snapshot yet nothing is known, so nothing is gone.
func podGone(p *Page, snaps []*fleet.Snapshot) bool {
	latest := latestOf(snaps)
	if p.Pod == "" || latest == nil || fleet.PodsOf(latest, p.Pod) != nil {
		return false
	}
	label := p.Pod
	for _, s := range slices.Backward(snaps) {
		if seen := fleet.PodsOf(s, p.Pod); seen != nil {
			label = seen[0].Label
			break
		}
	}
	p.Banner = p.Noun.Cap() + " " + label + " is no longer reported — reload"
	p.Gone = label
	return true
}

// podDown reports whether p.Pod names a pod that is errored in the newest
// of snaps, and then puts its label and error in p's banner: the page has
// none of its data to show, and the banner says why.
func podDown(p *Page, snaps []*fleet.Snapshot) bool {
	if p.Pod == "" {
		return false
	}
	pods := fleet.PodsOf(latestOf(snaps), p.Pod)
	if len(pods) == 0 || pods[0].Err == "" {
		return false
	}
	p.Banner = cmp.Or(pods[0].Label, p.Pod) + ": " + pods[0].Err
	return true
}

// latestOf is the newest of snaps, nil if there are none.
func latestOf(snaps []*fleet.Snapshot) *fleet.Snapshot {
	if len(snaps) == 0 {
		return nil
	}
	return snaps[len(snaps)-1]
}

// render writes the layout of the page set for p with status. It renders
// into a buffer first so a template error never leaves half a page.
func (h *handler) render(w http.ResponseWriter, set string, status int, p Page) {
	var buf bytes.Buffer
	if err := h.tmpl[set].ExecuteTemplate(&buf, "layout", p); err != nil {
		// Escaped: htmx inserts an error response into #errors as HTML.
		http.Error(w, template.HTMLEscapeString("render "+set+": "+err.Error()), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}
