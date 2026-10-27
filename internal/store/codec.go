package store

import (
	"bytes"
	"compress/flate"
	"encoding/json"
	"io"
	"math"
	"sync"
	"time"

	"github.com/goptics/vartop/internal/fleet"
)

// The record types are the stored shape of a snapshot: private, so that a
// change to fleet's types never changes what is on disk unnoticed, and with
// short field names, since they are in every row. A change to them is a new
// StoreVersion.

// snapshotRec is a fleet.Snapshot but for its At, which is a column. Source
// came after StoreVersion 1 without a new version: rows from before it
// read with none, which no action takes for current.
type snapshotRec struct {
	Units  []unitRec      `json:"u,omitempty"`
	Behind map[string]int `json:"b,omitempty"`
	Err    string         `json:"e,omitempty"`
	Source string         `json:"src,omitempty"`
}

// unitRec is a fleet.PodSnapshot.
type unitRec struct {
	Key       string               `json:"k"`
	Via       string               `json:"v,omitempty"`
	Instance  string               `json:"i,omitempty"`
	Label     string               `json:"l,omitempty"`
	Balanced  bool                 `json:"bl,omitempty"`
	Err       string               `json:"e,omitempty"`
	Workers   map[string]sampleRec `json:"w"` // null: no list (an errored unit), not an empty one
	Unreached []string             `json:"x,omitempty"`
}

// sampleRec is a fleet.Sample, filed under its worker's name. The fields a
// sample nearly always shares with its unit are left out when it does.
type sampleRec struct {
	Name        *string   `json:"nm,omitempty"` // nil: the name it is filed under
	At          int64     `json:"t"`            // ms after the snapshot's At (or before: negative)
	Status      string    `json:"s,omitempty"`
	Concurrency int       `json:"c,omitempty"`
	Processing  int       `json:"pr,omitempty"`
	Pending     int       `json:"pe,omitempty"`
	Idle        int       `json:"id,omitempty"`
	Instance    *string   `json:"i,omitempty"` // nil: the unit's Instance
	Origin      *string   `json:"o,omitempty"` // nil: the unit's Via
	Submitted   uint64    `json:"su,omitempty"`
	Completed   uint64    `json:"co,omitempty"`
	Successful  uint64    `json:"ok,omitempty"`
	Failed      uint64    `json:"fa,omitempty"`
	Rates       []float64 `json:"r,omitempty"` // completed/s, failed/s to 3 dp; absent unless RateOK
}

// labelsRec is a fleet.LabelState.
type labelsRec struct {
	Numbers  map[string]numberRec `json:"n"`
	Followed map[string]string    `json:"f"`
}

type numberRec struct {
	N    int   `json:"n"`
	Seen int64 `json:"s"` // unix ms; 0 for the zero time
}

// round3 rounds a rate to 3 decimal places, what the UIs show at most.
func round3(r float64) float64 { return math.Round(r*1000) / 1000 }

// ifDiffers is &v when v differs from the default def, else nil.
func ifDiffers(v, def string) *string {
	if v == def {
		return nil
	}
	return &v
}

func orDefault(v *string, def string) string {
	if v == nil {
		return def
	}
	return *v
}

