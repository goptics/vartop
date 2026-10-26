// Package web serves vartop's browser UI: the HTTP handlers and templates
// that render what the vartop daemon collected into the store, server by
// server as each request names it, and send worker actions to their units.
package web

import (
	"bytes"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"maps"
	"net"
	"net/http"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/goptics/vartop/internal/config"
	"github.com/goptics/vartop/internal/fleet"
	"github.com/goptics/vartop/internal/store"
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

// Source is what vartop web shows and acts through.
type Source struct {
	DataDir string // the store the daemon collects into
	// Config reads the config. It is called on every request: the servers
	// can change while vartop web runs, as they do for the daemon.
	Config  func() (*config.Config, error)
	Control *store.Control // the daemon's lease, the servers' errors and pokes
	Start   func() error   // starts the daemon (ensureDaemon); the "Start collector" button
}

// handler serves the pages of every configured server from the store.
// Pages only read it; only the POST routes in actions.go call varmq.
type handler struct {
	src   Source
	tmpl  map[string]*template.Template
	feeds *feedCache
	logf  func(format string, args ...any) // where a recovered panic is logged

	mu   sync.Mutex
	good *config.Config // the last config read without an error
}

// NewHandler returns the web UI over src. Guards, outermost first: the
// Host allowlist (every method), cross-origin protection, and
// "HX-Request: true" on every method but GET and HEAD; each refuses with
// 403. The POST routes that change state are registered here so every
// guard wraps them. A handler that panics answers 500, and the rest serve
// on.
func NewHandler(src Source, hosts []string) (http.Handler, error) {
	h, err := newHandler(src)
	if err != nil {
		return nil, err
	}
	return h.routes(hosts), nil
}

func newHandler(src Source) (*handler, error) {
	tmpl, err := parseTemplates()
	if err != nil {
		return nil, err
	}
	return &handler{src: src, tmpl: tmpl, feeds: newFeedCache(src.DataDir), logf: log.Printf}, nil
}

// routes is h behind the guards (see NewHandler).
func (h *handler) routes(hosts []string) http.Handler {
	static, err := fs.Sub(assets, "static")
	if err != nil {
		panic(err) // static is embedded: it is always there
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", h.dashboard)
	mux.HandleFunc("GET /grid", h.grid)
	mux.HandleFunc("GET /workers/{name}", h.detail)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("POST /workers/{name}/actions/{action}", h.action)
	mux.HandleFunc("POST /workers/{name}/concurrency", h.concurrency)
	mux.HandleFunc("POST /bulk/{action}", h.bulk)
	mux.HandleFunc("POST /daemon/start", h.startDaemon)

	return denyFraming(allowHosts(hosts, http.NewCrossOriginProtection().Handler(requireHTMX(recoverPanics(h.logf, mux)))))
}

// close closes the server files h reads.
func (h *handler) close() { h.feeds.close() }

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

// recoverPanics answers a request whose handler panicked with a 500 naming
// the panic, and logs it with the stack it was raised on, through logf.
// net/http would recover it too, but only to drop the connection: the
// browser would show nothing. Handlers render into a buffer before they
// write, so nothing was sent yet.
func recoverPanics(logf func(format string, args ...any), next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if v == http.ErrAbortHandler {
				panic(v)
			}
			p, ok := v.(*fleet.TickPanic) // raised again from a goroutine, with its own stack
			if !ok {
				p = &fleet.TickPanic{Value: v, Stack: debug.Stack()}
			}
			logf("vartop web: %s %s: %v", r.Method, r.URL.Path, p)
			// Escaped: htmx inserts an error response into #errors as HTML.
			http.Error(w, template.HTMLEscapeString(fmt.Sprintf("internal error: %v", p.Value)), http.StatusInternalServerError)
		}()
		next.ServeHTTP(w, r)
	})
}

