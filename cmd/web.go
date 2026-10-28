package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/goptics/vartop/internal/collect"
	"github.com/goptics/vartop/internal/config"
	"github.com/goptics/vartop/internal/store"
	"github.com/goptics/vartop/internal/web"
)

var (
	webHost   string
	webPort   int
	webNoOpen bool
)

// listenAddr is the address vartop web listens on: host, and port from 0
// (any free port) to 65535.
func listenAddr(host string, port int) (string, error) {
	if port < 0 || port > 65535 {
		return "", fmt.Errorf("--port %d: want 0 (any free port) to 65535", port)
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

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
// else collects (lease, the lease as Control.Lease reads it). It returns
// nil once ctx ends, or Run's collect.ErrDataDirRemoved: what vartop web
// reads is gone then.
func collectLoop(ctx context.Context, run func(context.Context) (int, error), lease func() (store.Lease, bool, error), retry time.Duration, log *slog.Logger) error {
	last := "" // what was logged last, so that it is logged once
	for ctx.Err() == nil {
		began := time.Now()
		code, err := run(ctx)
		switch {
		case ctx.Err() != nil:
			return nil
		case errors.Is(err, collect.ErrDataDirRemoved):
			return err
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
	return nil
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
// The data directory deleted under it stops the web too, with that error,
// whether it collects or only shows what another collects: its pages would
// go on reading the deleted files.
func runWeb(ctx context.Context, key, addr string, out io.Writer, open func(string) error, log *slog.Logger) error {
	src, release, err := webSource()
	if err != nil {
		return err
	}
	defer release()

	serveCtx, stopServing := context.WithCancel(ctx)
	defer stopServing()
	collectCtx, stopCollecting := context.WithCancel(ctx)
	collected := make(chan struct{})
	var collectErr error
	go func() {
		defer close(collected)
		o := collect.Options{DataDir: src.DataDir, Config: readConfig, Lease: ownLease(), Log: log}
		run := func(ctx context.Context) (int, error) {
			// The pages read the control.db opened at the start: deleted,
			// a Run would make a new one they never see (and the data
			// directory anew), so this vartop web stops as its collector
			// does.
			if gone, _ := src.Control.Replaced(); gone {
				return 1, fmt.Errorf("%w: %s no longer holds the control.db this vartop web opened", collect.ErrDataDirRemoved, src.DataDir)
			}
			return collect.Run(ctx, o)
		}
		if collectErr = collectLoop(collectCtx, run, src.Control.Lease, collectRetry, log); collectErr != nil {
			stopServing()
		}
	}()
	err = serveWeb(serveCtx, src, key, addr, out, open)
	stopCollecting()
	<-collected
	if collectErr != nil {
		return fmt.Errorf("%w; start vartop web again", collectErr)
	}
	return err
}

// ownLease is this vartop as the lease records its collector: its pid, and
// the environment collection depends on.
func ownLease() store.Lease {
	path, _ := config.Path() // "" when there is no config dir: the collector reads no config either
	return store.Lease{PID: os.Getpid(), ConfigPath: path, Kubeconfig: os.Getenv("KUBECONFIG")}
}

// readConfig is the collector's Options.Config: the config, and where its
// file is even when it cannot be read, so that the collector watches it
// still.
func readConfig() (*config.Config, string, error) {
	path, err := config.Path()
	if err != nil {
		return nil, "", err
	}
	cfg, err := config.Load()
	return cfg, path, err
}

// sameEnv reports whether the collectors of leases a and b collect in the
// same environment: the same config path and KUBECONFIG. vartop web acts
// on a server only while the collector collects in its environment (R31):
// the token it sends must not go to endpoints discovered under another
// config.
func sameEnv(a, b store.Lease) bool {
	return a.ConfigPath == b.ConfigPath && a.Kubeconfig == b.Kubeconfig
}

// sameEnvAsOwn returns vartop web's check of a lease (R31): whether its
// collector collects in this vartop's environment, sameEnv with ownLease
// (read once). It checks the lease whether it is fresh, released or stale:
// the snapshots stored under it are of its collector's environment (R44).
func sameEnvAsOwn() func(store.Lease) bool {
	own := ownLease()
	return func(l store.Lease) bool { return sameEnv(l, own) }
}

var webCmd = &cobra.Command{
	Use:   "web",
	Short: "Open the dashboard in your browser; collects every server while it runs",
	Long: `vartop web serves the dashboard on --host and --port (loopback only: the
dashboard has no login) and opens it in your browser. While it runs it
collects every configured server into the data directory, keeping 24h of
polls and each worker's rates for the server's history (1d to 30d); when it
stops, collection stops too.`,
	// A failure here (port in use, shutdown) is not a usage mistake.
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		addr, err := listenAddr(webHost, webPort)
		if err != nil {
			return err
		}
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
		return runWeb(ctx, serverFlag, addr, cmd.OutOrStdout(), open, slog.Default())
	},
}

func init() {
	webCmd.Flags().StringVar(&webHost, "host", "127.0.0.1", "Loopback host to listen on (the dashboard has no login)")
	webCmd.Flags().IntVar(&webPort, "port", 7070, "Port to listen on (0: any free port)")
	webCmd.Flags().BoolVar(&webNoOpen, "no-open", false, "Do not open the browser")
	rootCmd.AddCommand(webCmd)
}
