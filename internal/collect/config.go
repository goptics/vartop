package collect

import (
	"encoding/json"
	"errors"
	"maps"
	"os"
	"slices"
	"time"

	"github.com/goptics/vartop/internal/config"
)

// configWatch reads the config again when its file changes, and keeps the
// last good one.
type configWatch struct {
	read func() (*config.Config, string, error)

	path    string         // where the last read said the file is
	stat    fileStat       // of the file before the last read
	statted bool           // stat is that of path
	good    *config.Config // the last config read without an error; nil before the first
	err     string         // the last read error, "" after a good read
}

// fileStat is what tells a config file changed.
type fileStat struct {
	exists bool
	size   int64
	mod    time.Time
}

func statFile(path string) fileStat {
	fi, err := os.Stat(path)
	if err != nil {
		return fileStat{}
	}
	return fileStat{exists: true, size: fi.Size(), mod: fi.ModTime()}
}

// next reads the config if this is the first read or the file changed
// since the last one, and returns it when it is good. The file is statted
// before it is read, so that a write between the two is read next time. A
// read that fails keeps the last good config, and is returned as err only
// when its error is new. No config file at all is a good config of no
// servers: deleting it must stop the use of its tokens (the servers'
// files stay for RemovedGrace).
func (w *configWatch) next() (cfg *config.Config, err error) {
	if w.path != "" {
		st := statFile(w.path)
		if w.statted && st == w.stat {
			return nil, nil
		}
		w.stat, w.statted = st, true
	}
	cfg, path, err := w.read()
	if errors.Is(err, config.ErrConfigNotFound) {
		cfg, err = &config.Config{}, nil
	}
	if path != w.path {
		w.path, w.statted = path, false // stat it next time, and read it again then
	}
	if err != nil {
		if msg := err.Error(); msg != w.err {
			w.err = msg
			return nil, err
		}
		return nil, nil
	}
	w.err, w.good = "", cfg
	return cfg, nil
}

// reload applies the config if it changed: the collectors of servers
// removed stop, those of servers added start, and those of servers changed
// in any way (endpoints, interval, token…) restart; the others are left
// alone. Servers started together are staggered over their interval.
func (d *holder) reload() {
	cfg, err := d.conf.next()
	if err != nil {
		if d.conf.good == nil {
			d.o.Log.Error("config error; no servers to collect until it is fixed", "err", err)
		} else {
			d.o.Log.Error("config error; collecting with the last good config", "err", err)
		}
	}
	if cfg == nil {
		return
	}

	encoded := make(map[string]string, len(cfg.Servers))
	for name, srv := range cfg.Servers {
		data, _ := json.Marshal(srv) // a config.Server always encodes
		encoded[name] = string(data)
	}
	var starts []string
	for _, name := range slices.Sorted(maps.Keys(d.collectors)) {
		c := d.collectors[name]
		switch enc, ok := encoded[name]; {
		case !ok:
			d.o.Log.Info("server removed", "server", name)
		case enc != c.enc:
			d.o.Log.Info("server restarted", "server", name)
			starts = append(starts, name)
		default:
			continue
		}
		c.cancel()
		delete(d.collectors, name)
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.Servers)) {
		if _, running := d.collectors[name]; !running && !slices.Contains(starts, name) {
			d.o.Log.Info("server added", "server", name)
			starts = append(starts, name)
		}
	}
	slices.Sort(starts)
	for i, name := range starts {
		srv := cfg.Servers[name]
		delay := time.Duration(i) * srv.PollInterval() / time.Duration(len(starts))
		d.start(name, srv, encoded[name], delay)
	}
}
