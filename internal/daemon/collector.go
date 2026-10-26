package daemon

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goptics/vartop/internal/api"
	"github.com/goptics/vartop/internal/config"
	"github.com/goptics/vartop/internal/fleet"
	"github.com/goptics/vartop/internal/store"
)

const (
	// queueLen is how many ticks wait for a server's writer; past it,
	// ticks are dropped.
	queueLen = 8
	// firstRetry and maxRetry bound the backoff of a server whose pool or
	// file cannot be had: 5s, doubling to 5min.
	firstRetry = 5 * time.Second
	maxRetry   = 5 * time.Minute
)

// internalError is the error of the snapshot a tick that panicked stores.
const internalError = "internal error (see daemon log)"

// collector collects one configured server: it opens the server's file,
// builds its pool and runs a fleet.Collector over it, whose ticks a writer
// goroutine appends to the file. Every goroutine it starts ends once it is
// cancelled, but for a pool build, which cannot be: that one closes its
// pool when it ends.
type collector struct {
	d      *daemon
	name   string
	srv    *config.Server
	enc    string             // srv as JSON: what tells an edit
	cancel context.CancelFunc // stops the collector
	done   chan struct{}      // closed once its file is closed

	poke  chan struct{}  // a poll asked for; one pending at most
	prune chan time.Time // a prune asked for, at that time
	queue chan fleet.Tick
	// labels is the label state of the newest tick dropped since a tick
	// that changed it was queued: newer than any queued tick's, so the
	// writer saves it with the next tick it writes.
	labels atomic.Pointer[fleet.LabelState]
	seen   stateLog
}

// start starts collecting srv, named name and encoded as enc, after delay.
// A collector of the same name stopped before lets go of the file first.
func (d *daemon) start(name string, srv *config.Server, enc string, delay time.Duration) {
	ctx, cancel := context.WithCancel(context.Background())
	c := &collector{
		d: d, name: name, srv: srv, enc: enc, cancel: cancel,
		done:  make(chan struct{}),
		poke:  make(chan struct{}, 1),
		prune: make(chan time.Time, 1),
		queue: make(chan fleet.Tick, queueLen),
	}
	prev := d.done[name]
	d.collectors[name], d.done[name] = c, c.done
	go c.run(ctx, prev, delay)
}

// run collects until ctx is cancelled. It waits for prev, the collector of
// the same name stopped before, to let go of the file, and for delay; then
// it opens the file, starts the writer, builds the pool and polls.
func (c *collector) run(ctx context.Context, prev <-chan struct{}, delay time.Duration) {
	defer close(c.done)
	if prev != nil {
		<-prev // cancelled: it ends at once
	}
	if !sleep(ctx, delay) {
		return
	}
	db, init, ok := c.open(ctx)
	if !ok {
		return
	}
	var wg sync.WaitGroup
	defer wg.Wait()
	w := &writer{c: c, db: db}
	wg.Go(func() { w.run(ctx) })

	pool := c.connect(ctx)
	if pool == nil {
		return
	}
	fc := fleet.NewCollector(c.srv, pool, init, c.sink)
	defer func() { _ = fc.Close() }()
	c.serve(ctx, fc)
}

// open opens the server's file and reads the state its collector starts
// from, retrying with backoff while that fails. ok is false once ctx is
// cancelled.
func (c *collector) open(ctx context.Context) (*store.ServerDB, fleet.CollectorInit, bool) {
	for wait := firstRetry; ; wait = min(2*wait, maxRetry) {
		db, init, err := c.load()
		if err == nil {
			return db, init, true
		}
		if c.seen.changed("open", err.Error()) {
			c.d.o.Log.Error("cannot open the server file", "server", c.name, "err", err)
		}
		c.setErr(fmt.Sprintf("store: %v — retrying in %v", err, wait))
		if !sleep(ctx, wait) {
			return nil, init, false
		}
	}
}

