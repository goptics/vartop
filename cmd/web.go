package cmd

import (
	"context"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/spf13/cobra"

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
// daemon follows its changes too), and ensureDaemon to start the daemon.
// release closes it.
func webSource() (src web.Source, release func(), err error) {
	dataDir, err := store.DataDir()
	if err != nil {
		return src, nil, err
	}
	ctl, err := store.OpenControl(dataDir)
	if err != nil {
		return src, nil, err
	}
	return web.Source{DataDir: dataDir, Config: config.Load, Control: ctl, Start: ensureLocalDaemon},
		func() { _ = ctl.Close() }, nil
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
		startCollecting(cmd.ErrOrStderr())
		src, release, err := webSource()
		if err != nil {
			return err
		}
		defer release()

		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		// Once the first signal starts the shutdown, restore the default
		// handling: a second Ctrl+C during the drain kills vartop.
		context.AfterFunc(ctx, stop)

		open := openBrowser
		if webNoOpen {
			open = func(string) error { return nil }
		}
		return serveWeb(ctx, src, serverFlag, webAddr, cmd.OutOrStdout(), open)
	},
}

func init() {
	webCmd.Flags().StringVar(&webAddr, "addr", "127.0.0.1:7070", "Loopback address to listen on")
	webCmd.Flags().BoolVar(&webNoOpen, "no-open", false, "Do not open the browser")
	rootCmd.AddCommand(webCmd)
}
