package collect

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
const internalError = "internal error (see vartop web's log)"

// collector collects one configured server: it opens the server's file,
// builds its pool and runs a fleet.Collector over it, whose ticks a writer
// goroutine appends to the file. Every goroutine it starts ends once it is
// cancelled, but for a pool build, which cannot be: that one closes its
// pool when it ends.
type collector struct {
	d      *holder
	name   string
	srv    *config.Server
	enc    string             // srv as JSON: what tells an edit
	source string             // fleet.SourceOf(srv), stamped on every snapshot
	cancel context.CancelFunc // stops the collector
	done   chan struct{}      // closed once its file is closed

	poke  chan struct{}  // a poll asked for; one pending at most
	prune chan time.Time // a prune asked for, at that time
	queue chan queued
	// labelSeq numbers the label states the sink is handed, in order.
	labelSeq atomic.Uint64
	// dropped is the label state of the newest tick dropped that had one,
	// until the writer takes it to save with the next tick it writes.
	dropped atomic.Pointer[labelled]
	seen    stateLog
}

// labelled is a label state the sink was handed, numbered in the order it
// was: the writer saves a state only if it is newer than the one it saved
// last, whatever order it meets them in.
type labelled struct {
	state *fleet.LabelState
	seq   uint64 // 1, 2, …; 0 with no state
}

// queued is a tick waiting for the writer, with its label state numbered.
type queued struct {
	tick   fleet.Tick
	labels labelled
}