// load opens the server's file and reads its label state and latest
// snapshot. A file found corrupt is set aside and started afresh.
func (c *collector) load() (*store.ServerDB, fleet.CollectorInit, error) {
	var init fleet.CollectorInit
	db, err := store.OpenServer(c.d.o.DataDir, c.name, true)
	if err != nil {
		return nil, init, err
	}
	init, err = readInit(db)
	if store.IsCorrupt(err) {
		if db, err = c.reopen(db, err); err != nil {
			return nil, init, err
		}
		init, err = readInit(db)
	}
	if err != nil {
		_ = db.Close()
		return nil, init, err
	}
	return db, init, nil
}

// reopen closes db, the server's file, found corrupt (by err) once open,
// sets it aside and opens a fresh one. OpenServer alone would open it
// again as it is: it sets aside only a file it cannot open.
func (c *collector) reopen(db *store.ServerDB, err error) (*store.ServerDB, error) {
	c.d.o.Log.Warn("server file corrupt; setting it aside", "server", c.name, "err", err)
	_ = db.Close()
	if err := store.SetAside(c.d.o.DataDir, c.name); err != nil {
		return nil, err
	}
	return store.OpenServer(c.d.o.DataDir, c.name, true)
}

func readInit(db *store.ServerDB) (fleet.CollectorInit, error) {
	var init fleet.CollectorInit
	labels, ok, err := db.Labels()
	if err != nil {
		return init, err
	}
	if ok {
		init.Labels = &labels
	}
	init.Last, err = db.Latest()
	return init, err
}

// connect builds the server's pool, trying again with backoff while that
// fails, and stores an error snapshot every interval until it is built:
// "connecting…" while the first build runs, then the last failure. It
// returns nil once ctx is cancelled.
func (c *collector) connect(ctx context.Context) *api.Pool {
	type result struct {
		pool *api.Pool
		err  error
	}
	results := make(chan result, 1)
	build := func() {
		go func() {
			var r result
			defer func() { results <- r }()
			defer func() {
				if v := recover(); v != nil {
					r.err = fmt.Errorf("panic: %v", v)
					c.d.o.Log.Error("pool build panicked", "server", c.name, "panic", v)
				}
			}()
			r.pool, r.err = c.d.o.BuildPool(c.srv)
		}()
	}

	interval := c.srv.PollInterval()
	every := time.NewTicker(interval)
	defer every.Stop()
	state, building, wait := "connecting…", true, firstRetry
	var retry <-chan time.Time
	build()
	for {
		select {
		case <-ctx.Done():
			if building {
				go func() {
					if r := <-results; r.pool != nil {
						_ = r.pool.Close()
					}
				}()
			}
			return nil
		case r := <-results:
			building = false
			if r.err == nil {
				return r.pool
			}
			state = fmt.Sprintf("discovery failed: %v — retrying in %v", r.err, wait)
			c.failed(state)
			every.Reset(interval)
			retry = time.After(wait)
			wait = min(2*wait, maxRetry)
		case <-retry:
			retry, building = nil, true
			build()
		case <-every.C:
			c.failed(state)
		}
	}
}

// serve has fc poll at once when the server is poked, at most once every
// MinPokeGap, until ctx is cancelled.
func (c *collector) serve(ctx context.Context, fc *fleet.Collector) {
	select {
	case <-c.poke: // poked before fc ran: its first tick, under way, answers it
	default:
	}
	var last time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.poke:
		}
		if !sleep(ctx, MinPokeGap-time.Since(last)) {
			return
		}
		last = time.Now()
		fc.PollNow(ctx)
	}
}

// sink hands a tick to the writer without ever blocking the poller. With
// the writer behind, the queue full, the tick is dropped but its label
// state kept for the next tick written. The tick of a closed collector
// that comes late (see fleet.Collector.Close) is queued for a writer that
// no longer reads: dropped too.
func (c *collector) sink(t fleet.Tick) error {
	if t.Panic != nil {
		t.Snap.Err = internalError
		if c.seen.changed("panic", fmt.Sprint(t.Panic.Value)) {
			c.d.o.Log.Error("tick panicked", "server", c.name, "panic", t.Panic.Value, "stack", string(t.Panic.Stack))
		}
	} else if t.Snap.Err == "" {
		c.seen.changed("panic", "")
	}
	if t.Labels != nil {
		c.labels.Store(nil) // older than t's
	}
	select {
	case c.queue <- t:
		if c.seen.changed("queue", "") {
			c.d.o.Log.Info("writer caught up", "server", c.name)
		}
	default:
		if t.Labels != nil {
			c.labels.Store(t.Labels)
		}
		if c.seen.changed("queue", "full") {
			c.d.o.Log.Warn("writer behind; dropping ticks", "server", c.name)
		}
	}
	return nil
}

