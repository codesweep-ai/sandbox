package cli

import (
	"fmt"

	"github.com/codesweep-ai/sandbox/internal/fcnet"
	"github.com/codesweep-ai/sandbox/internal/hostroute"
	"github.com/codesweep-ai/sandbox/internal/paths"
	"github.com/codesweep-ai/sandbox/internal/state"
	"github.com/spf13/cobra"
)

// hostRoute builds the HostRoute driver from the resolved App config. Groups
// are separate bridges on separate subnets, so the host needs one leg per
// group; the default group leads, because that is where a bare name lives.
func (a *App) hostRoute() hostroute.HostRoute {
	groups, _ := state.ListGroups(a.InstDir)
	legs := []hostroute.Leg{a.hostRouteLeg(state.DefaultGroup, "")}
	for _, g := range groups {
		if g.Name == state.DefaultGroup {
			legs[0] = a.hostRouteLeg(g.Name, g.TapPrefix)
			continue
		}
		legs = append(legs, a.hostRouteLeg(g.Name, g.TapPrefix))
	}
	return hostroute.HostRoute{
		Runner:  a.Runner,
		InstDir: a.InstDir,
		NetDir:  paths.FCNet(),
		UID:     a.Host.UID,
		Suffix:  envOr("CS_SANDBOX_DNS_SUFFIX", "cs.sandbox"),
		Legs:    legs,
	}
}

// hostRouteLeg describes one group's presence on the host plane. Each group
// keeps its own fabric dir and dnsmasq, so its names are served on its own
// subnet and nowhere else.
func (a *App) hostRouteLeg(group, tapPrefix string) hostroute.Leg {
	netDir := paths.FCNetFor(group)
	return hostroute.Leg{
		Group:     group,
		TapPrefix: tapPrefix,
		NetDir:    netDir,
		Fab: fcnet.Fabric{
			Runner: a.Runner, Network: state.NetworkName(group), Image: a.Image,
			NetDir: netDir, TapPrefix: tapPrefix,
		},
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
				fmt.Fprintf(out, "host-route: UP — reach sandboxes from the host by name under .%s:\n", hr.Suffix)
				fmt.Fprintf(out, "    ping <name>.%s                 curl http://<name>.%s:8000\n", hr.Suffix, hr.Suffix)
				fmt.Fprintf(out, "    ping <name>.<group>.%s   — members of a non-default group\n", hr.Suffix)
				fmt.Fprintf(out, "  Names auto-update as sandboxes come and go — no further sudo. (ssh still works.)\n")
				fmt.Fprintf(out, "  A group created later needs one more sudo: cs-sandbox host-route refresh\n")
				fmt.Fprintln(out, hr.Status(cmd.Context()))
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
