// Package daemon is vartop's collector: the one process, holding the
// store's lease, that polls every configured server into the store for the
// UIs to read. It follows the config as servers are added, removed or
// changed, polls a server at once when a UI pokes it, and keeps the server
// files pruned.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/goptics/vartop/internal/api"
	"github.com/goptics/vartop/internal/config"
	"github.com/goptics/vartop/internal/store"
)

const (
	// PokeEvery is how often the daemon looks for pokes and stop requests.
	PokeEvery = 250 * time.Millisecond
	// MinPokeGap is the least time between two polls of a server that
	// pokes ask for.
	MinPokeGap = time.Second
	// ConfigEvery is how often the daemon looks for a change to the config.
	ConfigEvery = 2 * time.Second
	// PruneEvery is how often the server files are pruned.
	PruneEvery = 5 * time.Minute
	// RemovedGrace is how long the file of a server no longer configured is
	// kept after its last write: the retention, so that nothing a UI still
	// shows is lost, and the server put back meanwhile resumes its history.
	RemovedGrace = 24 * time.Hour
)

// Options are what Run collects with.
type Options struct {
	DataDir string // the store; store.DataDir() when ""
	// Config reads the config and says where its file is (with an error
	// too, when it can): Run reads it again whenever the file's mtime or
	// size changes.
	Config func() (*config.Config, string, error)
	// BuildPool makes a server's pool; api.PoolFor when nil. It may hang
	// (a Kubernetes credential plugin waiting for input): that holds up
	// its server only.
	BuildPool func(*config.Server) (*api.Pool, error)
	Lease     store.Lease      // what the lease is claimed as
	Now       func() time.Time // the clock of the lease, pruning and file removal; time.Now when nil
	Log       *slog.Logger     // state changes only; discarded when nil
}

// Run collects every configured server into the store until ctx ends, a
// stop is requested or another daemon takes the lease, and returns 0 for
// each. It returns 0 at once when another daemon holds a fresh lease, and
// a non-zero code with the error when control.db fails.
func Run(ctx context.Context, o Options) (code int, err error) {
	if o.DataDir == "" {
		if o.DataDir, err = store.DataDir(); err != nil {
			return 1, err
		}
	}
	if o.BuildPool == nil {
		o.BuildPool = api.PoolFor
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}

	ctl, err := store.OpenControl(o.DataDir)
	if err != nil {
		return 1, err
	}
	defer func() { _ = ctl.Close() }()
	token, ok, err := ctl.Claim(o.Lease, o.Now())
	if err != nil {
		return 1, fmt.Errorf("claim the lease: %w", err)
	}
	if !ok {
		o.Log.Info("another vartop daemon is collecting")
		return 0, nil
	}
	o.Log.Info("collecting", "pid", o.Lease.PID, "build", o.Lease.Build, "data", o.DataDir)

	d := &daemon{
		o:          o,
		ctl:        ctl,
		token:      token,
		conf:       configWatch{read: o.Config},
		collectors: make(map[string]*collector),
		done:       make(map[string]<-chan struct{}),
		exit:       make(chan exit, 1),
	}
	code, err = d.run(ctx)
	for _, c := range d.collectors {
		c.cancel()
	}
	for _, done := range d.done {
		<-done
	}
	if err := ctl.Release(token); err != nil {
		o.Log.Warn("cannot release the lease", "err", err)
	}
	o.Log.Info("stopped", "code", code)
	return code, err
}

// daemon is a Run holding the lease.
type daemon struct {
	o     Options
	ctl   *store.Control
	token string
	seen  stateLog // the daemon's own

	conf       configWatch
	collectors map[string]*collector      // by server name: the config's servers
	done       map[string]<-chan struct{} // by server name: the last collector's, stopped or not
	pokes      map[string]int64           // the poke counters last seen
	pruned     time.Time                  // when the files were last pruned, by o.Now
	exit       chan exit                  // why a collector ended the Run
}

