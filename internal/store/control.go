package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/goptics/vartop/internal/fleet"
)

// DeadAfter is how long a lease that looks fresh may go without a beat
// before its holder is taken for dead: three beats missed.
const DeadAfter = 3 * BeatEvery

// beatPoll is how often BeatAdvances reads the beat it watches.
const beatPoll = 100 * time.Millisecond

// ErrLeaseLost means the token given is not the lease's: another daemon
// claimed it, or it was released.
var ErrLeaseLost = errors.New("daemon lease lost")

// Lease is the row of the daemon collecting into the store.
type Lease struct {
	Token string // "" once released
	PID   int
	Build string // build identity of the daemon's executable
	// The daemon's environment, as far as collection depends on it.
	ConfigPath, Kubeconfig, PathEnv string
	Started, Beat                   time.Time
	Stop                            int // stop requests since the claim
}

// Control is control.db: the daemon's lease, and per server the UIs' poke
// counter and the daemon's collection error. The daemon and every UI open
// it; each writes its own fields.
type Control struct {
	db   *sql.DB
	path string
	file os.FileInfo // the file at path when it was opened
}

const controlSchema = `
CREATE TABLE IF NOT EXISTS daemon (
	id          INTEGER PRIMARY KEY CHECK (id = 1),
	token       TEXT    NOT NULL,
	pid         INTEGER NOT NULL,
	build       TEXT    NOT NULL,
	config_path TEXT    NOT NULL,
	kubeconfig  TEXT    NOT NULL,
	path_env    TEXT    NOT NULL,
	started     INTEGER NOT NULL,
	beat        INTEGER NOT NULL,
	stop        INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS servers (
	name TEXT PRIMARY KEY,
	err  TEXT    NOT NULL DEFAULT '',
	poke INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID;`

// OpenControl opens dataDir's control file, creating it (and dataDir) if
// needed.
func OpenControl(dataDir string) (*Control, error) {
	path := controlPath(dataDir)
	if _, err := prepareFile(path); err != nil {
		return nil, err
	}
	db := openDB(path, true, "")
	if _, err := db.Exec(controlSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	pinID(fi)
	return &Control{db: db, path: path, file: fi}, nil
}

// Close closes the control file.
func (c *Control) Close() error { return c.db.Close() }

// fresh reports whether a lease with token, last renewed at beat (unix
// ms), still holds at now: renewed less than LeaseTTL before now, and not
// more than 2·BeatEvery after it. A beat further ahead is no daemon's
// renewing, as clocks a little apart would make it, but a dead daemon's
// last one, read after the clock stepped back: it must not hold the lease
// for as long as the step.
func fresh(token string, beat int64, now time.Time) bool {
	age := now.Sub(time.UnixMilli(beat))
	return token != "" && age < LeaseTTL && age >= -2*BeatEvery
}

// Claim makes the caller the collecting daemon, as l (but for its Token,
// Started, Beat and Stop), unless another daemon's lease is fresh at now:
// then ok is false. The token it returns is what Beat, Release and every
// ServerDB.Append need.
func (c *Control) Claim(l Lease, now time.Time) (token string, ok bool, err error) {
	return c.claim(l, now, "")
}

// TakeOver claims the lease as Claim does, but from the daemon holding it
// with token dead too, however fresh its lease looks: one whose beat did
// not advance for DeadAfter (see BeatAdvances), so dead. ok is false when
// the lease is fresh and another's than dead: another daemon took it over
// first, or claimed it once dead's went stale.
func (c *Control) TakeOver(dead string, l Lease, now time.Time) (token string, ok bool, err error) {
	return c.claim(l, now, dead)
}

// claim is Claim, taking the lease from token dead's holder too when dead
// is not "".
func (c *Control) claim(l Lease, now time.Time, dead string) (token string, ok bool, err error) {
	tx, err := c.db.Begin() // IMMEDIATE: two claimants queue here
	if err != nil {
		return "", false, err
	}
	defer func() { _ = tx.Rollback() }()

	var cur string
	var beat int64
	switch err := tx.QueryRow(`SELECT token, beat FROM daemon WHERE id = 1`).Scan(&cur, &beat); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return "", false, err
	case fresh(cur, beat, now) && (dead == "" || cur != dead):
		return "", false, nil
	}
	token = rand.Text()
	ms := now.UnixMilli()
	if _, err := tx.Exec(`INSERT OR REPLACE INTO daemon
		(id, token, pid, build, config_path, kubeconfig, path_env, started, beat, stop)
		VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, 0)`,
		token, l.PID, l.Build, l.ConfigPath, l.Kubeconfig, l.PathEnv, ms, ms); err != nil {
		return "", false, err
	}
	if err := tx.Commit(); err != nil {
		return "", false, err
	}
	return token, true, nil
}

