package cmd

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/goptics/vartop/internal/api"
	"github.com/goptics/vartop/internal/config"
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

// serveWeb serves the dashboard of the server configured under key (a
// configuration key, not Server.Name) on the loopback address addr until ctx
// is done, then shuts down: in-flight requests get shutdownTimeout to finish,
// and the session is closed. It prints the URL to out and passes it to open,
// whose error is only printed.
func serveWeb(ctx context.Context, cfg *config.Config, key, addr string, out io.Writer, open func(string) error) error {
	ln, err := web.Listen(addr)
	if err != nil {
		return err
	}

	m := web.NewManager(cfg, api.PoolFor)
	if err := m.Switch(key); err != nil {
		_ = ln.Close()
		return err
	}
	h, err := web.NewHandler(m, web.AllowedHosts(ln))
	if err != nil {
		_ = ln.Close()
		_ = m.Close()
		return err
	}

	url := "http://" + ln.Addr().String()
	fmt.Fprintf(out, "vartop web: %s\n", url)
	if err := open(url); err != nil {
		fmt.Fprintf(out, "could not open a browser: %v\n", err)
	}

	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	select {
	case err := <-serveErr: // Serve failed on its own; it is never ErrServerClosed here
		_ = m.Close()
		return err
	case <-ctx.Done():
	}

	shutCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	shutErr := srv.Shutdown(shutCtx)
	if shutErr != nil {
		_ = srv.Close() // drop the requests that outlived the timeout
	}
	return errors.Join(shutErr, m.Close())
}

var webCmd = &cobra.Command{
	Use:   "web",
	Short: "Open the dashboard in your browser",
	// A failure here (port in use, shutdown) is not a usage mistake.
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadOrOnboard()
		if err != nil {
			return err
		}
		if _, err := cfg.GetActiveServer(serverFlag); err != nil {
			return err
		}
		// The key GetActiveServer resolved: the web session follows keys.
		key := cmp.Or(serverFlag, cfg.DefaultServer)
		startCollecting(cmd.ErrOrStderr())

		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		// Once the first signal starts the shutdown, restore the default
		// handling: a second Ctrl+C during the drain kills vartop.
		context.AfterFunc(ctx, stop)

		open := openBrowser
		if webNoOpen {
			open = func(string) error { return nil }
		}
		return serveWeb(ctx, cfg, key, webAddr, cmd.OutOrStdout(), open)
	},
}

func init() {
	webCmd.Flags().StringVar(&webAddr, "addr", "127.0.0.1:7070", "Loopback address to listen on")
	webCmd.Flags().BoolVar(&webNoOpen, "no-open", false, "Do not open the browser")
	rootCmd.AddCommand(webCmd)
}
