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

	ring atomic.Pointer[fleet.Ring] // replaced whole on a reload, so readers never see a mix

	mu     sync.Mutex  // serialises Sync and Close; guards the fields below
	db     *ServerDB   // nil until opened, and after an error
	file   os.FileInfo // the file db has open
	loaded bool        // the window was loaded from db
	meta   meta        // db's meta when it was
	cursor int64       // newest snapshot id taken
}

// NewFeed returns an empty feed of the server configured under key, polled
// every interval; the first Sync opens its file.
func NewFeed(dataDir, key string, interval time.Duration) *Feed {
	f := &Feed{dataDir: dataDir, key: key, path: PathFor(dataDir, key), interval: interval}
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
// opens the file (none yet is no error: nothing to show) and loads the last
// hour. After an error the file is closed, and the next Sync starts over.
// A file reset under the feed — its snapshots dropped for a new version or
// pruned away, or the file set aside as corrupt and recreated, or removed —
// is noticed and its contents loaded anew, never added to what was held.
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
	fi, err := os.Stat(f.path)
	if errors.Is(err, fs.ErrNotExist) {
		f.close()
		if f.Latest() != nil {
			f.ring.Store(fleet.NewRing(f.interval))
		}
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
		if err != nil {
			return err
		}
		f.db, f.file = db, fi
	}

	m, err := f.db.meta()
	if err != nil {
		return err
	}
	if f.loaded && m == f.meta {
		snaps, last, err := f.db.Tail(f.cursor)
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
	snaps, last, err := f.db.Window(time.Now().Add(-time.Hour - 3*f.interval))
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

// close closes the file, if open; the snapshots held stay.
func (f *Feed) close() {
	if f.db != nil {
		_ = f.db.Close()
	}
	f.db, f.file, f.loaded = nil, nil, false
}

// Close closes the feed's file. A later Sync opens it again.
func (f *Feed) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.close()
	return nil
}
