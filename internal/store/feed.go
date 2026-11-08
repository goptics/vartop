package store

import (
	"errors"
	"io/fs"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goptics/vartop/internal/fleet"
)

// Feed is a UI's view of one server's file: the last hour of snapshots
// (with none that recent, the newest alone), loaded once and then followed
// as the collector appends. It rates nothing:
// the rates come stored. Sync may be called from several goroutines; the
// snapshots it holds are read-only.
type Feed struct {
	dataDir, key, path string
	interval           time.Duration
	now                func() time.Time // the clock the window is reckoned by: time.Now but in tests
	windowRead         func()           // called once a load has read its window: nil but in tests, which append there

	ring atomic.Pointer[fleet.Ring] // replaced whole on a reload, so readers never see a mix

	mu     sync.Mutex  // serialises Sync and Close; guards the fields below
	closed bool        // Close was called: Sync opens nothing any more
	db     *ServerDB   // nil until opened, and after an error
	file   os.FileInfo // the file db has open
	loaded bool        // the window was loaded from db
	meta   meta        // db's meta when it was
	cursor int64       // newest snapshot id taken
}

// NewFeed returns an empty feed of the server configured under key, polled
// every interval; the first Sync opens its file.
func NewFeed(dataDir, key string, interval time.Duration) *Feed {
	f := &Feed{dataDir: dataDir, key: key, path: PathFor(dataDir, key), interval: interval, now: time.Now}
	f.ring.Store(fleet.NewRing(interval))
	return f
}

// Day reads worker's buckets of the view label from since on through the
// feed's file (ServerDB.Day): an empty day while it has none open. Sync
// first.
func (f *Feed) Day(worker, label string, since time.Time) (*fleet.Day, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.db == nil {
		return fleet.NewDay(worker), nil
	}
	return f.db.Day(worker, label, since)
}

// Interval is the poll interval the feed was made for.
func (f *Feed) Interval() time.Duration { return f.interval }

// Snapshots returns the snapshots held, oldest first.
func (f *Feed) Snapshots() []*fleet.Snapshot { return f.ring.Load().Snapshots() }

// Latest returns the newest snapshot held, or nil.
func (f *Feed) Latest() *fleet.Snapshot { return f.ring.Load().Latest() }

// Sync takes in what the collector stored since the last Sync. The first one
// opens the file (none yet, or an empty one, is no error: nothing to show)
// and loads the last hour. After an error the file is closed, and the next Sync starts over.
// A file reset under the feed — its snapshots dropped for a new version or
// pruned away, or the file set aside as corrupt and recreated, or removed —
// is noticed and its contents loaded anew, never added to what was held.
// A feed not synced for longer than its window (hidden, say) loads the
// window anew too, rather than everything stored since. A window with no
// row of a file that has some holds the file's newest, for the UIs to show
// stale. After Close, Sync does nothing: the snapshots held stay.
func (f *Feed) Sync() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	err := f.sync()
	if err != nil {
		f.close()
	}
	return err
}

func (f *Feed) sync() error {
	if f.closed {
		return nil
	}
	fi, err := os.Stat(f.path)
	if errors.Is(err, fs.ErrNotExist) || err == nil && fi.Size() == 0 {
		f.none()
		return nil
	}
	if err != nil {
		return err
	}
	if f.db != nil && !os.SameFile(fi, f.file) {
		f.close() // another file took its name
	}
	if f.db == nil {
		db, err := OpenServer(f.dataDir, f.key, false)
		if errors.Is(err, fs.ErrNotExist) {
			f.none() // gone since, or not set up yet
			return nil
		}
		if err != nil {
			return err
		}
		f.db, f.file = db, fi
	}

	m, err := f.db.meta()
	if err != nil {
		return err
	}
	since := f.now().Add(-time.Hour - 3*f.interval)
	same := f.loaded && m == f.meta // the file the snapshots held came from, not reset since
	if latest := f.Latest(); same && latest != nil && !latest.At.Before(since) {
		// Every row after the cursor is newer than the snapshot it took:
		// within the window. A cursor older than the window loads it anew.
		snaps, last, err := f.db.tailSince(f.cursor, since)
		if err != nil {
			return err
		}
		if last >= f.cursor {
			ring := f.ring.Load()
			for _, s := range snaps {
				ring.Append(s)
			}
			f.cursor = last
			return nil
		}
		// The ids went back: what the cursor counted is gone.
	}
	snaps, last, err := f.db.Window(since)
	if err != nil {
		return err
	}
	if f.windowRead != nil {
		f.windowRead()
	}
	if same && len(snaps) == 0 && last >= f.cursor {
		// Nothing within the window, and nothing dropped: the server was
		// not collected for longer than the window (the collector down). What
		// the feed holds stays, for the UIs to show stale.
		f.cursor = last
		return nil
	}
	if len(snaps) == 0 && last > 0 {
		// The same, on a first load (or one after a reset): the file's
		// newest row is held, for the UIs to show stale with its age,
		// rather than as a server never collected (R43). The newest Window
		// counted, whose id the cursor takes: one appended since is the
		// next Sync's, and taken in once. One that cannot be decoded, or
		// pruned since, is none.
		latest, err := f.db.snapshot(last)
		if err != nil && !errors.Is(err, ErrUndecodable) {
			return err
		}
		if latest != nil {
			snaps = []*fleet.Snapshot{latest}
		}
	}
	ring := fleet.NewRing(f.interval)
	for _, s := range snaps {
		ring.Append(s)
	}
	f.ring.Store(ring)
	f.loaded, f.meta, f.cursor = true, m, last
	return nil
}

// none empties the feed of a server with no file, or one the collector has
// only just created: nothing to show yet.
func (f *Feed) none() {
	f.close()
	if f.Latest() != nil {
		f.ring.Store(fleet.NewRing(f.interval))
	}
}

// close closes the file, if open; the snapshots held stay.
func (f *Feed) close() {
	if f.db != nil {
		_ = f.db.Close()
	}
	f.db, f.file, f.loaded = nil, nil, false
}

// Close closes the feed's file, for good: a later Sync (one under way when
// the feed was dropped, say) opens nothing, and the snapshots held stay.
func (f *Feed) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	f.close()
	return nil
}
