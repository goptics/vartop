package cmd

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/goptics/vartop/internal/api"
	"github.com/goptics/vartop/internal/config"
	"github.com/goptics/vartop/internal/daemon"
	"github.com/goptics/vartop/internal/fleet"
	"github.com/goptics/vartop/internal/store"
)

const (
	// startWait is how long a daemon just started gets to claim the lease.
	startWait = 3 * time.Second
	// stopWait is how long a daemon asked to stop gets to let the lease go
	// before it is killed.
	stopWait = 5 * time.Second
	// leasePoll is how often the lease is read while waiting on it.
	leasePoll = 100 * time.Millisecond
	// suspectAfter is how old the beat of a fresh lease may be before
	// ensureDaemon takes its daemon for possibly dead (R42): two beats
	// missed.
	suspectAfter = 2 * store.BeatEvery
	// logMaxBytes is the size past which the daemon log is renamed to
	// daemon.log.1 when a daemon starts.
	logMaxBytes = 5 << 20
)

// stoppedMsg is what `vartop daemon stop` says once the daemon stopped.
const stoppedMsg = "vartop daemon stopped. It starts again at your next login or the next time you open vartop."

// notRespondingMsg is `vartop daemon stop`'s error when the daemon neither
// stopped nor beat (notResponding).
const notRespondingMsg = "the vartop daemon is not responding; its lease expires within 15s"

// exitError ends vartop with code, printing err when there is one.
type exitError struct {
	code int
	err  error
}

func (e exitError) Error() string {
	if e.err == nil {
		return fmt.Sprintf("exit status %d", e.code)
	}
	return e.err.Error()
}

func (e exitError) Unwrap() error { return e.err }

// executable is the absolute path of the running vartop.
func executable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Abs(exe)
}

// buildID identifies the executable vartop runs from: its absolute path,
// size and modification time. A vartop that finds a daemon of another
// build running restarts it.
func buildID() string {
	exe, err := executable()
	if err != nil {
		return ""
	}
	fi, err := os.Stat(exe)
	if err != nil {
		return exe
	}
	return fmt.Sprintf("%s (%d bytes, modified %s)", exe, fi.Size(), fi.ModTime().UTC().Format(time.RFC3339Nano))
}

// ownLease is this vartop as a daemon's lease records it: its build, and
// the environment collection depends on.
func ownLease() store.Lease {
	path, _ := config.Path() // "" when there is no config dir: the daemon reads no config either
	return store.Lease{
		PID:        os.Getpid(),
		Build:      buildID(),
		ConfigPath: path,
		Kubeconfig: os.Getenv("KUBECONFIG"),
		PathEnv:    os.Getenv("PATH"),
	}
}

// readConfig is the daemon's Options.Config: the config, and where its file
// is even when it cannot be read, so that the daemon watches it still.
func readConfig() (*config.Config, string, error) {
	path, err := config.Path()
	if err != nil {
		return nil, "", err
	}
	cfg, err := config.Load()
	return cfg, path, err
}

// sameDaemon reports whether the daemon of lease l collects as want would:
// the same build, config path and KUBECONFIG. PATH is not compared, so
// that ordinary differences between shells restart nothing.
func sameDaemon(l, want store.Lease) bool {
	return l.Build == want.Build && l.ConfigPath == want.ConfigPath && l.Kubeconfig == want.Kubeconfig
}

func daemonLogPath(dataDir string) string { return filepath.Join(dataDir, "daemon.log") }

// rotateIdleLog renames dataDir's daemon log to daemon.log.1 when it has
// grown past logMaxBytes, but only while no daemon holds a fresh lease in
// ctl: the log of a daemon collecting is never renamed under it.
func rotateIdleLog(ctl *store.Control, dataDir string) {
	if _, fresh, err := ctl.Lease(); err != nil || fresh {
		return
	}
	path := daemonLogPath(dataDir)
	if fi, err := os.Stat(path); err == nil && fi.Size() > logMaxBytes {
		_ = os.Rename(path, path+".1") // fails on Windows while a daemon writes it: it grows on then
	}
}