// failed stores an error snapshot: the server could not be polled at all,
// for the reason msg.
func (c *collector) failed(msg string) {
	_ = c.sink(fleet.Tick{Snap: &fleet.Snapshot{At: time.Now().Round(0), Err: msg}})
}

// setErr records msg as the server's error in control.db, for the UIs.
func (c *collector) setErr(msg string) {
	err := c.d.ctl.SetServerErr(c.name, msg)
	if c.seen.changed("ctl", errString(err)) && err != nil {
		c.d.o.Log.Warn("cannot record the server's error", "server", c.name, "err", err)
	}
}

// writer appends a collector's ticks to the server's file. It belongs to
// the collector's writer goroutine.
type writer struct {
	c       *collector
	db      *store.ServerDB   // nil after it could not be opened again: opened for the next tick
	unsaved *fleet.LabelState // the label state of a tick whose write failed
	errSet  bool              // err is recorded in control.db
	err     string            // the server's error recorded last
}

// run writes the queued ticks, and prunes the file when asked, until ctx
// is cancelled; then it closes the file.
func (w *writer) run(ctx context.Context) {
	defer func() {
		if w.db != nil {
			_ = w.db.Close()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-w.c.prune:
			if w.db == nil {
				continue
			}
			err := w.db.Prune(now)
			if store.IsCorrupt(err) {
				w.db, err = w.c.reopen(w.db, err)
			}
			if w.c.seen.changed("prune", errString(err)) && err != nil {
				w.c.d.o.Log.Warn("cannot prune the server file", "server", w.c.name, "err", err)
			}
		case t := <-w.c.queue:
			if !w.write(t) {
				return
			}
		}
	}
}

// write appends t, with the newest label state not saved yet, and records
// a change of the server's error. It reports false when the lease is lost:
// Run ends then.
func (w *writer) write(t fleet.Tick) bool {
	if l := w.c.labels.Swap(nil); l != nil {
		t.Labels = l
	} else if t.Labels == nil {
		t.Labels = w.unsaved
	}
	err := w.append(t)
	if w.c.seen.changed("write", errString(err)) && err != nil && !errors.Is(err, store.ErrLeaseLost) {
		w.c.d.o.Log.Error("cannot store a snapshot", "server", w.c.name, "err", err)
	}
	switch {
	case errors.Is(err, store.ErrLeaseLost):
		if w.c.d.end(0, nil) {
			w.c.d.o.Log.Warn("lease lost: another daemon collects")
		}
		return false
	case err != nil:
		if t.Labels != nil {
			w.unsaved = t.Labels
		}
		return true
	}
	w.unsaved = nil
	if msg := t.Snap.Err; !w.errSet || msg != w.err {
		switch {
		case msg != "":
			w.c.d.o.Log.Warn("server error", "server", w.c.name, "err", msg)
		case w.errSet:
			w.c.d.o.Log.Info("server collected again", "server", w.c.name)
		}
		w.c.setErr(msg)
		w.errSet, w.err = true, msg
	}
	return true
}

// append appends t to the file, opening it first when it is not open. A
// file found corrupt is set aside, and t appended to a fresh one.
func (w *writer) append(t fleet.Tick) error {
	if w.db == nil {
		db, err := store.OpenServer(w.c.d.o.DataDir, w.c.name, true)
		if err != nil {
			return err
		}
		w.db = db
	}
	_, err := w.db.Append(w.c.d.token, t)
	if !store.IsCorrupt(err) {
		return err
	}
	if w.db, err = w.c.reopen(w.db, err); err != nil {
		return err
	}
	_, err = w.db.Append(w.c.d.token, t)
	return err
}

// sleep waits for d, and reports false if ctx is cancelled first.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
