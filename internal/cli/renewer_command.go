package cli

import (
	"log/slog"
	"os/signal"
	"syscall"

	"github.com/codesweep-ai/sandbox/internal/renew"
	"github.com/spf13/cobra"
)

// newRenewerCmd runs the credential renewer in the foreground.
//
// Almost nobody types it. `create --lend-agent-login` starts one detached on
// first use, the way `forward` starts its ssh child, and the last `destroy` in a
// group stops it. It is a command rather than a private helper for two reasons:
// the detached child has to be able to re-exec this binary as something, and a
// host that keeps sandboxes up on purpose — a CI machine lending a seat — wants
// something a service manager can supervise and signal.
func newRenewerCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "renewer",
		Short: "Keep lent host logins from expiring while sandboxes borrow them",
		Long: "Runs a real turn through an agent's own client shortly before that agent's login expires, " +
			"so a sandbox borrowing it does not lose its credential partway through a long run.\n\n" +
			"The renewer never refreshes a credential itself: it asks the client that owns the login to, " +
			"and re-reads the file that client rewrites.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			level := slog.LevelInfo
			if app.Verbose {
				level = slog.LevelDebug
			}
			log := slog.New(slog.NewTextHandler(app.stderr(), &slog.HandlerOptions{Level: level}))

			// A signal has to land while the renewer is sleeping between ticks,
			// which is almost all of the time, so the context is the only way it
			// stops promptly.
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			// The same configuration create's one-shot renewal uses, from one
			// builder: the two have to agree about every path, above all the
			// host-global lock directory that stops them running a client at the
			// same moment.
			cfg := app.renewerConfig()
			cfg.Log = log
			return renew.Run(ctx, cfg)
		},
	}
}