// exit is how and why Run ends.
type exit struct {
	code int
	err  error
}

// end makes Run return code and err, and reports true, unless it is
// ending already.
func (d *daemon) end(code int, err error) bool {
	select {
	case d.exit <- exit{code, err}:
		return true
	default:
		return false
	}
}

// run is the daemon's loop: it renews the lease, passes pokes on, follows
// the config and has the files pruned, until it has to stop. It never
// waits on a server.
func (d *daemon) run(ctx context.Context) (int, error) {
	pokes, err := d.ctl.Pokes()
	if err != nil {
		return 1, fmt.Errorf("read pokes: %w", err)
	}
	d.pokes = pokes // a poke from before the start is no poke for this daemon
	d.reload()

	beat := time.NewTicker(store.BeatEvery)
	defer beat.Stop()
	poke := time.NewTicker(PokeEvery)
	defer poke.Stop()
	conf := time.NewTicker(ConfigEvery)
	defer conf.Stop()
	for {
		select {
		case <-ctx.Done():
			return 0, nil
		case e := <-d.exit:
			return e.code, e.err
		case <-beat.C:
			stop, err := d.ctl.Beat(d.token, d.o.Now())
			switch {
			case errors.Is(err, store.ErrLeaseLost):
				d.o.Log.Warn("lease lost: another daemon collects")
				return 0, nil
			case err != nil:
				return 1, fmt.Errorf("renew the lease: %w", err)
			case stop:
				d.o.Log.Info("stop requested")
				return 0, nil
			}
		case <-poke.C:
			// Beat tells too, but up to BeatEvery late.
			if l, _, err := d.ctl.Lease(); err == nil {
				switch {
				case l.Token != d.token:
					d.o.Log.Warn("lease lost: another daemon collects")
					return 0, nil
				case l.Stop > 0:
					d.o.Log.Info("stop requested")
					return 0, nil
				}
			}
			d.poke()
		case <-conf.C:
			d.reload()
			d.maintain()
		}
	}
}

// poke has every server whose poke counter moved polled.
func (d *daemon) poke() {
	pokes, err := d.ctl.Pokes()
	if d.seen.changed("pokes", errString(err)) && err != nil {
		d.o.Log.Warn("cannot read pokes", "err", err)
	}
	for name, n := range pokes {
		if n == d.pokes[name] {
			continue
		}
		d.pokes[name] = n
		if c := d.collectors[name]; c != nil {
			select {
			case c.poke <- struct{}{}:
			default: // one is pending: it answers this one too
			}
		}
	}
}

// maintain, once every PruneEvery by the daemon's clock, has every server
// file pruned and removes the files of servers absent from the config for
// RemovedGrace. Before a config was read, no file is taken for a removed
// server's.
func (d *daemon) maintain() {
	now := d.o.Now()
	if since := now.Sub(d.pruned); since >= 0 && since < PruneEvery {
		return
	}
	d.pruned = now
	for _, c := range d.collectors {
		select {
		case c.prune <- now:
		default:
		}
	}
	if d.conf.good == nil {
		return
	}
	removed, err := store.RemoveStale(d.o.DataDir, slices.Collect(maps.Keys(d.conf.good.Servers)), now.Add(-RemovedGrace))
	for _, path := range removed {
		d.o.Log.Info("removed the file of a server no longer configured", "file", path)
	}
	if d.seen.changed("sweep", errString(err)) && err != nil {
		d.o.Log.Warn("cannot remove the file of a server no longer configured", "err", err)
	}
}

// stateLog remembers what was logged under each key, so that only changes
// are logged. It is safe for concurrent use.
type stateLog struct {
	mu    sync.Mutex
	state map[string]string
}

// changed records state under key, and reports whether it differs from the
// state recorded before ("" at first).
func (l *stateLog) changed(key, state string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state[key] == state {
		return false
	}
	if l.state == nil {
		l.state = make(map[string]string)
	}
	l.state[key] = state
	return true
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