func unixMilli(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func fromUnixMilli(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

var (
	flateWriters = sync.Pool{New: func() any {
		w, _ := flate.NewWriter(nil, flate.BestSpeed) // only fails for a bad level
		return w
	}}
	flateReaders = sync.Pool{New: func() any { return flate.NewReader(nil) }}
)

// encodeSnapshot is the blob stored for s: its record, as JSON, deflated.
func encodeSnapshot(s *fleet.Snapshot) ([]byte, error) {
	rec := snapshotRec{Behind: s.Behind, Err: s.Err, Source: s.Source}
	if len(s.Pods) > 0 {
		rec.Units = make([]unitRec, len(s.Pods))
	}
	at := s.At.UnixMilli()
	for i, p := range s.Pods {
		u := unitRec{Key: p.Key, Via: p.Via, Instance: p.Instance, Label: p.Label, Balanced: p.Balanced,
			Err: p.Err, Unreached: p.Unreached}
		if p.Workers != nil {
			u.Workers = make(map[string]sampleRec, len(p.Workers))
		}
		for name, sm := range p.Workers {
			w := sampleRec{
				Name: ifDiffers(sm.Name, name), At: sm.At.UnixMilli() - at,
				Status: sm.Status, Concurrency: sm.Concurrency,
				Processing: sm.NumProcessing, Pending: sm.NumPending, Idle: sm.NumIdleWorkers,
				Instance: ifDiffers(sm.Instance, p.Instance), Origin: ifDiffers(sm.Origin, p.Via),
				Submitted: sm.Metrics.Submitted, Completed: sm.Metrics.Completed,
				Successful: sm.Metrics.Successful, Failed: sm.Metrics.Failed,
			}
			if c, f := round3(sm.CompletedRate), round3(sm.FailedRate); sm.RateOK && finite(c) && finite(f) {
				w.Rates = []float64{c, f}
			}
			u.Workers[name] = w
		}
		rec.Units[i] = u
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	zw := flateWriters.Get().(*flate.Writer)
	defer flateWriters.Put(zw)
	zw.Reset(&buf)
	if _, err := zw.Write(data); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// finite reports whether a rate can be stored: JSON has no NaN or ±Inf, and
// a rate that is either is not one.
func finite(r float64) bool { return !math.IsNaN(r) && !math.IsInf(r, 0) }

// decodeSnapshot is the snapshot taken at atMs (unix ms) stored as data.
func decodeSnapshot(atMs int64, data []byte) (*fleet.Snapshot, error) {
	zr := flateReaders.Get().(io.ReadCloser)
	defer flateReaders.Put(zr)
	if err := zr.(flate.Resetter).Reset(bytes.NewReader(data), nil); err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		return nil, err
	}
	var rec snapshotRec
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, err
	}

	s := &fleet.Snapshot{At: time.UnixMilli(atMs), Behind: rec.Behind, Err: rec.Err, Source: rec.Source}
	if len(rec.Units) > 0 {
		s.Pods = make([]fleet.PodSnapshot, len(rec.Units))
	}
	for i, u := range rec.Units {
		p := fleet.PodSnapshot{Key: u.Key, Via: u.Via, Instance: u.Instance, Label: u.Label, Balanced: u.Balanced,
			Err: u.Err, Unreached: u.Unreached}
		if u.Workers != nil {
			p.Workers = make(map[string]*fleet.Sample, len(u.Workers))
		}
		for name, w := range u.Workers {
			sm := &fleet.Sample{At: time.UnixMilli(atMs + w.At)}
			sm.Name, sm.Status, sm.Concurrency = orDefault(w.Name, name), w.Status, w.Concurrency
			sm.NumProcessing, sm.NumPending, sm.NumIdleWorkers = w.Processing, w.Pending, w.Idle
			sm.Instance, sm.Origin = orDefault(w.Instance, u.Instance), orDefault(w.Origin, u.Via)
			sm.Metrics.Submitted, sm.Metrics.Completed = w.Submitted, w.Completed
			sm.Metrics.Successful, sm.Metrics.Failed = w.Successful, w.Failed
			if len(w.Rates) == 2 {
				sm.CompletedRate, sm.FailedRate, sm.RateOK = w.Rates[0], w.Rates[1], true
			}
			p.Workers[name] = sm
		}
		s.Pods[i] = p
	}
	return s, nil
}

// encodeLabels is the blob stored for a label state: JSON, small enough
// as it is.
func encodeLabels(l fleet.LabelState) ([]byte, error) {
	rec := labelsRec{Numbers: make(map[string]numberRec, len(l.Numbers)), Followed: l.Followed}
	for k, n := range l.Numbers {
		rec.Numbers[k] = numberRec{N: n.N, Seen: unixMilli(n.Seen)}
	}
	return json.Marshal(rec)
}

func decodeLabels(data []byte) (fleet.LabelState, error) {
	var rec labelsRec
	if err := json.Unmarshal(data, &rec); err != nil {
		return fleet.LabelState{}, err
	}
	l := fleet.LabelState{Numbers: make(map[string]fleet.LabelNumber, len(rec.Numbers)), Followed: rec.Followed}
	for k, n := range rec.Numbers {
		l.Numbers[k] = fleet.LabelNumber{N: n.N, Seen: fromUnixMilli(n.Seen)}
	}
	if l.Followed == nil {
		l.Followed = make(map[string]string)
	}
	return l, nil
}
