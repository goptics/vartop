package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/goptics/vartop/internal/fleet"
)

// ServerDB is one server's file: its snapshots, one row per tick, and its
// label state. The daemon opens it as the writer; the UIs as readers.
type ServerDB struct {
	db     *sql.DB
	key    string
	writer bool
	path   string
	file   os.FileInfo // the file at path once a writer set it up; nil for a reader

	skipped atomic.Bool // a snapshot that cannot be decoded was skipped, and logged

	capBytes int64 // Prune's cap on the bytes in use: ServerCapBytes, but in tests
	batch    int   // snapshots Prune deletes at a time past the cap
}

// meta is what a server file says about itself.
type meta struct {
	key     string // the config key the file belongs to
	version int64  // StoreVersion of its snapshots
	epoch   int64  // when its snapshots table was created: changes on every reset
}

const serverSchema = `
CREATE TABLE IF NOT EXISTS meta (k TEXT PRIMARY KEY, v) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS labels (id INTEGER PRIMARY KEY CHECK (id = 1), state BLOB NOT NULL);`

// snapshotsSchema: AUTOINCREMENT, so that a pruned id is never reused and
// a reader's cursor stays meaningful.
const snapshotsSchema = `
CREATE TABLE snapshots (id INTEGER PRIMARY KEY AUTOINCREMENT, at INTEGER NOT NULL, data BLOB NOT NULL);
CREATE INDEX snapshots_at ON snapshots (at);`

var errReadOnly = errors.New("server file opened read-only")

// ErrUndecodable is the error of a stored record that cannot be decoded:
// bit rot inside a blob, which SQLite does not see, or a codec changed
// without a new StoreVersion.
var ErrUndecodable = errors.New("stored record cannot be decoded")

// logger is where the store reports what it gets past without failing.
var logger atomic.Pointer[slog.Logger]

// SetLogger sets where the store reports what it gets past without failing
// — a stored snapshot it skips because it cannot be decoded, once per file
// opened — or, with nil, has it report nothing, as before it is set: the
// TUI owns the terminal.
func SetLogger(l *slog.Logger) { logger.Store(l) }

func storeLog() *slog.Logger {
	if l := logger.Load(); l != nil {
		return l
	}
	return slog.New(slog.DiscardHandler)
}

// errNotSetUp is a reader's error for a file that has no tables yet: its
// writer's first connection has made it a database, and its first
// transaction, which creates them, is not committed yet.
var errNotSetUp = fmt.Errorf("server file not set up yet: %w", fs.ErrNotExist)

// OpenServer opens the file of the server configured under key (see
// PathFor). A writer — the daemon — creates it if needed; drops its
// snapshots, keeping its labels, when they are of another StoreVersion; and
// renames a corrupt file to <file>.corrupt-<unix seconds> and starts afresh.
// A reader needs the file to exist and be set up (an error wrapping
// fs.ErrNotExist when it is not: the daemon may be creating it), and to be
// of this key and version.
func OpenServer(dataDir, key string, writer bool) (*ServerDB, error) {
	path := PathFor(dataDir, key)
	if !writer {
		if _, err := os.Stat(path); err != nil {
			return nil, err
		}
		s := &ServerDB{db: openDB(path, false, ""), key: key, path: path}
		if err := s.checkReader(); err != nil {
			_ = s.db.Close()
			return nil, fmt.Errorf("open %s: %w", path, err)
		}
		return s, nil
	}
	s, err := openWriter(dataDir, path, key)
	if IsCorrupt(err) {
		if err := setAside(path); err != nil {
			return nil, err
		}
		s, err = openWriter(dataDir, path, key)
	}
	return s, err
}

