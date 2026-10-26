package web

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goptics/vartop/internal/api"
	"github.com/goptics/vartop/internal/config"
	"github.com/goptics/vartop/internal/fleet"
)

// Session is the active server's fleet.Collector and the History the pages
// read: its sink appends every snapshot to History, so a session's late
// tick can never land in another session's history.
type Session struct {
	Server   *config.Server
	Interval time.Duration
	History  *fleet.History

	pool      *api.Pool // the collector's, for Clients
	collector *fleet.Collector
}

// NewSession starts polling srv through pool, which the session now owns.
// The first poll starts immediately; each next one starts Interval after the
// previous one ended.
func NewSession(srv *config.Server, pool *api.Pool) *Session {
	interval := srv.PollInterval()
	hist := fleet.NewHistory(interval)
	return &Session{
		Server:   srv,
		Interval: interval,
		History:  hist,
		pool:     pool,
		collector: fleet.NewCollector(srv, pool, func(t fleet.Tick) error {
			// The collector rated it; Append rates it again against the
			// same samples, which leaves every rate as it is.
			hist.Append(t.Snap)
			return nil
		}),
	}
}

// Clients returns the pool's current clients, one per pod endpoint.
func (s *Session) Clients() []*api.Client { return s.pool.Clients() }

// PollNow asks for an immediate poll and waits for it: see
// fleet.Collector.PollNow. True means History holds a snapshot started after
// the call.
func (s *Session) PollNow(ctx context.Context) bool { return s.collector.PollNow(ctx) }

// Close stops polling and closes the pool. It does not wait for the poller:
// a tick in flight has its requests cancelled.
func (s *Session) Close() error { return s.collector.Close() }

// errManagerClosed is returned by Switch once the Manager is closed.
var errManagerClosed = errors.New("session manager closed")

// Manager holds the active Session and switches servers in the background,
// like the TUI's switchSeq: every Switch that starts a build supersedes any
// build still in flight, and only the latest build is applied. A failed
// build keeps the current session polling.
type Manager struct {
	cfg   *config.Config // read-only
	build func(*config.Server) (*api.Pool, error)
	cur   atomic.Pointer[Session]

	mu      sync.Mutex // guards the fields below; never held while building
	seq     uint64     // bumped by every switch; a build applies only if it is unchanged
	pending string     // server being connected, "" if none
	err     error      // last build error, cleared by a successful build
	failed  string     // server whose build set err, "" when err is nil
	active  string     // server of cur, "" when cur is nil; set with cur
	closed  bool
}

// NewManager returns a Manager with no session; Switch starts the first one.
// build makes a server's pool (api.PoolFor in production) and may be slow,
// e.g. a Kubernetes informer sync.
func NewManager(cfg *config.Config, build func(*config.Server) (*api.Pool, error)) *Manager {
	return &Manager{cfg: cfg, build: build}
}

// Switch starts connecting to the named server and returns without waiting
// for the build. It errors for a server not in the configuration and once
// the Manager is closed. Switching to the server being connected is a no-op;
// switching to the active one drops any build in flight and keeps the
// active session.
func (m *Manager) Switch(name string) error {
	srv := m.cfg.Servers[name]
	if srv == nil {
		return fmt.Errorf("server %q not found in configuration", name)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errManagerClosed
	}
	if name == m.pending {
		return nil
	}
	m.seq++
	if cur := m.cur.Load(); cur != nil && cur.Server == srv {
		m.pending = ""
		return nil
	}
	m.pending = name
	go m.connect(m.seq, srv)
	return nil
}

// connect builds srv's pool outside the lock, then applies it if no switch
// or Close happened since: on success the new session replaces the current
// one, which is closed; on failure the current one is kept. A superseded
// build's pool is closed and dropped.
func (m *Manager) connect(seq uint64, srv *config.Server) {
	pool, err := m.build(srv)

	m.mu.Lock()
	if seq != m.seq {
		m.mu.Unlock()
		if pool != nil {
			_ = pool.Close()
		}
		return
	}
	// seq is unchanged, so pending still names this build's server.
	name := m.pending
	m.pending = ""
	if err != nil {
		m.err, m.failed = err, name
		m.mu.Unlock()
		return
	}
	m.err, m.failed, m.active = nil, "", name
	old := m.cur.Swap(NewSession(srv, pool))
	m.mu.Unlock()

	if old != nil {
		_ = old.Close()
	}
}

// Current returns the active session: nil until the first build succeeds,
// and after Close. A session it returned may be closed by a later switch;
// its History stays readable.
func (m *Manager) Current() *Session { return m.cur.Load() }

// Active returns the active session and the name (configuration key) of its
// server, read together: "" and nil until the first build succeeds, and
// after Close. Server names are keys, never config.Server.Name, which the
// configuration does not keep equal to its key.
func (m *Manager) Active() (string, *Session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active, m.cur.Load()
}

// Pending returns the name of the server being connected, "" if none.
func (m *Manager) Pending() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pending
}

// Err returns the last build error, nil once a later build succeeds.
func (m *Manager) Err() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.err
}

// Failed returns the name of the server whose build failed with Err, "" when
// Err is nil.
func (m *Manager) Failed() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.failed
}

// Servers returns the configured server names, sorted.
func (m *Manager) Servers() []string { return slices.Sorted(maps.Keys(m.cfg.Servers)) }

// Close closes the current session. A build still in flight has its pool
// closed and dropped when it finishes, and Switch fails from now on. Close
// is idempotent.
func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	m.seq++
	m.pending, m.active = "", ""
	old := m.cur.Swap(nil)
	m.mu.Unlock()

	if old == nil {
		return nil
	}
	return old.Close()
}

// ConnectionLabel names how srv is reached, as the TUI header does: the
// kube context/namespace, the dns host:port, or its endpoint(s). Unlike the
// TUI, several endpoints are counted as instances ("2 instances"), not pods.
func ConnectionLabel(srv *config.Server) string {
	switch {
	case srv.Kubernetes != nil:
		return srv.Kubernetes.Context + "/" + srv.Kubernetes.Namespace
	case srv.DNS != nil:
		return fmt.Sprintf("dns:%s:%d", srv.DNS.Host, srv.DNS.Port)
	case len(srv.Endpoints) > 1:
		return fmt.Sprintf("%d %s", len(srv.Endpoints), fleet.NounFor(srv).Many)
	default:
		return srv.DisplayEndpoint()
	}
}