// openLog opens the daemon log at path for appending.
func openLog(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
}

// isTerminal reports whether f is a terminal, or another character device
// such as /dev/null: none that matters where it is asked.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// daemonLogger logs to the data dir's daemon.log, whoever started the
// daemon, and to stderr too when that is a terminal; to stderr alone when
// the log cannot be opened.
func daemonLogger(dataDir string) (*slog.Logger, func()) {
	path := daemonLogPath(dataDir)
	f, err := openLog(path)
	if err != nil {
		log := slog.New(slog.NewTextHandler(os.Stderr, nil))
		log.Warn("cannot open the daemon log", "file", path, "err", err)
		return log, func() {}
	}
	var w io.Writer = f
	if isTerminal(os.Stderr) {
		w = io.MultiWriter(f, os.Stderr)
	}
	return slog.New(slog.NewTextHandler(w, nil)), func() { _ = f.Close() }
}

// spawnDetached starts exe with args as a daemon of its own (see
// detached), working in dataDir (an absolute path), with no stdin and its
// stdout and stderr appended to dataDir's daemon.log. It does not wait for
// it, but reaps it should it end while vartop runs.
func spawnDetached(exe string, args []string, dataDir string) error {
	log, err := openLog(daemonLogPath(dataDir))
	if err != nil {
		return err
	}
	defer log.Close() // the daemon has its own
	c := exec.Command(exe, args...)
	// Not vartop's working dir, which the daemon would keep from being
	// removed or unmounted; a relative VARTOP_DATA_DIR would now name
	// another dir, so it gets the absolute one.
	c.Dir = dataDir
	if os.Getenv("VARTOP_DATA_DIR") != "" {
		c.Env = append(os.Environ(), "VARTOP_DATA_DIR="+dataDir)
	}
	c.Stdout, c.Stderr = log, log
	c.SysProcAttr = detached()
	if err := c.Start(); err != nil {
		return err
	}
	go func() { _ = c.Wait() }()
	return nil
}

