package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"github.com/codesweep-ai/sandbox/internal/fcnet"
	"github.com/codesweep-ai/sandbox/internal/paths"
	"github.com/codesweep-ai/sandbox/internal/ports"
	"github.com/codesweep-ai/sandbox/internal/state"
	"github.com/spf13/cobra"
	"path/filepath"
)

// A group owns an isolated network, its own SSH trust material and a gateway.
// Membership is the boundary: members reach each other, members of different
// groups neither resolve nor connect to one another, and — because the keys are
// per group — could not authenticate even if some future routing change made
// them reachable.

func newGroupCmd(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "group",
		Short: "Manage sandbox groups (isolated network + SSH trust + gateway)",
		Long: `Manage sandbox groups (isolated network + SSH trust + gateway).

Groups are optional. Without --group every sandbox joins one called "default",
reachable from every other, which is all most setups need.

Use a group when unrelated fleets share this host and must not see each other:
each gets its own network, its own SSH keys and its own gateway, so members of
different groups cannot resolve, reach or authenticate to one another.

  cs-sandbox create api --group cache-redis  # creates the group if needed
  cs-sandbox exec api.cache-redis ls         # identity is (group, name)
  cs-sandbox group rm cache-redis -f         # -f destroys its sandboxes too

A bare name always means the default group, never "whichever group has it".`,
	}
	cmd.AddCommand(newGroupCreateCmd(app), newGroupLsCmd(app), newGroupRmCmd(app))
	return cmd
}

func newGroupCreateCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "create <group>",
		Short: "Create a group and its network, keys and gateway",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			g, err := app.ensureGroup(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "group %s ready (network=%s, keys=%s)\n",
				g.Name, state.NetworkName(g.Name), paths.GroupKeys(g.Name))
			if g.GWPort != 0 {
				fmt.Fprintf(out, "  gateway: ssh port %d — members are reachable as <name>.%s\n", g.GWPort, g.Name)
			}
			return nil
		},
	}
}

// groupItem is the stable shape of `group ls --json`: the inventory a tool
// reads. It exists because the alternative is parsing the table below, or —
// worse — matching the prose of an error to find out whether a group is there
// at all, which is what `ls --json` was added to stop for sandboxes.
type groupItem struct {
	Name    string `json:"name"`
	Network string `json:"network"`
	Gateway int    `json:"gateway,omitempty"`
	Members int    `json:"members"`
	Created string `json:"created,omitempty"`
}

func newGroupLsCmd(app *App) *cobra.Command {
	var quiet, asJSON bool
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List groups",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if quiet && asJSON {
				return fmt.Errorf("--quiet and --json are mutually exclusive")
			}
			groups, err := state.ListGroups(app.InstDir)
			if err != nil {
				return err
			}
			// Names only: the scripting form `ls -q` gives for sandboxes, one level
			// up, so `group ls -q | xargs -n1 cs-sandbox group rm -f` works. A group
			// name is already the whole reference — nothing to qualify it with — and
			// the member count nothing here prints is skipped along with it.
			if quiet {
				for _, g := range groups {
					fmt.Fprintln(cmd.OutOrStdout(), g.Name)
				}
				return nil
			}
			insts, _ := state.List(app.InstDir)
			members := map[string]int{}
			for _, in := range insts {
				members[in.Group]++
			}
			if asJSON {
				items := make([]groupItem, 0, len(groups))
				for _, g := range groups {
					items = append(items, groupItem{
						Name: g.Name, Network: state.NetworkName(g.Name),
						Gateway: g.GWPort, Members: members[g.Name], Created: g.Created,
					})
				}
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(items)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "GROUP\tMEMBERS\tNETWORK\tGATEWAY\tAGE")
			for _, g := range groups {
				gw := "-"
				if g.GWPort != 0 {
					gw = fmt.Sprintf("%d", g.GWPort)
				}
				fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\n",
					g.Name, members[g.Name], state.NetworkName(g.Name), gw, age(g.Created, time.Now()))
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "print only group names, one per line (for scripting)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print stable machine-readable JSON")
	return cmd
}

func newGroupRmCmd(app *App) *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "rm <group>",
		Short: "Remove an empty group and its network, keys and gateway",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return app.removeGroup(cmd.Context(), args[0], force, cmd.OutOrStdout())
		},
	}
	cmd.Flags().BoolVarP(&force, "force", "f", false, "destroy the group's sandboxes first")
	return cmd
}

// ensureGroup creates a group's record and artifacts if they are missing, and
// returns the record either way. `create --group X` calls this too, so a group
// never has to be declared before it is used.
func (a *App) ensureGroup(ctx context.Context, group string) (*state.Group, error) {
	if err := state.ValidGroup(group); err != nil {
		return nil, err
	}
	if g, err := state.LoadGroup(a.InstDir, group); err == nil {
		return g, a.ensureGroupArtifacts(ctx, g)
	}
	g := &state.Group{Name: group, Created: time.Now().UTC().Format(time.RFC3339)}
	prefix, err := a.allocTapPrefix(group)
	if err != nil {
		return nil, err
	}
	g.TapPrefix = prefix
	port, err := a.allocGatewayPort()
	if err != nil {
		return nil, err
	}
	g.GWPort = port
	if err := a.ensureGroupArtifacts(ctx, g); err != nil {
		return nil, err
	}
	if err := state.SaveGroup(a.InstDir, g); err != nil {
		return nil, err
	}
	return g, nil
}

