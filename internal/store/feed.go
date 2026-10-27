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

// Feed is a UI's view of one server's file: the last hour of snapshots,
// loaded once and then followed as the daemon appends. It rates nothing:
// the rates come stored. Sync may be called from several goroutines; the
// snapshots it holds are read-only.
type Feed struct {
	dataDir, key, path string
	interval           time.Duration
	now                func() time.Time // the clock the window is reckoned by: time.Now but in tests

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

// Interval is the poll interval the feed was made for.
func (f *Feed) Interval() time.Duration { return f.interval }

// Snapshots returns the snapshots held, oldest first.
func (f *Feed) Snapshots() []*fleet.Snapshot { return f.ring.Load().Snapshots() }

// Latest returns the newest snapshot held, or nil.
func (f *Feed) Latest() *fleet.Snapshot { return f.ring.Load().Latest() }

// Sync takes in what the daemon stored since the last Sync. The first one
// opens the file (none yet, or an empty one, is no error: nothing to show)
// and loads the last hour. After an error the file is closed, and the next Sync starts over.
// A file reset under the feed — its snapshots dropped for a new version or
// pruned away, or the file set aside as corrupt and recreated, or removed —
// is noticed and its contents loaded anew, never added to what was held.
// A feed not synced for longer than its window (hidden, say) loads the
// window anew too, rather than everything stored since. After Close, Sync
// does nothing: the snapshots held stay.
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
	if latest := f.Latest(); f.loaded && m == f.meta && latest != nil && !latest.At.Before(since) {
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
	ring := fleet.NewRing(f.interval)
	for _, s := range snaps {
		ring.Append(s)
	}
	f.ring.Store(ring)
	f.loaded, f.meta, f.cursor = true, m, last
	return nil
}

// none empties the feed of a server with no file, or one the daemon has
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