// killPID kills the process pid; one already gone is no error.
func killPID(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := p.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

// ensureOptions are what ensureDaemon and the daemon commands act
// through.
type ensureOptions struct {
	dataDir string
	ctl     *store.Control
	want    store.Lease         // this vartop's build and environment (ownLease)
	sup     supervisor          // the login service
	exe     string              // what the login service runs
	env     map[string]string   // what the login service keeps of this vartop's environment
	spawn   func() error        // starts a detached daemon
	kill    func(pid int) error // ends a daemon that does not stop when asked
	now     func() time.Time    // time.Now when nil
	sleep   func(time.Duration) // time.Sleep when nil
	warn    io.Writer           // where launchDaemon's warning goes; nil drops it
}

func (o ensureOptions) withDefaults() ensureOptions {
	if o.now == nil {
		o.now = time.Now
	}
	if o.sleep == nil {
		o.sleep = time.Sleep
	}
	return o
}

// localDaemon is this vartop's view of its daemon, over the data dir of its
// environment; release closes it. Tests replace it.
var localDaemon = func() (o ensureOptions, release func(), err error) {
	dataDir, err := store.DataDir()
	if err != nil {
		return o, nil, err
	}
	exe, err := executable()
	if err != nil {
		return o, nil, err
	}
	ctl, err := store.OpenControl(dataDir)
	if err != nil {
		return o, nil, err
	}
	return ensureOptions{
		dataDir: dataDir,
		ctl:     ctl,
		want:    ownLease(),
		sup:     newSupervisor(runCommand),
		exe:     exe,
		env:     withDataDir(serviceEnv(os.Environ()), dataDir),
		spawn: func() error {
			rotateIdleLog(ctl, dataDir)
			return spawnDetached(exe, []string{"daemon", "run"}, dataDir)
		},
		kill: killPID,
	}, func() { _ = ctl.Close() }, nil
}

// withDataDir is the login service's environment env with
// VARTOP_DATA_DIR, when set, made dataDir, the absolute dir it names: the
// service manager runs the daemon in a working dir of its own, where a
// relative one names another dir (as with spawnDetached).
func withDataDir(env map[string]string, dataDir string) map[string]string {
	if env["VARTOP_DATA_DIR"] != "" {
		env["VARTOP_DATA_DIR"] = dataDir
	}
	return env
}

// ensureDaemon makes sure a daemon collects for this vartop: one of its
// build, config path and KUBECONFIG (the UI's environment wins). With none
// running it starts one; with another one running (or one stopping) it
// stops that and starts its own. A daemon is started through the login
// service when that is installed — Install rewrites it with this vartop's
// executable and environment, and starts it — else as a detached process;
// ensureDaemon waits up to startWait for it to claim the lease.
//
// A daemon it would keep whose beat is more than suspectAfter old may have
// crashed (R42): it starts one anyway, without stopping that one. The new
// daemon takes a dead one's lease over, or leaves a live one its lease and
// exits 0 (daemon.Run); a daemon then beating again is no failure.
func ensureDaemon(o ensureOptions) error {
	o = o.withDefaults()
	l, fresh, err := o.ctl.Lease()
	if err != nil {
		return err
	}
	if fresh && l.Stop == 0 && sameDaemon(l, o.want) {
		if o.now().Sub(l.Beat) <= suspectAfter {
			return nil
		}
		if _, err := launchDaemon(o, l.Token); err != nil {
			if cur, fresh, lerr := o.ctl.Lease(); lerr == nil && fresh && cur.Token == l.Token && !cur.Beat.Equal(l.Beat) {
				return nil // it was alive, only slow to beat
			}
			return err
		}
		return nil
	}
	if fresh {
		// A daemon that neither stops nor beats is left alone
		// (notResponding): the one launched takes its lease over.
		if l, _, err = stopDaemon(o); err != nil {
			return err
		}
	}
	_, err = launchDaemon(o, l.Token)
	return err
}

// launchDaemon starts a daemon and returns its lease once it claimed it:
// the first lease other than that of token old. With the login service
// installed, Install alone starts it — unless Install fails, or no daemon
// claims the lease after it: then a detached daemon is spawned (R32), so
// that a broken service manager never means no daemon. The service's
// failure is a warning (o.warn) once the spawned daemon claimed the lease,
// part of the error should it not.
func launchDaemon(o ensureOptions, old string) (store.Lease, error) {
	o = o.withDefaults()
	if !o.sup.Installed() {
		return spawnDaemon(o, old)
	}
	svcErr := o.sup.Install(o.exe, o.env)
	if svcErr == nil {
		l, err := waitStarted(o, old)
		if err == nil {
			return l, nil
		}
		svcErr = err
	}
	l, err := spawnDaemon(o, old)
	if err != nil {
		return store.Lease{}, errors.Join(fmt.Errorf("vartop daemon's login service: %w", svcErr), err)
	}
	if o.warn != nil {
		fmt.Fprintf(o.warn, "vartop daemon started detached; its login service failed: %v\n", svcErr)
	}
	return l, nil
}

// spawnDaemon spawns a detached daemon, and returns its lease once it
// claimed it (see waitStarted).
func spawnDaemon(o ensureOptions, old string) (store.Lease, error) {
	if err := o.spawn(); err != nil {
		return store.Lease{}, fmt.Errorf("start the vartop daemon: %w", err)
	}
	return waitStarted(o, old)
}

// waitStarted waits up to startWait for a fresh lease other than that of
// token old: old's daemon, stale but not stopped, may renew its lease yet on
// waking from a suspend. With old's lease fresh when it starts waiting, it
// waits store.DeadAfter longer: the new daemon watches old's beat that long
// before it takes the lease of a crashed daemon over (daemon.Run).
func waitStarted(o ensureOptions, old string) (store.Lease, error) {
	deadline := o.now().Add(startWait)
	for first := true; ; first = false {
		l, fresh, err := o.ctl.Lease()
		if err == nil && fresh && l.Token != old {
			return l, nil
		}
		if first && err == nil && fresh {
			deadline = deadline.Add(store.DeadAfter)
		}
		if !o.now().Before(deadline) {
			return store.Lease{}, fmt.Errorf("vartop daemon did not start; see %s", daemonLogPath(o.dataDir))
		}
		o.sleep(leasePoll)
	}
}

// stopDaemon asks the daemon holding a fresh lease to stop, and waits up to
// stopWait for it to let the lease go. One that does not is killed only
// while its lease is fresh and its beat moves (R33): a daemon beating is
// alive, so the pid of its lease is still its own, not one reused since
// it crashed. Its lease is let go for it, so that the next daemon need not
// wait for it to go stale. One whose beat does not move for
// store.DeadAfter is left alone (notResponding): its lease goes stale
// within LeaseTTL, and a daemon started meanwhile takes it over
// (daemon.Run). It returns the lease it found, and how the daemon stopped.
func stopDaemon(o ensureOptions) (store.Lease, stopped, error) {
	o = o.withDefaults()
	l, fresh, err := o.ctl.Lease()
	if err != nil || !fresh {
		return l, notRunning, err
	}
	if err := o.ctl.RequestStop(); err != nil {
		return l, notRunning, err
	}
	deadline := o.now().Add(stopWait)
	for {
		gone, err := leaseGone(o.ctl, l.Token)
		if err != nil || gone {
			return l, stoppedAsked, err
		}
		if !o.now().Before(deadline) {
			break
		}
		o.sleep(leasePoll)
	}
	beating, err := o.ctl.BeatAdvances(context.Background(), l.Token, store.DeadAfter, o.now, o.sleep)
	if err != nil {
		return l, notRunning, err
	}
	switch gone, err := leaseGone(o.ctl, l.Token); {
	case err != nil:
		return l, notRunning, err
	case gone: // it stopped while watched
		return l, stoppedAsked, nil
	case !beating:
		return l, notResponding, nil
	}
	if err := o.kill(l.PID); err != nil {
		return l, notRunning, fmt.Errorf("vartop daemon (pid %d) did not stop: %w", l.PID, err)
	}
	return l, stoppedKilled, o.ctl.Release(l.Token)
}

// leaseGone reports whether the daemon of token no longer holds a fresh
// lease: it let it go, or it went stale.
func leaseGone(ctl *store.Control, token string) (bool, error) {
	cur, fresh, err := ctl.Lease()
	if err != nil {
		return false, err
	}
	return !fresh || cur.Token != token, nil
}

// stopped is how stopDaemon went.
type stopped int

const (
	notRunning    stopped = iota // none was stopped: none held a fresh lease, or the error says why
	stoppedAsked                 // the daemon stopped when asked
	stoppedKilled                // it did not, and was killed
	notResponding                // it neither stopped nor beat: left alone, dead or hung
)

// installService installs the login service, with this vartop's executable
// and environment, and returns the lease of the daemon it started. A daemon
// running is stopped first: it would keep the lease, unsupervised. Should
// Install fail, that daemon is started again, detached: a failed install
// does not stop collection.
func installService(o ensureOptions) (store.Lease, error) {
	o = o.withDefaults()
	old, how, err := stopDaemon(o)
	if err != nil {
		return store.Lease{}, err
	}
	if err := o.sup.Install(o.exe, o.env); err != nil {
		if how == notRunning {
			return store.Lease{}, err
		}
		if _, spawnErr := spawnDaemon(o, old.Token); spawnErr != nil {
			return store.Lease{}, errors.Join(err, spawnErr)
		}
		return store.Lease{}, fmt.Errorf("%w (the vartop daemon runs detached meanwhile)", err)
	}
	return waitStarted(o, old.Token)
}

// startCollecting makes sure the daemon collects (ensureDaemon) before a UI
// opens. A UI works without it, so a failure is printed to w, not
// returned; so is a warning (see launchDaemon).
func startCollecting(w io.Writer) {
	if err := ensureLocal(w); err != nil {
		fmt.Fprintln(w, err)
	}
}

// ensureLocalDaemon is ensureDaemon over this vartop's daemon (see
// localDaemon): what a UI starts the daemon with once it runs (web's Start
// collector, the TUI's r), dropping a warning: the daemon runs.
func ensureLocalDaemon() error { return ensureLocal(nil) }

// ensureLocal is ensureDaemon over this vartop's daemon, warning to warn
// (nil drops a warning).
func ensureLocal(warn io.Writer) error {
	o, closeCtl, err := localDaemon()
	if err != nil {
		return err
	}
	defer closeCtl()
	o.warn = warn
	return ensureDaemon(o)
}

// offerLoginService asks, once onboarding saved the first config, whether
// the daemon should start at login, and installs the login service on yes.
// It never fails onboarding: what goes wrong is printed.
func offerLoginService(ask func() (bool, error)) {
	o, closeCtl, err := localDaemon()
	if err != nil {
		fmt.Printf("Could not set up the vartop daemon: %v\n", err)
		return
	}
	defer closeCtl()
	if o.sup.Installed() {
		return
	}
	if yes, err := ask(); err != nil || !yes {
		return
	}
	if _, err := installService(o); err != nil {
		fmt.Printf("Could not install the vartop daemon's login service: %v\n", err)
		return
	}
	fmt.Println("✓ The vartop daemon starts at login.")
}

// writeStatus prints the daemon's state — and, while it runs, its pid,
// uptime, build and environment — then the store's and, for each server
// of this vartop's config, its newest snapshot. It reports whether the
// daemon runs.
func writeStatus(w io.Writer, o ensureOptions) (running bool, err error) {
	o = o.withDefaults()
	l, running, err := o.ctl.Lease()
	if err != nil {
		return false, err
	}
	now := o.now()
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if running {
		fmt.Fprintln(tw, "vartop daemon is running")
		fmt.Fprintf(tw, "  pid\t%d\n", l.PID)
		fmt.Fprintf(tw, "  uptime\t%s\n", now.Sub(l.Started).Round(time.Second))
		fmt.Fprintf(tw, "  build\t%s\n", l.Build)
		fmt.Fprintf(tw, "  config\t%s\n", l.ConfigPath)
		fmt.Fprintf(tw, "  KUBECONFIG\t%s\n", cmp.Or(l.Kubeconfig, "(unset)"))
		fmt.Fprintf(tw, "  PATH\t%s\n", l.PathEnv)
	} else {
		fmt.Fprintln(tw, "vartop daemon is not running")
	}
	fmt.Fprintf(tw, "  data\t%s (%s)\n", o.dataDir, formatBytes(dirSize(o.dataDir)))
	fmt.Fprintf(tw, "  log\t%s\n", daemonLogPath(o.dataDir))
	service := "not installed"
	if o.sup.Installed() {
		service = "installed"
	}
	fmt.Fprintf(tw, "  login service\t%s\n", service)
	if err := tw.Flush(); err != nil {
		return running, err
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(w, "\nservers: %v\n", err)
		return running, nil
	}
	errs, err := o.ctl.ServerErrs()
	if err != nil {
		return running, err
	}
	fmt.Fprintln(w, "\nservers:")
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, name := range slices.Sorted(maps.Keys(cfg.Servers)) {
		age, units, msg := serverStatus(o.dataDir, name, now)
		row := []string{"  " + name, age, units, cmp.Or(errs[name], msg)}
		for row[len(row)-1] == "" {
			row = row[:len(row)-1] // no padding after the last cell
		}
		fmt.Fprintln(tw, strings.Join(row, "\t"))
	}
	return running, tw.Flush()
}

// serverStatus describes the newest snapshot stored for the server
// configured under key: how old it is, its units (and how many are
// healthy, when not all), and its error.
func serverStatus(dataDir, key string, now time.Time) (age, units, msg string) {
	db, err := store.OpenServer(dataDir, key, false)
	if errors.Is(err, fs.ErrNotExist) {
		return "no data", "", ""
	}
	if err != nil {
		return "no data", "", err.Error()
	}
	defer db.Close()
	snap, err := db.Latest()
	if err != nil {
		return "no data", "", err.Error()
	}
	if snap == nil {
		return "no data", "", ""
	}
	h := fleet.Health(snap)
	units = fmt.Sprintf("%d units", h.Total)
	if h.Total == 1 {
		units = "1 unit"
	}
	if h.Healthy < h.Total {
		units += fmt.Sprintf(", %d healthy", h.Healthy)
	}
	return now.Sub(snap.At).Round(time.Second).String() + " ago", units, snap.Err
}

// dirSize is the size of the files under dir.
func dirSize(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

// formatBytes is n in binary units: "512 B", "1.5 KiB", "12.0 MiB".
func formatBytes(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	div, unit := int64(1024), 0
	for m := n / 1024; m >= 1024 && unit < 4; m /= 1024 {
		div *= 1024
		unit++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[unit])
}

// withDaemon is a command's RunE that runs fn with this vartop's view of
// its daemon, printing to the command's output.
func withDaemon(fn func(o ensureOptions, w io.Writer) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, _ []string) error {
		o, closeCtl, err := localDaemon()
		if err != nil {
			return err
		}
		defer closeCtl()
		return fn(o, cmd.OutOrStdout())
	}
}

var daemonCmd = &cobra.Command{
	Use:   "daemon",
	Short: "Run or manage the vartop daemon, which collects every server's history",
	Long: `The vartop daemon polls every configured server into vartop's history, which
vartop and vartop web read. vartop starts it when it is not running; the login
service (vartop daemon install) starts it at login too, and again after a crash.

vartop daemon alone runs it in the foreground, as vartop daemon run does.`,
}

// daemonRunEnv are the NAME=VALUE pairs `daemon run --env` sets first.
var daemonRunEnv []string

var daemonRunCmd = &cobra.Command{
	Use:   "run",
	Short: "Collect in the foreground",
	Long: `Collect in the foreground until stopped (Ctrl+C, vartop daemon stop), unless
another daemon already collects. The log goes to daemon.log in the data dir,
and to the terminal too.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if err := applyEnv(daemonRunEnv); err != nil {
			return err
		}
		signal.Ignore(syscall.SIGHUP)
		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		code, err := runDaemon(ctx)
		if code != 0 {
			return exitError{code, err}
		}
		return err
	},
}

// applyEnv sets each NAME=VALUE of pairs in vartop's environment; NAME=
// unsets NAME.
func applyEnv(pairs []string) error {
	for _, kv := range pairs {
		name, value, ok := strings.Cut(kv, "=")
		var err error
		switch {
		case !ok:
		case value == "":
			err = os.Unsetenv(name)
		default:
			err = os.Setenv(name, value)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// runDaemon collects into the data dir of vartop's environment until ctx
// ends or the daemon has to stop (daemon.Run), logging as daemonLogger
// says.
func runDaemon(ctx context.Context) (int, error) {
	dataDir, err := store.DataDir()
	if err != nil {
		return 1, err
	}
	if ctl, err := store.OpenControl(dataDir); err == nil {
		rotateIdleLog(ctl, dataDir)
		_ = ctl.Close()
	}
	log, closeLog := daemonLogger(dataDir)
	defer closeLog()
	code, err := daemon.Run(ctx, daemon.Options{
		DataDir:   dataDir,
		Config:    readConfig,
		BuildPool: api.PoolFor,
		Lease:     ownLease(),
		Now:       time.Now,
		Log:       log,
	})
	if err != nil {
		log.Error("vartop daemon failed", "err", err)
	}
	return code, err
}

var daemonStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the daemon unless it is running",
	Long: `Start the daemon unless it is running. One of another build, config path or
KUBECONFIG is restarted with this vartop's, as vartop and vartop web do.`,
	RunE: withDaemon(func(o ensureOptions, w io.Writer) error {
		o.warn = w
		if err := ensureDaemon(o); err != nil {
			return err
		}
		l, _, err := o.ctl.Lease()
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "vartop daemon is running (pid %d)\n", l.PID)
		return nil
	}),
}

var daemonStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the daemon until your next login or the next time you open vartop",
	RunE: withDaemon(func(o ensureOptions, w io.Writer) error {
		_, how, err := stopDaemon(o)
		switch {
		case err != nil:
			return err
		case how == notRunning:
			fmt.Fprintln(w, "vartop daemon is not running")
			return nil
		case how == notResponding:
			return errors.New(notRespondingMsg)
		case how == stoppedKilled && o.sup.Installed():
			// The service manager restarts a daemon killed by a signal.
			if err := o.sup.Stop(); err != nil {
				return fmt.Errorf("vartop daemon killed, but its login service may start it again: %w", err)
			}
		}
		fmt.Fprintln(w, stoppedMsg)
		return nil
	}),
}

var daemonRestartCmd = &cobra.Command{
	Use:   "restart",
	Short: "Restart the daemon with this vartop's build and environment",
	RunE: withDaemon(func(o ensureOptions, w io.Writer) error {
		o.warn = w
		old, _, err := stopDaemon(o)
		if err != nil {
			return err
		}
		l, err := launchDaemon(o, old.Token)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "vartop daemon restarted (pid %d)\n", l.PID)
		return nil
	}),
}

var daemonStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show the daemon's state and each server's newest data (exit code 3 when it is not running)",
	RunE: withDaemon(func(o ensureOptions, w io.Writer) error {
		running, err := writeStatus(w, o)
		if err == nil && !running {
			return exitError{code: 3}
		}
		return err
	}),
}

var daemonInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Start the daemon at login, and again after a crash (a login service)",
	RunE: withDaemon(func(o ensureOptions, w io.Writer) error {
		l, err := installService(o)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "vartop daemon installed: it starts at login (running now, pid %d)\n", l.PID)
		return nil
	}),
}

var daemonUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Remove the login service; vartop then starts the daemon itself",
	RunE: withDaemon(func(o ensureOptions, w io.Writer) error {
		if !o.sup.Installed() {
			fmt.Fprintln(w, "vartop daemon's login service is not installed")
			return nil
		}
		// Gracefully, so that the daemon lets the lease go: the service
		// manager may kill it outright.
		if _, _, err := stopDaemon(o); err != nil {
			return err
		}
		if err := o.sup.Uninstall(); err != nil {
			return err
		}
		fmt.Fprintln(w, "vartop daemon's login service removed. vartop starts the daemon itself the next time you open it.")
		return nil
	}),
}

func init() {
	daemonCmd.RunE = daemonRunCmd.RunE
	for _, c := range []*cobra.Command{daemonCmd, daemonRunCmd} {
		c.Flags().StringArrayVar(&daemonRunEnv, "env", nil, "set NAME=VALUE in the daemon's environment first (NAME= unsets it)")
		_ = c.Flags().MarkHidden("env") // the Windows login service's way to pass its environment
	}
	for _, c := range []*cobra.Command{daemonCmd, daemonRunCmd, daemonStartCmd, daemonStopCmd, daemonRestartCmd,
		daemonStatusCmd, daemonInstallCmd, daemonUninstallCmd} {
		c.Args = cobra.NoArgs
		// Not usage mistakes; Execute prints the error, and exits with
		// an exitError's code.
		c.SilenceUsage, c.SilenceErrors = true, true
		if c != daemonCmd {
			daemonCmd.AddCommand(c)
		}
	}
	rootCmd.AddCommand(daemonCmd)
}
