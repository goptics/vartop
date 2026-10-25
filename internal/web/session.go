package web

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/goptics/vartop/internal/api"
	"github.com/goptics/vartop/internal/config"
)

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
	m.err, m.failed = nil, ""
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
	m.pending = ""
	old := m.cur.Swap(nil)
	m.mu.Unlock()

	if old == nil {
		return nil
	}
	return old.Close()
}

// ConnectionLabel names how srv is reached, as the TUI header does: the
// kube context/namespace, the dns host:port, or its endpoint(s).
func ConnectionLabel(srv *config.Server) string {
	switch {
	case srv.Kubernetes != nil:
		return srv.Kubernetes.Context + "/" + srv.Kubernetes.Namespace
	case srv.DNS != nil:
		return fmt.Sprintf("dns:%s:%d", srv.DNS.Host, srv.DNS.Port)
	default:
		return srv.DisplayEndpoint()
	}
}
