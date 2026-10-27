// Package store is vartop's shared history on disk: SQLite files that the
// collector writes and the UIs read. control.db holds the collector's lease, the
// UIs' poke counters and each server's collection error; every configured
// server has a file of its own under servers/ with its snapshots (24h) and
// label state. Times are wall-clock unix milliseconds. Nothing here talks
// to varmq or keeps a token.
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const (
	// Retention is how long a snapshot is kept.
	Retention = 24 * time.Hour
	// ServerCapBytes caps the bytes one server's snapshots take in its
	// file; Prune deletes the oldest past it.
	ServerCapBytes = 256 << 20
	// LeaseTTL is how long a lease stays fresh after its last beat.
	LeaseTTL = 15 * time.Second
	// BeatEvery is how often the collector renews its lease.
	BeatEvery = 2 * time.Second
	// StoreVersion is the shape of the snapshot records. A file of another
	// version has its snapshots dropped when the collector opens it.
	StoreVersion = 1
)

// DataDir is where the store lives: $VARTOP_DATA_DIR, else vartop in the
// user's cache directory.
func DataDir() (string, error) {
	dir := os.Getenv("VARTOP_DATA_DIR")
	if dir == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return "", fmt.Errorf("no data directory: %w", err)
		}
		dir = filepath.Join(cache, "vartop")
	}
	return filepath.Abs(dir)
}

// PathFor is the file of the server configured under key: the key made
// safe as a file name on every OS (lowercase, [a-z0-9._-] kept, anything
// else "-", at most 40 characters), then the first 6 hex digits of its
// SHA-256, so that keys alike once made safe (or differing only in case)
// still get files of their own. It is the only key → file mapping.
func PathFor(dataDir, key string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(key) {
		if b.Len() == 40 {
			break
		}
		switch {
		case 'a' <= r && r <= 'z', '0' <= r && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(dataDir, "servers", b.String()+"-"+hex.EncodeToString(sum[:3])+".db")
}

func controlPath(dataDir string) string { return filepath.Join(dataDir, "control.db") }

// pragmas are set on every connection to every file.
var pragmas = []string{"busy_timeout(5000)", "journal_mode(WAL)", "synchronous(NORMAL)", "journal_size_limit(16777216)"}

// dsn is the data source name of the file at path. A writer starts its
// transactions IMMEDIATE, so that writers queue at BEGIN instead of failing
// at their first write. A reader opens the file read-only: it never
// creates a file that is not there (yet, or any more) and never writes one.
func dsn(path string, writer bool) string {
	q := url.Values{"_pragma": pragmas}
	if writer {
		q.Set("_txlock", "immediate")
	} else {
		q.Add("_pragma", "query_only(1)")
		q.Set("mode", "ro")
	}
	return fileURI(path, q)
}

// fileURI is the SQLite URI of the file at path, so that no character of
// the path is taken for the query.
func fileURI(path string, q url.Values) string {
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p // C:/x → file:///C:/x
	}
	return (&url.URL{Scheme: "file", Path: p, RawQuery: q.Encode()}).String()
}

// connector opens connections to one file and, when attach is set, attaches
// that file (control.db) as ctl to each: a pool may replace a connection at
// any time, and an ATTACH run on one would be missing from the next.
type connector struct {
	dsn    string
	attach string
}

func (c *connector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Driver().Open(c.dsn)
	if err != nil || c.attach == "" {
		return conn, err
	}
	// mode=rw: a missing control.db is an error, not a new empty file.
	ctl := fileURI(c.attach, url.Values{"mode": {"rw"}})
	if _, err := conn.(driver.ExecerContext).ExecContext(ctx, "ATTACH DATABASE ? AS ctl",
		[]driver.NamedValue{{Ordinal: 1, Value: ctl}}); err != nil {
		_ = conn.Close()
		// %v: whatever is wrong with control.db, it is not this file.
		return nil, fmt.Errorf("attach %s: %v", c.attach, err)
	}
	return conn, nil
}

func (*connector) Driver() driver.Driver { return &sqlite.Driver{} }

// openDB opens a pool over the file at path, attaching control.db to every
// connection when attach is set. A writer has a single connection, which
// serialises its users.
func openDB(path string, writer bool, attach string) *sql.DB {
	db := sql.OpenDB(&connector{dsn: dsn(path, writer), attach: attach})
	if writer {
		db.SetMaxOpenConns(1)
	}
	return db
}

// prepareFile creates dir (0700) and an empty file at path (0600) if there
// is none, so that SQLite, which gives -wal and -shm the main file's mode,
// creates nothing readable by others. created says whether it made the file.
func prepareFile(path string) (created bool, err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, err
	}
	f, err := os.OpenFile(path, os.O_RDONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, f.Close()
}

// IsCorrupt reports whether err says a file is damaged or not a database.
func IsCorrupt(err error) bool {
	var se *sqlite.Error
	if !errors.As(err, &se) {
		return false
	}
	switch se.Code() & 0xff {
	case sqlite3.SQLITE_CORRUPT, sqlite3.SQLITE_NOTADB:
		return true
	}
	return false
}
