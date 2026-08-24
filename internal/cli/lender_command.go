package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/codesweep-ai/sandbox/internal/lend"
	"github.com/codesweep-ai/sandbox/internal/paths"
	"github.com/spf13/cobra"
)

// newLenderCmd runs the credential lender in the foreground.
//
// Most people never type it: `create --lend-…` starts one on first use, the way
// `forward` starts its ssh child, and it stops when the last loan is destroyed.
// The command exists for the host that keeps a lender up on purpose — a CI
// machine lending a seat to runners of its own — where a service manager needs
// something that stays in the foreground and can be stopped by a signal.
func newLenderCmd(app *App) *cobra.Command {
	var addr string
	var origins []string
	cmd := &cobra.Command{
		Use:   "lender",
		Short: "Run the credential lender in the foreground",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			o, err := parseOrigins(origins)
			if err != nil {
				return err
			}
			return runLender(cmd, app, addr, o)
		},
	}
	cmd.Flags().StringVar(&addr, "addr", envOr("CS_SANDBOX_LEND_ADDR", lend.DefaultBind),
		"listen address; must not be loopback, because a sandbox reaches the host on its ordinary side")
	cmd.Flags().StringArrayVar(&origins, "origin", nil,
		"send one slot's traffic somewhere else: SLOT=URL, for a gateway or a recorder in front of the provider (repeatable)")
	return cmd
}

// parseOrigins reads the SLOT=URL overrides. An unknown slot is an error rather
// than a line that quietly does nothing.
func parseOrigins(args []string) (map[string]string, error) {
	if len(args) == 0 {
		return nil, nil
	}
	out := map[string]string{}
	for _, a := range args {
		slot, u, ok := strings.Cut(a, "=")
		if !ok || slot == "" || u == "" {
			return nil, fmt.Errorf("--origin %q: use SLOT=URL", a)
		}
		if _, known := lend.SlotByID(slot); !known {
			return nil, fmt.Errorf("--origin %q: unknown slot %q", a, slot)
		}
		if _, err := url.Parse(u); err != nil {
			return nil, fmt.Errorf("--origin %q: %w", a, err)
		}
		out[slot] = u
	}
	return out, nil
}

func runLender(cmd *cobra.Command, app *App, addr string, origins map[string]string) error {
	level := slog.LevelInfo
	if app.Verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(app.stderr(), &slog.HandlerOptions{Level: level}))

	srv := lend.New(lend.Config{
		Home:      paths.AgentLoginHome(app.Host.Home),
		KeysDir:   lend.KeysDir(paths.AgentLoginHome(app.Host.Home)),
		Loans:     lend.NewFileLoans(app.InstDir),
		Log:       log,
		LocalOnly: true,
		Origins:   origins,
	})

	// The listener opens before anything is reported, so a port already in use
	// fails the command rather than leaving a caller waiting on a lender that
	// never came up.
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	defer l.Close()

	if host, _, err := net.SplitHostPort(l.Addr().String()); err == nil {
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			fmt.Fprintf(app.stderr(),
				"cs-sandbox: warning: %s is loopback, and a sandbox cannot reach it — it arrives on this host's ordinary side\n", addr)
		}
	}
	log.Info("lending", slog.String("listen", l.Addr().String()),
		slog.String("instances", app.InstDir), slog.Any("fronting", lend.Origins()))

	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	hs := &http.Server{Handler: srv, ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelError)}
	errc := make(chan error, 1)
	go func() {
		if err := hs.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
			return
		}
		errc <- nil
	}()

	select {
	case <-ctx.Done():
		// A second signal now means "stop whatever is in flight"; while
		// NotifyContext is armed it would be swallowed and the drain below
		// would read as a hang.
		stop()
	case err := <-errc:
		if err != nil {
			return err
		}
	}
	drainLender(hs)
	fmt.Fprintln(cmd.OutOrStdout(), srv.Snapshot().Summary())
	return nil
}

// drainLender waits for the requests still being answered. A model composing a
// reply is the long case, and cutting one off costs the caller the whole turn.
func drainLender(hs *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), lenderDrain)
	defer cancel()
	_ = hs.Shutdown(ctx)
}

// lenderDrain is a var only so a test can shorten it.
var lenderDrain = 30 * time.Second