// Beat renews the lease of token at now, and reports whether a stop was
// requested. ErrLeaseLost: the lease is no longer token's.
func (c *Control) Beat(token string, now time.Time) (stopRequested bool, err error) {
	var stop int
	err = c.db.QueryRow(`UPDATE daemon SET beat = ? WHERE id = 1 AND token = ? AND token <> '' RETURNING stop`,
		now.UnixMilli(), token).Scan(&stop)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrLeaseLost
	}
	return stop != 0, err
}

// BeatAdvances watches the lease of token for up to within, and reports
// whether its holder renewed it meanwhile: true as soon as the lease shows
// a beat other than the one it had at first (later, or earlier after the
// clock stepped back: renewed either way), false once within passed
// without one, at once if the lease is not token's (or no longer is). now
// and sleep are the clock it waits by; a ctx done ends the wait with its
// error.
func (c *Control) BeatAdvances(ctx context.Context, token string, within time.Duration, now func() time.Time, sleep func(time.Duration)) (advanced bool, err error) {
	first, held, err := c.beatOf(token)
	if err != nil || !held {
		return false, err
	}
	deadline := now().Add(within)
	for now().Before(deadline) {
		sleep(beatPoll)
		if err := ctx.Err(); err != nil {
			return false, err
		}
		beat, held, err := c.beatOf(token)
		if err != nil || !held {
			return false, err
		}
		if beat != first {
			return true, nil
		}
	}
	return false, nil
}

// beatOf returns the beat of the lease, and whether it is token's.
func (c *Control) beatOf(token string) (beat int64, held bool, err error) {
	err = c.db.QueryRow(`SELECT beat FROM daemon WHERE id = 1 AND token = ? AND token <> ''`, token).Scan(&beat)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return beat, err == nil, err
}

// Replaced reports whether the file c opened is no longer the one at its
// path: removed (the data dir deleted, say), or another put in its place.
// A daemon whose control.db is replaced would hold a lease no UI sees,
// while another daemon claims the new file's.
func (c *Control) Replaced() (bool, error) { return replaced(c.path, c.file) }

// pinID makes fi, a stat of a file just opened, identify that file. On
// Windows os.Stat leaves the file's ID to the first os.SameFile, which
// reads it from whatever has the path by then: a file put in its place
// before Replaced first looked would go unnoticed. Elsewhere it does
// nothing.
func pinID(fi os.FileInfo) { _ = os.SameFile(fi, fi) }

// replaced reports whether the file at path is no longer file.
func replaced(path string, file os.FileInfo) (bool, error) {
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return !os.SameFile(fi, file), nil
}

// Release gives up the lease of token, so that the next daemon need not
// wait for it to go stale. Releasing a lease already lost does nothing.
func (c *Control) Release(token string) error {
	_, err := c.db.Exec(`UPDATE daemon SET token = '' WHERE id = 1 AND token = ? AND token <> ''`, token)
	return err
}

// Lease returns the last daemon's lease, and whether it is fresh: held and
// renewed less than LeaseTTL ago. With no daemon ever, it is the zero
// Lease, not fresh.
func (c *Control) Lease() (Lease, bool, error) {
	var l Lease
	var started, beat int64
	err := c.db.QueryRow(`SELECT token, pid, build, config_path, kubeconfig, path_env, started, beat, stop
		FROM daemon WHERE id = 1`).
		Scan(&l.Token, &l.PID, &l.Build, &l.ConfigPath, &l.Kubeconfig, &l.PathEnv, &started, &beat, &l.Stop)
	if errors.Is(err, sql.ErrNoRows) {
		return Lease{}, false, nil
	}
	if err != nil {
		return Lease{}, false, err
	}
	l.Started, l.Beat = time.UnixMilli(started), time.UnixMilli(beat)
	return l, fresh(l.Token, beat, time.Now()), nil
}