func isHTMX(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

// The copy of what a page or an action cannot show or reach.
func waitingMsg(server string) string       { return "Waiting for the first snapshot of " + server + "…" }
func notConfiguredMsg(server string) string { return "Server " + server + " is not configured" }

// noServersMsg is the notice of a config with no server at all.
const noServersMsg = "No servers are configured"

// unknownLabelMsg is the notice of a label no snapshot of the hour has.
func unknownLabelMsg(label, server string, n fleet.Noun) string {
	return label + " is not " + n.Article + " " + n.One + " of " + server
}

// goneLabelMsg is the notice of a label only older snapshots have.
func goneLabelMsg(label string, n fleet.Noun) string {
	return n.Cap() + " " + label + " is no longer reported — reload"
}

// staleAfter is how old the newest snapshot of a server polled every
// interval may get before the pages call the server stale: two polls
// missed, and 2s of slack.
func staleAfter(interval time.Duration) time.Duration { return 2*interval + 2*time.Second }

// isStale reports whether latest, of a server polled every interval, is
// stale at now.
func isStale(latest *fleet.Snapshot, interval time.Duration, now time.Time) bool {
	return latest != nil && now.Sub(latest.At) > staleAfter(interval)
}

func (h *handler) dashboard(w http.ResponseWriter, r *http.Request) {
	p, snaps, ok := h.page(r)
	p.Title = pageTitle("vartop", p.Server)
	if ok && !podGone(&p, snaps) {
		d := fleet.BuildDashboard(snaps, p.Pod, 0, p.Noun)
		p.Dashboard = &d
		podDown(&p, snaps) // its banner stands in for "No workers reported yet."
	}
	h.render(w, "dashboard", p.status(r), p)
}

func (h *handler) grid(w http.ResponseWriter, r *http.Request) {
	p, snaps, ok := h.page(r)
	p.Title = pageTitle(p.Noun.Cap()+" grid", p.Server)
	if ok {
		g := fleet.BuildGrid(latestOf(snaps))
		p.Grid = &g
	}
	p.Pods = nil // the grid shows every unit: no unit select
	h.render(w, "grid", p.status(r), p)
}

// detail is 404 when no selected unit reports the worker, except to htmx: a
// poll gets a 200 so the notice reaches the banner (htmx would swap an error
// response into #errors, through the poller's #live filter, as nothing). A
// selected unit that is down is not a missing worker: its error is the
// banner, with a 200.
func (h *handler) detail(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	p, snaps, ok := h.page(r)
	p.Title = pageTitle(name, p.Server)
	status := p.status(r)
	if ok {
		if podGone(&p, snaps) || podDown(&p, snaps) {
			// p says why; no unit's data is shown under the gone one's label.
		} else if d, found := fleet.BuildDetail(snaps, p.Pod, name, 0, p.Noun); found {
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

// page starts the Page for r: the server it names (its server parameter,
// else the config's default) and the unit label it names in that server's
// noun's parameter. It syncs the server's feed and returns its snapshots,
// oldest first, and whether the page shows them; when it does not, the
// Page says why: the config cannot be read, the server is not configured,
// or it has no snapshot yet. A stale newest snapshot is shown, with a
// notice saying how old it is, and why.
//
// A poll swaps in only #live, #banner and #status, so to htmx the
// waiting message is sent as the Banner instead.
func (h *handler) page(r *http.Request) (p Page, snaps []*fleet.Snapshot, ok bool) {
	p = Page{SelfURL: r.URL.RequestURI(), Health: fleet.Health(nil), Noun: fleet.NounFor(nil)}
	defer func() {
		if isHTMX(r) && p.Waiting != "" {
			p.Banner, p.Waiting = p.Waiting, ""
		}
	}()
	cfg, err := h.config()
	if err != nil {
		p.Banner = "cannot read the config: " + err.Error()
		return p, nil, false
	}
	p.Servers = slices.Sorted(maps.Keys(cfg.Servers))
	p.Server = serverOf(cfg, r.FormValue("server"))
	srv := cfg.Servers[p.Server]
	if srv == nil {
		p.Unconfigured = p.Server != ""
		p.Banner = notConfiguredMsg(p.Server)
		if p.Server == "" {
			p.Banner = noServersMsg
		}
		return p, nil, false
	}
	p.Noun = fleet.NounFor(srv)
	p.Pod = labelOf(srv, r.FormValue(p.Noun.Param))
	// The poller asks for what this page shows, its server and unit named,
	// whatever the URL left out or carried besides; the path stays escaped.
	p.SelfURL = pageURL(r.URL.EscapedPath(), p.Server, p.Noun.Param, p.Pod)
	interval := srv.PollInterval()
	p.IntervalSec = int(interval / time.Second)

	feed := h.feeds.get(p.Server, interval, time.Now())
	if err := feed.Sync(); err != nil {
		p.Banner = "cannot read the history of " + p.Server + ": " + err.Error()
	}
	snaps = feed.Snapshots()
	latest := latestOf(snaps)
	fill(&p, snaps)
	switch now := time.Now(); {
	case latest == nil:
		p.Waiting = waitingMsg(p.Server)
		p.DaemonDown = !h.daemonRunning()
		if msg := h.serverErr(p.Server, nil); msg != "" && !p.DaemonDown {
			p.Banner = p.Server + ": " + msg
		}
		return p, nil, false
	case isStale(latest, interval, now):
		p.Stale, p.DaemonDown = h.staleMsg(p.Server, latest, now)
	case latest.Err != "":
		p.Banner = p.Server + ": " + latest.Err
	}
	return p, snaps, true
}

// status is the status of a page whose server cannot be shown because it
// is not configured: 404, but to htmx, whose poll would then drop the
// notice; 200 otherwise.
func (p Page) status(r *http.Request) int {
	if p.Unconfigured && !isHTMX(r) {
		return http.StatusNotFound
	}
	return http.StatusOK
}

// config reads the config. One that cannot be read (half written, say)
// gives way to the last one read, as the daemon's does; with none read
// yet, the error is returned.
func (h *handler) config() (*config.Config, error) {
	cfg, err := h.src.Config()
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case err == nil && cfg != nil:
		h.good = cfg
		return cfg, nil
	case h.good != nil:
		return h.good, nil
	case err == nil:
		err = config.ErrConfigNotFound
	}
	return nil, err
}

// serverOf is the key of the server a request names by want: want itself,
// else the config's default server, else the first configured one.
func serverOf(cfg *config.Config, want string) string {
	switch {
	case want != "":
		return want
	case cfg.DefaultServer != "":
		return cfg.DefaultServer
	}
	if keys := slices.Sorted(maps.Keys(cfg.Servers)); len(keys) > 0 {
		return keys[0]
	}
	return ""
}

// labelOf is the unit label value names on srv: value itself, but on
// Kubernetes a value shaped like a unit key — "#" and an instance ID, or a
// URL, what pod= held before labels — which no pod name is: then none, so
// that an old link shows the merged view.
func labelOf(srv *config.Server, value string) string {
	if srv.Kubernetes != nil && (strings.HasPrefix(value, "#") || strings.Contains(value, "://")) {
		return ""
	}
	return value
}

// daemonRunning reports whether a daemon holds a fresh lease. A lease that
// cannot be read counts as held: nothing says the daemon is down.
func (h *handler) daemonRunning() bool {
	_, fresh, err := h.src.Control.Lease()
	return fresh || err != nil
}

// serverErr is the collection error the daemon recorded for the server
// keyed key, else latest's, else "".
func (h *handler) serverErr(key string, latest *fleet.Snapshot) string {
	if errs, err := h.src.Control.ServerErrs(); err == nil && errs[key] != "" {
		return errs[key]
	}
	if latest != nil {
		return latest.Err
	}
	return ""
}

// staleMsg says how old latest, the stale newest snapshot of the server
// keyed key, is at now, and why there is none newer: the daemon is not
// running (down), or the server's collection error. With neither known it
// says only how old.
func (h *handler) staleMsg(key string, latest *fleet.Snapshot, now time.Time) (msg string, down bool) {
	msg = "No new data for " + now.Sub(latest.At).Round(time.Second).String()
	switch {
	case !h.daemonRunning():
		return msg + " — the vartop daemon is not running.", true
	default:
		if err := h.serverErr(key, latest); err != "" {
			return msg + " — " + key + ": " + err, false
		}
	}
	return msg + ".", false
}

// fill sets p's header, its unit options and whether its scope holds a
// balanced unit, from snaps, oldest first. It keeps p.Pod even when the
// newest snapshot lacks it: see podGone.
func fill(p *Page, snaps []*fleet.Snapshot) {
	latest := latestOf(snaps)
	p.Health = fleet.Health(latest)
	p.Note = fleet.BalancerNote(latest)

	p.Pods, p.Updated = nil, ""
	if latest != nil {
		p.Updated = fleet.PointTime(latest)
		for _, ps := range latest.Pods {
			p.Pods = append(p.Pods, PodOption{Label: ps.Label, Tip: ps.Tip()})
		}
	}
	p.ScopeBalanced = slices.ContainsFunc(fleet.PodsOf(latest, p.Pod), func(ps fleet.PodSnapshot) bool { return ps.Balanced })
	p.OneBalanced = p.Pod != "" && p.ScopeBalanced
}

// missingLabel says why label names no unit of the newest of snaps, of the
// server keyed server: no longer reported, when an older snapshot had it,
// else not a unit of the server. It is "" when the newest has it.
func missingLabel(snaps []*fleet.Snapshot, label, server string, n fleet.Noun) string {
	if fleet.PodsOf(latestOf(snaps), label) != nil {
		return ""
	}
	if slices.ContainsFunc(snaps, func(s *fleet.Snapshot) bool { return fleet.PodsOf(s, label) != nil }) {
		return goneLabelMsg(label, n)
	}
	return unknownLabelMsg(label, server, n)
}

// podGone reports whether p.Pod labels no unit of the newest of snaps: one
// that left discovery, or one never seen (a stale bookmark). p then gets
// the banner saying which (see missingLabel), and the label as Gone.
// Callers must then show no data: another unit's, under this one's label,
// would mislead. SelfURL keeps the label, so every poll says the same, and
// shows the unit's data again should it come back. With no snapshot yet
// nothing is known, and a newest snapshot that is the server's error
// holds no unit: neither makes a unit gone.
func podGone(p *Page, snaps []*fleet.Snapshot) bool {
	latest := latestOf(snaps)
	if p.Pod == "" || latest == nil || latest.Err != "" {
		return false
	}
	msg := missingLabel(snaps, p.Pod, p.Server, p.Noun)
	if msg == "" {
		return false
	}
	p.Banner, p.Gone = msg, p.Pod
	return true
}

// podDown reports whether p.Pod labels a unit that is errored in the newest
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
	p.Banner = p.Pod + ": " + pods[0].Err
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

// feedIdle is how long a server's feed no request read stays open.
const feedIdle = 10 * time.Minute

// feedCache keeps a feed per server and poll interval, so that only the
// first request pays for loading the hour (see store.Feed.Sync): a server
// whose interval changed gets a new one. A feed no request read for
// feedIdle is closed; it lets its file go.
type feedCache struct {
	dataDir string

	mu    sync.Mutex
	feeds map[feedKey]*cachedFeed
}

type feedKey struct {
	server   string
	interval time.Duration
}

type cachedFeed struct {
	feed *store.Feed
	used time.Time
}

func newFeedCache(dataDir string) *feedCache {
	return &feedCache{dataDir: dataDir, feeds: make(map[feedKey]*cachedFeed)}
}

// get returns the feed of the server keyed server, polled every interval,
// as read at now, first closing the feeds idle for feedIdle.
func (c *feedCache) get(server string, interval time.Duration, now time.Time) *store.Feed {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, cf := range c.feeds {
		if now.Sub(cf.used) > feedIdle {
			_ = cf.feed.Close()
			delete(c.feeds, k)
		}
	}
	k := feedKey{server, interval}
	cf := c.feeds[k]
	if cf == nil {
		cf = &cachedFeed{feed: store.NewFeed(c.dataDir, server, interval)}
		c.feeds[k] = cf
	}
	cf.used = now
	return cf.feed
}

// close closes every feed.
func (c *feedCache) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, cf := range c.feeds {
		_ = cf.feed.Close()
		delete(c.feeds, k)
	}
}
