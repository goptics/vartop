package cmd

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	neturl "net/url"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/goptics/vartop/internal/collect"
	"github.com/goptics/vartop/internal/config"
	"github.com/goptics/vartop/internal/store"
	"github.com/goptics/vartop/internal/web"
)

var (
	webAddr   string
	webNoOpen bool
)

// shutdownTimeout is how long in-flight requests get to finish on shutdown.
const shutdownTimeout = 5 * time.Second

// openBrowser asks the OS to open url in the default browser. It starts the
// opener and does not wait for it; the opener is reaped in the background,
// so no zombie is left.
func openBrowser(url string) error {
	var c *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		c = exec.Command("open", url)
	case "windows":
		c = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		c = exec.Command("xdg-open", url)
	}
	if err := c.Start(); err != nil {
		return err
	}
	go func() { _ = c.Wait() }()
	return nil
}

// serveWeb serves the web UI over src on the loopback address addr until
// ctx is done, then shuts down: in-flight requests get shutdownTimeout to
// finish. It prints the URL to out and passes it to open, whose error is
// only printed. With key, a configuration key (not Server.Name), the URL
// opens that server's dashboard, else the default server's; a key that is
// not configured is an error.
func serveWeb(ctx context.Context, src web.Source, key, addr string, out io.Writer, open func(string) error) error {
	if key != "" {
		cfg, err := src.Config()
		if err != nil {
			return err
		}
		if _, err := cfg.GetActiveServer(key); err != nil {
			return err
		}
	}
	ln, err := web.Listen(addr)
	if err != nil {
		return err
	}
	h, err := web.NewHandler(src, web.AllowedHosts(ln))
	if err != nil {
		_ = ln.Close()
		return err
	}

	url := "http://" + ln.Addr().String()
	if key != "" {
		url += "/?server=" + neturl.QueryEscape(key)
	}
	fmt.Fprintf(out, "vartop web: %s\n", url)
	if err := open(url); err != nil {
		fmt.Fprintf(out, "could not open a browser: %v\n", err)
	}

	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	select {
	case err := <-serveErr: // Serve failed on its own; it is never ErrServerClosed here
		return err
	case <-ctx.Done():
	}

	shutCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	err = srv.Shutdown(shutCtx)
	if err != nil {
		_ = srv.Close() // drop the requests that outlived the timeout
	}
	return err
}

// webSource is what vartop web reads and acts through: the store of this
// vartop's environment, the config read again on every request (the
// collector follows its changes too), and the check of the lease against
// this environment (R31). release closes it.
func webSource() (src web.Source, release func(), err error) {
	dataDir, err := store.DataDir()
	if err != nil {
		return src, nil, err
	}
	ctl, err := store.OpenControl(dataDir)
	if err != nil {
		return src, nil, err
	}
	return web.Source{DataDir: dataDir, Config: config.Load, Control: ctl, SameEnv: sameEnvAsOwn()},
		func() { _ = ctl.Close() }, nil
}

// collectRetry is the least time between two starts of the collector: how
// long vartop web waits to collect again after it failed, or ended at once.
const collectRetry = 5 * time.Second

// collectLoop collects (run, a collect.Run) until ctx ends. Run returns
// when another vartop holds the lease and beats (once its beat moves, or
// it takes the lease over if the beat stays still for store.DeadAfter),
// when it loses the lease, or when it fails; the
// loop calls it again, at least retry after the last start — so that a
// failure is retried without spinning, and this vartop web collects once
// the other one stops. It logs, once while each lasts, a failure and who
// else collects (lease, the lease as Control.Lease reads it).
func collectLoop(ctx context.Context, run func(context.Context) (int, error), lease func() (store.Lease, bool, error), retry time.Duration, log *slog.Logger) {
	last := "" // what was logged last, so that it is logged once
	for ctx.Err() == nil {
		began := time.Now()
		code, err := run(ctx)
		if ctx.Err() != nil {
			return
		}
		switch l, fresh, lerr := lease(); {
		case err != nil || code != 0:
			if msg := fmt.Sprintf("failed: %v", err); msg != last {
				last = msg
				log.Error("cannot collect; retrying", "err", err, "code", code, "every", retry)
			}
		case lerr == nil && fresh:
			if msg := "other: " + l.Token; msg != last {
				last = msg
				log.Info("another vartop is collecting into this data directory; this one shows what it collects, and takes over once it stops", "pid", l.PID)
			}
		default:
			last = ""
		}
		sleep(ctx, time.Until(began.Add(retry)))
	}
}

// sleep waits d, or until ctx ends.
func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// runWeb serves the web UI on addr (serveWeb) and, until it stops,
// collects every configured server into the store (collectLoop), logging
// to log. Once the web stops, collection stops too and lets the lease go.
func runWeb(ctx context.Context, key, addr string, out io.Writer, open func(string) error, log *slog.Logger) error {
	src, release, err := webSource()
	if err != nil {
		return err
	}
	defer release()

	collectCtx, stopCollecting := context.WithCancel(ctx)
	collected := make(chan struct{})
	go func() {
		defer close(collected)
		o := collect.Options{DataDir: src.DataDir, Config: readConfig, Lease: ownLease(), Log: log}
		run := func(ctx context.Context) (int, error) { return collect.Run(ctx, o) }
		collectLoop(collectCtx, run, src.Control.Lease, collectRetry, log)
	}()
	err = serveWeb(ctx, src, key, addr, out, open)
	stopCollecting()
	<-collected
	return err
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
	return l.Build == want.Build && sameEnv(l, want)
}

// sameEnv reports whether the daemons of leases a and b collect in the same
// environment: the same config path and KUBECONFIG, whatever their build
// (or PATH). A UI acts on a server only while the daemon collects in its
// environment (R31): the token it sends must not go to endpoints
// discovered under another config.
func sameEnv(a, b store.Lease) bool {
	return a.ConfigPath == b.ConfigPath && a.Kubeconfig == b.Kubeconfig
}

// sameEnvAsOwn returns the UIs' check of a lease (R31): whether its daemon
// collects in this vartop's environment, sameEnv with ownLease (read
// once). The UIs check the lease whether it is fresh, released or stale:
// the snapshots stored under it are of its daemon's environment (R44).
func sameEnvAsOwn() func(store.Lease) bool {
	own := ownLease()
	return func(l store.Lease) bool { return sameEnv(l, own) }
}

var webCmd = &cobra.Command{
	Use:   "web",
	Short: "Open the dashboard in your browser",
	// A failure here (port in use, shutdown) is not a usage mistake.
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		if _, err := loadOrOnboard(); err != nil {
			return err
		}
		store.SetLogger(slog.Default()) // as web logs: through the log package, to stderr

		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		// Once the first signal starts the shutdown, restore the default
		// handling: a second Ctrl+C during the drain kills vartop.
		context.AfterFunc(ctx, stop)

		open := openBrowser
		if webNoOpen {
			open = func(string) error { return nil }
		}
		return runWeb(ctx, serverFlag, webAddr, cmd.OutOrStdout(), open, slog.Default())
	},
}

func init() {
	webCmd.Flags().StringVar(&webAddr, "addr", "127.0.0.1:7070", "Loopback address to listen on")
	webCmd.Flags().BoolVar(&webNoOpen, "no-open", false, "Do not open the browser")
	rootCmd.AddCommand(webCmd)
}