// openWriter opens the file at path as its writer. A file it created and
// could not set up is removed: no reader is to find it empty.
func openWriter(dataDir, path, key string) (*ServerDB, error) {
	created, err := prepareFile(path)
	if err != nil {
		return nil, err
	}
	s := &ServerDB{db: openDB(path, true, controlPath(dataDir)), key: key, writer: true, path: path,
		capBytes: ServerCapBytes, batch: 1000}
	err = s.init()
	if err == nil {
		if s.file, err = os.Stat(path); err == nil {
			pinID(s.file)
		}
	}
	if err != nil {
		_ = s.db.Close()
		if created {
			_ = removeFile(path)
		}
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return s, nil
}

// Replaced reports whether the file a writer opened is no longer the one at
// its path: removed (servers/ deleted, say), or another put in its place.
// A writer whose file is replaced writes what no UI reads.
func (s *ServerDB) Replaced() (bool, error) {
	if s.file == nil {
		return false, errReadOnly
	}
	return replaced(s.path, s.file)
}

// blame returns err as it is unless it says a file is corrupt and came from
// a statement that takes control.db (attached as ctl) as well as the
// server file — a writer's BEGIN IMMEDIATE takes both — while the server
// file itself reads: then it is control.db's, and made an error IsCorrupt
// is false for. Setting a healthy server file aside for control.db's fault
// would lose its history for nothing.
func (s *ServerDB) blame(err error) error {
	if !IsCorrupt(err) || IsCorrupt(checkFile(s.path)) {
		return err
	}
	return ctlErr(err)
}

// ctlErr makes err, an error of control.db met on a server file's
// connection, one IsCorrupt is false for (%v, as Connect's ATTACH).
func ctlErr(err error) error { return fmt.Errorf("control.db: %v", err) }

// checkFile reads the header and schema of the database file at path, on
// a connection of its own with nothing attached.
func checkFile(path string) error {
	db := openDB(path, false, "")
	defer func() { _ = db.Close() }()
	var n int
	return db.QueryRow(`SELECT count(*) FROM sqlite_master`).Scan(&n)
}

// init creates what the file lacks, and resets what is not this key's or
// of this version.
func (s *ServerDB) init() error {
	tx, err := s.db.Begin()
	if err != nil {
		return s.blame(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(serverSchema); err != nil {
		return err
	}
	m, err := readMeta(tx)
	if err != nil {
		return err
	}
	switch {
	case m.key != s.key || m.epoch == 0:
		// New, or another key's (two keys of the same file name): nothing
		// in it is this server's.
		if _, err := tx.Exec(`DELETE FROM labels`); err != nil {
			return err
		}
	case m.version != StoreVersion:
	default:
		return tx.Commit()
	}
	if _, err := tx.Exec(`DROP TABLE IF EXISTS snapshots`); err != nil {
		return err
	}
	if _, err := tx.Exec(snapshotsSchema); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO meta (k, v) VALUES ('key', ?), ('version', ?), ('epoch', ?)`,
		s.key, StoreVersion, time.Now().UnixNano()); err != nil {
		return err
	}
	return tx.Commit()
}

type queryer interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

func readMeta(q queryer) (meta, error) {
	rows, err := q.Query(`SELECT k, v FROM meta`)
	if err != nil {
		return meta{}, err
	}
	defer rows.Close()
	var m meta
	for rows.Next() {
		var k string
		var v any
		if err := rows.Scan(&k, &v); err != nil {
			return meta{}, err
		}
		switch k {
		case "key":
			m.key, _ = v.(string)
		case "version":
			m.version, _ = v.(int64)
		case "epoch":
			m.epoch, _ = v.(int64)
		}
	}
	return m, rows.Err()
}

// checkReader checks, as one read of the file, that its writer set it up
// (errNotSetUp if not) and that it is this key's and version's.
func (s *ServerDB) checkReader() error {
	tx, err := s.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var tables int
	if err := tx.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'meta'`).Scan(&tables); err != nil {
		return err
	}
	if tables == 0 {
		return errNotSetUp
	}
	_, err = checkMeta(tx, s.key)
	return err
}

// meta reads the file's meta, which must be this key's and version's.
func (s *ServerDB) meta() (meta, error) { return checkMeta(s.db, s.key) }

// checkMeta reads the meta of a file through q, which must be key's and
// of this version.
func checkMeta(q queryer, key string) (meta, error) {
	m, err := readMeta(q)
	switch {
	case err != nil:
		return meta{}, err
	case m.key != key:
		return meta{}, fmt.Errorf("server file is %q's, not %q's", m.key, key)
	case m.version != StoreVersion:
		return meta{}, fmt.Errorf("server file is of store version %d, not %d", m.version, StoreVersion)
	}
	return m, nil
}

// SetAside renames the file of the server configured under key, with its
// -wal and -shm, as OpenServer does a corrupt one: for a file found corrupt
// once open (its header and schema read, a table not), which OpenServer
// would open again as it is. The file must be closed. The next writer
// starts afresh.
func SetAside(dataDir, key string) error { return setAside(PathFor(dataDir, key)) }

// setAside renames the corrupt file at path, with its -wal and -shm, to
// <path>.corrupt-<unix seconds>: a new file at path must not pick up the
// old one's log or shared memory.
func setAside(path string) error {
	aside := fmt.Sprintf("%s.corrupt-%d", path, time.Now().Unix())
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Rename(path+suffix, aside+suffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("set corrupt server file aside: %w", err)
		}
	}
	return nil
}

// Close closes the file.
func (s *ServerDB) Close() error { return s.db.Close() }

// Append stores the tick's snapshot (and its label state, when the tick
// carries one) in one transaction, as the daemon holding the lease of
// token: if it does not, nothing is written and the error is ErrLeaseLost.
// It returns the snapshot's id.
func (s *ServerDB) Append(token string, t fleet.Tick) (id int64, err error) {
	if !s.writer {
		return 0, errReadOnly
	}
	data, err := encodeSnapshot(t.Snap)
	if err != nil {
		return 0, err
	}
	var labels []byte
	if t.Labels != nil {
		if labels, err = encodeLabels(*t.Labels); err != nil {
			return 0, err
		}
	}

	// IMMEDIATE takes control.db's write lock too: a Claim cannot slip in
	// between the check and the insert.
	tx, err := s.db.Begin()
	if err != nil {
		return 0, s.blame(err)
	}
	defer func() { _ = tx.Rollback() }()
	var holder string
	switch err := tx.QueryRow(`SELECT token FROM ctl.daemon WHERE id = 1`).Scan(&holder); {
	case errors.Is(err, sql.ErrNoRows):
		return 0, ErrLeaseLost
	case err != nil:
		return 0, ctlErr(err)
	case holder == "" || holder != token:
		return 0, ErrLeaseLost
	}
	res, err := tx.Exec(`INSERT INTO snapshots (at, data) VALUES (?, ?)`, t.Snap.At.UnixMilli(), data)
	if err != nil {
		return 0, err
	}
	if id, err = res.LastInsertId(); err != nil {
		return 0, err
	}
	if labels != nil {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO labels (id, state) VALUES (1, ?)`, labels); err != nil {
			return 0, err
		}
	}
	return id, tx.Commit()
}

// Window returns the snapshots taken since since, oldest first, and the
// newest id in the file (0 when it has none) to Tail from.
func (s *ServerDB) Window(since time.Time) ([]*fleet.Snapshot, int64, error) {
	// A range of ids from the first in the window (found by the index on at:
	// left to itself, min(id) walks the table from its oldest row), not
	// "WHERE at >= ?1 ORDER BY id", which scans the whole table.
	return s.scan(`SELECT id, at, data FROM snapshots
		WHERE id >= (SELECT min(id) FROM snapshots INDEXED BY snapshots_at WHERE at >= ?1) AND at >= ?1
		ORDER BY id`, since.UnixMilli())
}

// Tail returns the snapshots after id afterID, oldest first, and the newest
// id in the file (0 when it has none). A newest id below afterID means the
// snapshots afterID counted were dropped: the file was reset or pruned
// empty.
func (s *ServerDB) Tail(afterID int64) ([]*fleet.Snapshot, int64, error) {
	return s.scan(`SELECT id, at, data FROM snapshots WHERE id > ?1 ORDER BY id`, afterID)
}

// tailSince is Tail, but without the snapshots taken before since: a feed
// takes in none older than its window, whatever their ids.
func (s *ServerDB) tailSince(afterID int64, since time.Time) ([]*fleet.Snapshot, int64, error) {
	return s.scan(`SELECT id, at, data FROM snapshots WHERE id > ?1 AND at >= ?2 ORDER BY id`, afterID, since.UnixMilli())
}

// scan runs a query of id, at and data, and reads the newest id, in one
// read transaction, so that the two agree. A snapshot that cannot be
// decoded is skipped (and logged, the first time): one bad row must not
// cost the whole window.
func (s *ServerDB) scan(query string, args ...any) ([]*fleet.Snapshot, int64, error) {
	tx, err := s.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var newest int64
	if err := tx.QueryRow(`SELECT coalesce(max(id), 0) FROM snapshots`).Scan(&newest); err != nil {
		return nil, 0, err
	}
	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var snaps []*fleet.Snapshot
	for rows.Next() {
		var id, at int64
		var data []byte
		if err := rows.Scan(&id, &at, &data); err != nil {
			return nil, 0, err
		}
		snap, err := decodeSnapshot(at, data)
		if err != nil {
			if !s.skipped.Swap(true) {
				storeLog().Warn("skipping stored snapshots that cannot be decoded", "file", s.path, "id", id, "err", err)
			}
			continue
		}
		snaps = append(snaps, snap)
	}
	return snaps, newest, rows.Err()
}

// Latest returns the newest snapshot, or nil when there is none; an error
// wrapping ErrUndecodable when it cannot be decoded.
func (s *ServerDB) Latest() (*fleet.Snapshot, error) {
	var at int64
	var data []byte
	err := s.db.QueryRow(`SELECT at, data FROM snapshots ORDER BY id DESC LIMIT 1`).Scan(&at, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	snap, err := decodeSnapshot(at, data)
	if err != nil {
		return nil, fmt.Errorf("latest snapshot: %w: %w", ErrUndecodable, err)
	}
	return snap, nil
}

// snapshot returns the snapshot of id, or nil when there is none (pruned,
// say); an error wrapping ErrUndecodable when it cannot be decoded.
func (s *ServerDB) snapshot(id int64) (*fleet.Snapshot, error) {
	var at int64
	var data []byte
	err := s.db.QueryRow(`SELECT at, data FROM snapshots WHERE id = ?`, id).Scan(&at, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	snap, err := decodeSnapshot(at, data)
	if err != nil {
		return nil, fmt.Errorf("snapshot %d: %w: %w", id, ErrUndecodable, err)
	}
	return snap, nil
}

// Labels returns the label state stored last, and whether there is one; an
// error wrapping ErrUndecodable when it cannot be decoded.
func (s *ServerDB) Labels() (fleet.LabelState, bool, error) {
	var data []byte
	err := s.db.QueryRow(`SELECT state FROM labels WHERE id = 1`).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return fleet.LabelState{}, false, nil
	}
	if err != nil {
		return fleet.LabelState{}, false, err
	}
	l, err := decodeLabels(data)
	if err != nil {
		return fleet.LabelState{}, false, fmt.Errorf("labels: %w: %w", ErrUndecodable, err)
	}
	return l, true, nil
}

// Prune deletes the snapshots older than Retention at now, then the oldest
// ones, a batch at a time, while the file uses more than ServerCapBytes.
// Pages freed are reused, not returned: the file does not shrink.
func (s *ServerDB) Prune(now time.Time) error {
	if !s.writer {
		return errReadOnly
	}
	if _, err := s.db.Exec(`DELETE FROM snapshots WHERE at < ?`, now.Add(-Retention).UnixMilli()); err != nil {
		return err
	}
	for {
		used, err := s.used()
		if err != nil || used <= s.capBytes {
			return err
		}
		res, err := s.db.Exec(`DELETE FROM snapshots WHERE id IN (SELECT id FROM snapshots ORDER BY id LIMIT ?)`, s.batch)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return err
		}
	}
}

// used is the bytes of the file's pages in use: the pages, but for the
// free ones.
func (s *ServerDB) used() (int64, error) {
	var n int64
	err := s.db.QueryRow(`SELECT (c.page_count - f.freelist_count) * z.page_size
		FROM pragma_page_count() c, pragma_freelist_count() f, pragma_page_size() z`).Scan(&n)
	return n, err
}

// RemoveServerFile deletes the file of the server configured under key,
// with its -wal and -shm. A file already gone is no error; one still open
// by a reader may fail to go on Windows, to be retried later.
func RemoveServerFile(dataDir, key string) error { return removeFile(PathFor(dataDir, key)) }

// fileSuffixes are the files SQLite keeps beside a database file.
var fileSuffixes = []string{"-wal", "-shm", "-journal"}

// removeFile deletes the file at path and the files SQLite keeps beside it.
// A file already gone is no error.
func removeFile(path string) error {
	var errs []error
	for _, suffix := range append([]string{""}, fileSuffixes...) {
		if err := os.Remove(path + suffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// RemoveStale deletes the files under servers/ that are not the file of one
// of keys and were last written before cutoff, with their -wal and -shm:
// the files of servers no longer configured, and corrupt ones set aside. A
// file's last write is its own or its -wal's, not its -shm's, which
// readers write. It returns the files removed. One a reader holds open may
// fail to go on Windows, to be retried later.
func RemoveStale(dataDir string, keys []string, cutoff time.Time) (removed []string, err error) {
	dir := filepath.Join(dataDir, "servers")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	keep := make(map[string]bool, len(keys))
	for _, key := range keys {
		keep[filepath.Base(PathFor(dataDir, key))] = true
	}
	written := make(map[string]time.Time) // file name → its last write
	for _, e := range entries {
		name, base := e.Name(), e.Name()
		for _, suffix := range fileSuffixes {
			base = strings.TrimSuffix(base, suffix)
		}
		info, err := e.Info()
		if e.IsDir() || keep[base] || err != nil { // err: gone since ReadDir
			continue
		}
		at := written[base]
		if !strings.HasSuffix(name, "-shm") && info.ModTime().After(at) {
			at = info.ModTime()
		}
		written[base] = at
	}
	var errs []error
	for _, base := range slices.Sorted(maps.Keys(written)) {
		if !written[base].Before(cutoff) {
			continue
		}
		path := filepath.Join(dir, base)
		if err := removeFile(path); err != nil {
			errs = append(errs, err)
			continue
		}
		removed = append(removed, path)
	}
	return removed, errors.Join(errs...)
}
