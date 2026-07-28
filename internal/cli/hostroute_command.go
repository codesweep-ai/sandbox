package cli

import (
	"fmt"

	"github.com/codesweep-ai/sandbox/internal/fcnet"
	"github.com/codesweep-ai/sandbox/internal/hostroute"
	"github.com/codesweep-ai/sandbox/internal/paths"
	"github.com/spf13/cobra"
)

// hostRoute builds the HostRoute driver from the resolved App config.
func (a *App) hostRoute() hostroute.HostRoute {
	netDir := paths.FCNet()
	return hostroute.HostRoute{
		Fab:     fcnet.Fabric{Runner: a.Runner, Network: a.Network, Image: a.Image, NetDir: netDir},
		Runner:  a.Runner,
		InstDir: a.InstDir,
		NetDir:  netDir,
		UID:     a.Host.UID,
		Network: a.Network,
		Suffix:  envOr("CS_SANDBOX_DNS_SUFFIX", "cs.sandbox"),
	}
}

// refreshHostRoute republishes names when host-route is on (rootless lifecycle
// hook after create/start/stop/rm/destroy). Best-effort.
func (a *App) refreshHostRoute(cmd *cobra.Command) {
	a.hostRoute().RefreshIfActive(cmd.Context())
}

func newHostRouteCmd(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:       "host-route up|down|refresh|status",
		Short:     "Optional: reach sandboxes from the host by name (Linux-only, one-time sudo)",
		Args:      cobra.MaximumNArgs(1),
		ValidArgs: []string{"up", "down", "refresh", "status"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if app.Host.IsMacOS {
				return fmt.Errorf("host-route is Linux-only (the rootless network fabric is Linux/firecracker)")
			}
			sub := "status"
			if len(args) == 1 {
				sub = args[0]
			}
			hr := app.hostRoute()
			out := cmd.OutOrStdout()
			switch sub {
			case "up":
				if err := hr.Up(cmd.Context()); err != nil {
					return err
				}
				prefix := hr.Fab.Prefix(cmd.Context())
				fmt.Fprintf(out, "host-route: UP — reach sandboxes from the host by name under .%s:\n", hr.Suffix)
				fmt.Fprintf(out, "    ping <name>.%s        curl http://<name>.%s:8000\n", hr.Suffix, hr.Suffix)
				fmt.Fprintf(out, "  Names auto-update as sandboxes come and go — no further sudo. (ssh <name> still works.)\n")
				fmt.Fprintf(out, "  Host is on the sandbox network at %s.%d; DNS via %s.\n", prefix, 251, hr.Fab.DNSIP(cmd.Context()))
			case "down":
				hr.Down(cmd.Context())
				fmt.Fprintln(out, "host-route: DOWN — reverted the resolver, removed the veth and the published names.")
			case "refresh":
				if err := hr.Refresh(cmd.Context()); err != nil {
					return err
				}
				fmt.Fprintln(out, "host-route: refreshed the veth, resolver, and published names.")
			case "status":
				fmt.Fprintln(out, hr.Status(cmd.Context()))
			default:
				return fmt.Errorf("usage: cs-sandbox host-route up|down|refresh|status")
			}
			return nil
		},
	}
	return cmd
}
