package store

import (
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/goptics/vartop/internal/fleet"
)

// bucketsSchema is the day's 5-minute buckets (fleet.DayBucket): each
// worker's fleet.DayStat per bucket start (unix seconds), merged (an
// empty label) and per unit label, summed as each snapshot is appended, so
// that a 24h chart reads 288 rows a view, not a day of snapshots.
const bucketsSchema = `
CREATE TABLE buckets (
	worker    TEXT    NOT NULL,
	start     INTEGER NOT NULL,
	label     TEXT    NOT NULL,
	polls     INTEGER NOT NULL,
	completed REAL    NOT NULL,
	failed    REAL    NOT NULL,
	peak_c    REAL    NOT NULL,
	peak_f    REAL    NOT NULL,
	fewest    INTEGER NOT NULL,
	out_of    INTEGER NOT NULL,
	PRIMARY KEY (worker, start, label)
) WITHOUT ROWID;`

// addBucket sums one point into its bucket: fleet.DayStat.Add, in SQL —
// the worst poll kept is the one missing the most units, a tie going to
// fewer contributing. The SET expressions read the row as it was.
const addBucket = `
INSERT INTO buckets (worker, start, label, polls, completed, failed, peak_c, peak_f, fewest, out_of)
VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?5, ?6, ?7, ?8)
ON CONFLICT (worker, start, label) DO UPDATE SET
	polls = polls + excluded.polls,
	completed = completed + excluded.completed,
	failed = failed + excluded.failed,
	peak_c = max(peak_c, excluded.peak_c),
	peak_f = max(peak_f, excluded.peak_f),
	out_of = CASE WHEN excluded.out_of - excluded.fewest > out_of - fewest
		OR excluded.out_of - excluded.fewest = out_of - fewest AND excluded.fewest < fewest
		THEN excluded.out_of ELSE out_of END,
	fewest = CASE WHEN excluded.out_of - excluded.fewest > out_of - fewest
		OR excluded.out_of - excluded.fewest = out_of - fewest AND excluded.fewest < fewest
		THEN excluded.fewest ELSE fewest END`

// addBuckets sums snap's points (fleet.DayPoints) into their bucket, in tx.
func addBuckets(tx *sql.Tx, snap *fleet.Snapshot) error {
	points := fleet.DayPoints(snap)
	if len(points) == 0 {
		return nil
	}
	stmt, err := tx.Prepare(addBucket)
	if err != nil {
		return err
	}
	defer stmt.Close()
	start := snap.At.Truncate(fleet.DayBucket).Unix()
	for _, p := range points {
		polls, c, f := 0, 0.0, 0.0
		if p.Contributing > 0 {
			polls, c, f = 1, p.Completed, p.Failed
		}
		if _, err := stmt.Exec(p.Worker, start, p.Label, polls, c, f, p.Contributing, p.Expected); err != nil {
			return err
		}
	}
	return nil
}

// Day reads worker's buckets from since on into a fleet.Day. A file whose
// writer has not made its buckets yet (one written before they were) has
// none: an empty day.
func (s *ServerDB) Day(worker string, since time.Time) (*fleet.Day, error) {
	d := fleet.NewDay(worker)
	rows, err := s.db.Query(`SELECT start, label, polls, completed, failed, peak_c, peak_f, fewest, out_of
		FROM buckets WHERE worker = ? AND start >= ?`, worker, since.Truncate(fleet.DayBucket).Unix())
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return d, nil
		}
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var start int64
		var label string
		var st fleet.DayStat
		if err := rows.Scan(&start, &label, &st.Polls, &st.Completed, &st.Failed,
			&st.PeakCompleted, &st.PeakFailed, &st.Fewest, &st.Of); err != nil {
			return nil, err
		}
		d.Put(time.Unix(start, 0), label, st)
	}
	return d, rows.Err()
}

// FillBuckets sums into the buckets up to n of the snapshots stored before
// the file had them (from the meta's filled up to its fill_to), and reports
// whether more are left. Each batch is one transaction of the server file
// alone, its progress recorded in it: a fill stopped half way resumes where
// it was, and none is summed twice, by whichever collector fills next. It
// takes no lock on control.db — unlike Append, whose transaction does — so
// a fill, however long, never keeps the lease's beat or a page's poke
// waiting. It needs no lease either: a collector fills only while it
// collects. Snapshots taken before since are passed over.
func (s *ServerDB) FillBuckets(since time.Time, n int) (more bool, err error) {
	if !s.writer {
		return false, errReadOnly
	}
	if s.filler == nil {
		s.filler = openDB(s.path, true, "") // the server file's write lock, and no other
	}
	tx, err := s.filler.Begin()
	if err != nil {
		return false, s.blame(err)
	}
	defer func() { _ = tx.Rollback() }()
	filled, to, err := fillMarks(tx)
	if err != nil || filled >= to {
		return false, err
	}
	snaps, last, err := s.unfilled(tx, filled, to, n)
	if err != nil {
		return false, err
	}
	for _, snap := range snaps {
		if !snap.At.Before(since) {
			if err := addBuckets(tx, snap); err != nil {
				return false, err
			}
		}
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO meta (k, v) VALUES ('filled', ?)`, last); err != nil {
		return false, err
	}
	return last < to, tx.Commit()
}

// fillMarks reads how far the buckets are filled, and up to where: 0, 0
// for a file that had them from its start.
func fillMarks(tx *sql.Tx) (filled, to int64, err error) {
	err = tx.QueryRow(`SELECT coalesce((SELECT v FROM meta WHERE k = 'filled'), 0),
		coalesce((SELECT v FROM meta WHERE k = 'fill_to'), 0)`).Scan(&filled, &to)
	return filled, to, err
}

// unfilled reads up to n snapshots after id after, up to id to, and the id it
// read to: to itself when there are none left (pruned meanwhile, say). A
// snapshot that cannot be decoded is passed over, as scan does.
func (s *ServerDB) unfilled(tx *sql.Tx, after, to int64, n int) ([]*fleet.Snapshot, int64, error) {
	rows, err := tx.Query(`SELECT id, at, data FROM snapshots WHERE id > ? AND id <= ? ORDER BY id LIMIT ?`, after, to, n)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var snaps []*fleet.Snapshot
	last := after
	for rows.Next() {
		var id, at int64
		var data []byte
		if err := rows.Scan(&id, &at, &data); err != nil {
			return nil, 0, err
		}
		last = id
		if snap, err := decodeSnapshot(at, data); err == nil {
			snaps = append(snaps, snap)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	if len(snaps) == 0 && last == after {
		last = to
	}
	return snaps, last, nil
}

// checkLease fails with ErrLeaseLost unless token holds the lease, read in
// tx (control.db attached as ctl).
func checkLease(tx *sql.Tx, token string) error {
	var holder string
	switch err := tx.QueryRow(`SELECT token FROM ctl.daemon WHERE id = 1`).Scan(&holder); {
	case errors.Is(err, sql.ErrNoRows):
		return ErrLeaseLost
	case err != nil:
		return ctlErr(err)
	case holder == "" || holder != token:
		return ErrLeaseLost
	}
	return nil
}