// start starts collecting srv, named name and encoded as enc, after delay.
// A collector of the same name stopped before lets go of the file first.
func (d *holder) start(name string, srv *config.Server, enc string, delay time.Duration) {
	ctx, cancel := context.WithCancel(context.Background())
	c := &collector{
		d: d, name: name, srv: srv, enc: enc, source: fleet.SourceOf(srv), cancel: cancel,
		done:  make(chan struct{}),
		poke:  make(chan struct{}, 1),
		prune: make(chan time.Time, 1),
		queue: make(chan queued, queueLen),
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
	w := &writer{c: c, db: db, savedState: init.Labels}
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
	init, err = c.readInit(db)
	if store.IsCorrupt(err) {
		if db, err = c.reopen(db, err); err != nil {
			return nil, init, err
		}
		init, err = c.readInit(db)
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

// readInit reads the state the collector starts from: the label state
// saved last, and the latest snapshot to rate the first tick against. One
// that cannot be decoded is logged and done without — units numbered
// afresh, rates from the second tick — rather than keep the server from
// being collected at all.
func (c *collector) readInit(db *store.ServerDB) (fleet.CollectorInit, error) {
	var init fleet.CollectorInit
	labels, ok, err := db.Labels()
	switch {
	case errors.Is(err, store.ErrUndecodable):
		if c.seen.changed("labels", err.Error()) {
			c.d.o.Log.Error("saved label state cannot be decoded; numbering the units afresh", "server", c.name, "err", err)
		}
	case err != nil:
		return init, err
	case ok:
		init.Labels = &labels
	}
	init.Last, err = db.Latest()
	if errors.Is(err, store.ErrUndecodable) {
		if c.seen.changed("latest", err.Error()) {
			c.d.o.Log.Error("latest snapshot cannot be decoded; rating from the next tick", "server", c.name, "err", err)
		}
		init.Last, err = nil, nil
	}
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
// no longer reads: dropped too. Its calls come one at a time (from the
// poller, or from connect before the poller runs), so each label state it
// numbers is newer than every one before.
func (c *collector) sink(t fleet.Tick) error {
	t.Snap.Source = c.source
	if t.Panic != nil {
		t.Snap.Err = internalError
		if c.seen.changed("panic", fmt.Sprint(t.Panic.Value)) {
			c.d.o.Log.Error("tick panicked", "server", c.name, "panic", t.Panic.Value, "stack", string(t.Panic.Stack))
		}
	} else if t.Snap.Err == "" {
		c.seen.changed("panic", "")
	}
	var l labelled
	if t.Labels != nil {
		l = labelled{state: t.Labels, seq: c.labelSeq.Add(1)}
	}
	select {
	case c.queue <- queued{tick: t, labels: l}:
		if c.seen.changed("queue", "") {
			c.d.o.Log.Info("writer caught up", "server", c.name)
		}
	default:
		if l.state != nil {
			c.dropped.Store(&l)
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
	c          *collector
	db         *store.ServerDB   // nil after it could not be opened again: opened for the next tick
	saved      uint64            // the number of the label state saved last
	savedState *fleet.LabelState // the label state saved last (in this file or one before): nil before any
	fresh      bool              // db was started afresh, and has no label state yet
	unsaved    labelled          // the newest label state a failed write did not save
	errSet     bool              // err is recorded in control.db
	err        string            // the server's error recorded last
	unfilled   bool              // db may hold snapshots from before its day buckets (see fill)
}

const (
	// fillEvery is the least pause between two batches of fill; a batch
	// that took longer is followed by a pause as long, so that a fill takes
	// at most half of the server file's time and leaves the ticks room.
	fillEvery = 50 * time.Millisecond
	// fillBatch is how many snapshots one batch of fill sums: one short
	// transaction, even for a large fleet.
	fillBatch = 50
)

// run writes the queued ticks, and prunes the file when asked, until ctx
// is cancelled; then it closes the file.
func (w *writer) run(ctx context.Context) {
	defer func() {
		if w.db != nil {
			_ = w.db.Close()
		}
	}()
	w.unfilled = true
	fill := time.NewTimer(fillEvery)
	defer fill.Stop()
	for {
		var filling <-chan time.Time
		if w.unfilled && w.db != nil {
			filling = fill.C
		}
		select {
		case <-ctx.Done():
			return
		case now := <-w.c.prune:
			w.prune(now)
			w.unfilled = true // a fill that failed, or a file opened anew
		case q := <-w.c.queue:
			if !w.write(q) {
				return
			}
		case <-filling:
			began := time.Now()
			w.fill()
			fill.Reset(max(fillEvery, time.Since(began)))
		}
	}
}

// fill sums a batch of the snapshots the file holds from before it had
// day buckets into them (store.FillBuckets), until there are none left:
// the 24h charts then show the history stored by an older vartop too. A
// failure stops it until the next prune, which starts it again.
func (w *writer) fill() {
	more, err := w.db.FillBuckets(w.c.d.o.Now().Add(-fleet.DayWindow), fillBatch)
	if w.c.seen.changed("fill", errString(err)) && err != nil {
		w.c.d.o.Log.Warn("cannot fill the day buckets", "server", w.c.name, "err", err)
	}
	w.unfilled = more && err == nil
}

// prune prunes the file at now. A file found corrupt is set aside, and a
// fresh one opened; so is one no longer at its path (removed, with
// servers/ say): what the writer would add to it no UI could read.
func (w *writer) prune(now time.Time) {
	if w.db == nil {
		return
	}
	if gone, err := w.db.Replaced(); gone {
		w.c.d.o.Log.Warn("server file removed; starting a new one", "server", w.c.name)
		_ = w.db.Close()
		w.db, err = store.OpenServer(w.c.d.o.DataDir, w.c.name, true)
		if err != nil {
			w.db = nil // opened for the next tick
			w.c.d.o.Log.Error("cannot open the server file", "server", w.c.name, "err", err)
			return
		}
		w.fresh = true
	}
	err := w.db.Prune(now, w.c.srv.HistorySpan())
	if store.IsCorrupt(err) {
		if w.db, err = w.c.reopen(w.db, err); err == nil {
			w.fresh = true
		}
	}
	if w.c.seen.changed("prune", errString(err)) && err != nil {
		w.c.d.o.Log.Warn("cannot prune the server file", "server", w.c.name, "err", err)
	}
}

// write appends q's tick, with the newest label state of all those it has
// met (q's own, a dropped tick's, one a failed write did not save) when
// that is newer than the one saved last, and records a change of the
// server's error. It reports false when the lease is lost: Run ends then.
func (w *writer) write(q queued) bool {
	t, newest := q.tick, q.labels
	if d := w.c.dropped.Swap(nil); d != nil && d.seq > newest.seq {
		newest = *d
	}
	if w.unsaved.seq > newest.seq {
		newest = w.unsaved
	}
	t.Labels = nil
	if newest.seq > w.saved {
		t.Labels = newest.state
	}
	err := w.append(t)
	if w.c.seen.changed("write", errString(err)) && err != nil && !errors.Is(err, store.ErrLeaseLost) {
		w.c.d.o.Log.Error("cannot store a snapshot", "server", w.c.name, "err", err)
	}
	switch {
	case errors.Is(err, store.ErrLeaseLost):
		if w.c.d.end(0, nil) {
			w.c.d.o.Log.Warn("lease lost: another vartop collects")
		}
		return false
	case err != nil:
		if t.Labels != nil {
			w.unsaved = newest
		}
		// The UIs say why no new data comes, as for an open that fails.
		if msg := "store: " + err.Error(); !w.errSet || w.err != msg {
			w.c.setErr(msg)
			w.errSet, w.err = true, msg
		}
		return true
	}
	if t.Labels != nil {
		w.saved, w.savedState = newest.seq, newest.state
	}
	w.unsaved = labelled{}
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
// file found corrupt is set aside, and t appended to a fresh one. The
// first tick a file started afresh gets carries the label state saved last,
// if it carries none: a restart before the next label change would number
// the units anew.
func (w *writer) append(t fleet.Tick) error {
	if w.db == nil {
		db, err := store.OpenServer(w.c.d.o.DataDir, w.c.name, true)
		if err != nil {
			return err
		}
		w.db, w.fresh = db, true
	}
	err := w.appendTo(t)
	if !store.IsCorrupt(err) {
		return err
	}
	if w.db, err = w.c.reopen(w.db, err); err != nil {
		return err
	}
	w.fresh = true
	return w.appendTo(t)
}

// appendTo appends t to w.db, with the label state saved last if the file
// is fresh and t has none.
func (w *writer) appendTo(t fleet.Tick) error {
	if w.fresh && t.Labels == nil {
		t.Labels = w.savedState
	}
	_, err := w.db.Append(w.c.d.token, t)
	if err == nil {
		w.fresh = false
	}
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