// DaemonState is what a UI reads of the daemon in control.db (see
// ReadDaemon), and what that makes of the snapshots the store holds: which
// newest snapshot is current, and may be acted on (R44). Both UIs judge by
// it alone.
type DaemonState struct {
	Lease Lease // the last daemon's, fresh or not; the zero Lease when none ever claimed
	Fresh bool  // a daemon holds the lease: it collects
	// Other: the lease — fresh, released or stale — is of another
	// environment than the reader's (R31). Release and a crash keep its
	// config path and KUBECONFIG: the snapshots stored under it are of
	// that configuration's servers, whose units this one's token must not
	// reach.
	Other bool
	Err   error // the lease could not be read: nothing above is known
}

// ReadDaemon reads c's lease, and checks its environment with sameEnv; nil
// takes every daemon's for the reader's. A lease no daemon ever claimed is
// of no environment.
func ReadDaemon(c *Control, sameEnv func(Lease) bool) DaemonState {
	l, fresh, err := c.Lease()
	if err != nil {
		return DaemonState{Err: err}
	}
	return DaemonState{Lease: l, Fresh: fresh, Other: !l.Started.IsZero() && sameEnv != nil && !sameEnv(l)}
}

// Running reports whether a daemon runs to answer a poke, as far as d
// knows: a lease that cannot be read counts as held, since nothing says
// the daemon is down.
func (d DaemonState) Running() bool { return d.Fresh || d.Err != nil }

// Current reports whether latest, the newest snapshot of a server polled
// every interval, is current at now (R44): not stale (fleet.Stale), the
// lease of the reader's environment, and latest taken under it — not
// before its daemon claimed it. An older one is a previous daemon's; once
// a daemon claims, the previous one's appends fail (the token check), so
// every newer one is its own. A lease that cannot be read leaves the stale
// rule alone. No snapshot (nil) is not current.
func (d DaemonState) Current(latest *fleet.Snapshot, interval time.Duration, now time.Time) bool {
	switch {
	case latest == nil || fleet.Stale(latest, interval, now):
		return false
	case d.Err != nil:
		return true
	}
	return !d.Other && !latest.At.Before(d.Lease.Started)
}

// Acts reports whether an action may be sent on latest at now (R44): a
// daemon of the reader's environment holds a fresh lease, and latest is
// current under it. A lease that cannot be read refuses it: whose units
// latest names is not known.
func (d DaemonState) Acts(latest *fleet.Snapshot, interval time.Duration, now time.Time) bool {
	return d.Err == nil && d.Fresh && d.Current(latest, interval, now)
}

// RequestStop asks the daemon holding the lease to stop: its next Beat
// says so.
func (c *Control) RequestStop() error {
	_, err := c.db.Exec(`UPDATE daemon SET stop = stop + 1 WHERE id = 1 AND token <> ''`)
	return err
}

// Poke asks the daemon to poll server now, and returns when it asked: a
// snapshot taken after the poke is newer.
func (c *Control) Poke(server string) (time.Time, error) {
	at := time.Now().Round(0)
	_, err := c.db.Exec(`INSERT INTO servers (name, poke) VALUES (?, 1)
		ON CONFLICT (name) DO UPDATE SET poke = poke + 1`, server)
	return at, err
}

// Pokes returns every server's poke counter; the daemon polls a server
// whose counter moved.
func (c *Control) Pokes() (map[string]int64, error) {
	rows, err := c.db.Query(`SELECT name, poke FROM servers`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	pokes := make(map[string]int64)
	for rows.Next() {
		var name string
		var n int64
		if err := rows.Scan(&name, &n); err != nil {
			return nil, err
		}
		pokes[name] = n
	}
	return pokes, rows.Err()
}

// SetServerErr records why server cannot be collected; "" clears it.
func (c *Control) SetServerErr(server, msg string) error {
	_, err := c.db.Exec(`INSERT INTO servers (name, err) VALUES (?, ?)
		ON CONFLICT (name) DO UPDATE SET err = excluded.err`, server, msg)
	return err
}

// ServerErrs returns the servers that have a collection error, with it.
func (c *Control) ServerErrs() (map[string]string, error) {
	rows, err := c.db.Query(`SELECT name, err FROM servers WHERE err <> ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	errs := make(map[string]string)
	for rows.Next() {
		var name, msg string
		if err := rows.Scan(&name, &msg); err != nil {
			return nil, err
		}
		errs[name] = msg
	}
	return errs, rows.Err()
}