// ensureGroupArtifacts brings the network and trust material up. It is
// idempotent so a group whose network was removed underneath it recovers on the
// next create rather than failing.
func (a *App) ensureGroupArtifacts(ctx context.Context, g *state.Group) error {
	d := a.engineDepsFor(g.Name)
	if err := d.EnsureNetwork(ctx); err != nil {
		return err
	}
	if err := d.EnsureTierKeys(ctx); err != nil {
		return err
	}
	return a.ensureGateway(ctx, g)
}

// ensureGateway brings up the group's keepalive/gateway. The container pins the
// bridge and, published on one host port, is the ssh jump host into the group:
// through it the host reaches members by their bare names over the group's own
// DNS, exactly as members reach each other.
func (a *App) ensureGateway(ctx context.Context, g *state.Group) error {
	if g.GWPort == 0 {
		return nil
	}
	seedDir := filepath.Join(state.GroupDir(a.InstDir, g.Name), ".gateway", "seed")
	if err := os.MkdirAll(seedDir, 0o700); err != nil {
		return err
	}
	// Only the group's own user key opens the gateway, so it is an entrance to
	// this group and nothing else.
	pub, err := os.ReadFile(filepath.Join(paths.GroupKeys(g.Name), "id_cs-sandbox_user.pub"))
	if err != nil {
		return fmt.Errorf("gateway: group %s has no user key yet: %w", g.Name, err)
	}
	if err := os.WriteFile(filepath.Join(seedDir, "authorized_keys"), pub, 0o600); err != nil {
		return err
	}
	fab := fcnet.Fabric{
		Runner: a.Runner, Network: state.NetworkName(g.Name), Image: a.Image,
		NetDir: paths.FCNetFor(g.Name), TapPrefix: g.TapPrefix,
		GWPort: g.GWPort, GWBind: a.SSHBind, GWSeed: seedDir,
		GWUser: a.Host.User, GWUID: a.Host.UID, GWGID: a.Host.GID,
		GWHome: filepath.Join("/home", a.Host.User),
	}
	return fab.EnsureGateway(ctx)
}

// allocGatewayPort takes the first free port in 2400-2499. Gateways use their
// own range so a group's ingress can never collide with a sandbox's published
// SSH port (2200-2399).
func (a *App) allocGatewayPort() (int, error) {
	taken := map[int]bool{}
	groups, _ := state.ListGroups(a.InstDir)
	for _, g := range groups {
		taken[g.GWPort] = true
	}
	return ports.Alloc(2400, 2499, taken, ports.LoopbackBusy)
}

// allocTapPrefix hands out the lowest unused two-byte tap prefix. Linux
// interface names are host-global, so this is allocated and recorded rather
// than hashed from the group name: a hash collision between two groups would
// surface as an interface-name clash a long way from its cause.
func (a *App) allocTapPrefix(group string) (string, error) {
	taken := map[string]bool{}
	groups, _ := state.ListGroups(a.InstDir)
	for _, g := range groups {
		taken[g.TapPrefix] = true
	}
	for i := 0; i < 0x10000; i++ {
		p := fmt.Sprintf("fd%04x", i)
		if !taken[p] {
			return p, nil
		}
	}
	return "", fmt.Errorf("no free tap prefix left for group %q", group)
}

// removeGroup tears a group down. It refuses while members exist unless forced,
// in which case the members are destroyed first — the group owns its artifacts,
// so leaving them behind would strand a network and a key pair.
func (a *App) removeGroup(ctx context.Context, group string, force bool, out io.Writer) error {
	if err := state.ValidGroup(group); err != nil {
		return err
	}
	if _, err := state.LoadGroup(a.InstDir, group); err != nil {
		return fmt.Errorf("no such group %q", group)
	}
	insts, _ := state.List(a.InstDir)
	var members []*state.Instance
	for _, in := range insts {
		if in.Group == group {
			members = append(members, in)
		}
	}
	if len(members) > 0 && !force {
		return fmt.Errorf("group %q still has %d sandbox(es); destroy them or pass -f", group, len(members))
	}
	for _, in := range members {
		eng, _, err := a.engineFor(Ref(in))
		if err != nil {
			return err
		}
		if err := eng.Remove(ctx, in.Name, true); err != nil {
			return fmt.Errorf("destroy %s: %w", Ref(in), err)
		}
		fmt.Fprintf(out, "destroyed %s\n", Ref(in))
	}

	// The group's fabric is host-global state, not part of the instances dir:
	// leaving its dnsmasq running would keep a resolver on an address podman is
	// free to hand to the NEXT group, and that group's fabric would then refuse
	// to start because a stranger already owns its DNS address.
	if group != state.DefaultGroup {
		a.hostRouteLeg(group, "").Fab.Down(ctx)
		if err := os.RemoveAll(paths.FCNetFor(group)); err != nil {
			return err
		}
	}
	d := a.engineDepsFor(group)
	d.RemoveGateway(ctx)
	d.ReclaimNetwork(ctx)
	if err := os.RemoveAll(paths.GroupKeys(group)); err != nil {
		return err
	}
	if err := os.RemoveAll(state.GroupDir(a.InstDir, group)); err != nil {
		return err
	}
	if err := a.syncSSHConfig(); err != nil {
		return err
	}
	fmt.Fprintf(out, "removed group %s\n", group)
	return nil
}
