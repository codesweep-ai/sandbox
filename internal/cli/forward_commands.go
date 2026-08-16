package cli

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/codesweep-ai/sandbox/internal/forward"
	"github.com/codesweep-ai/sandbox/internal/paths"
	"github.com/codesweep-ai/sandbox/internal/state"
	"github.com/spf13/cobra"
)

func newForwardCmd(app *App) *cobra.Command {
	var bind string
	var socks int
	cmd := &cobra.Command{
		Use:               "forward <name> [HOSTPORT:]VMPORT... | --socks [PORT]",
		Short:             "Forward host ports into a sandbox over SSH",
		Args:              cobra.MinimumNArgs(1),
		ValidArgsFunction: app.completeSandbox,
		RunE: func(cmd *cobra.Command, args []string) error {
			in, err := app.resolve(args[0])
			if err != nil {
				return err
			}
			if bind != "127.0.0.1" && bind != "localhost" {
				fmt.Fprintf(cmd.ErrOrStderr(), "cs-sandbox: warning: --bind %s exposes the forward beyond host loopback\n", bind)
			}
			if socks < 0 || socks > 65535 {
				return errors.New("--socks port must be between 1 and 65535")
			}
			if socks > 0 {
				r, err := forward.Start(app.Host, paths.GroupKeys(in.Group), app.InstDir, in.Group, in.Name, in.Port, "D", socks, "socks", bind)
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "forward: socks5://%s:%d -> %s\n", r.Bind, r.HostPort, args[0])
				return nil
			}
			if len(args) < 2 {
				return errors.New("usage: cs-sandbox forward <name> [HOSTPORT:]VMPORT... | --socks [PORT]")
			}
			for _, spec := range args[1:] {
				hp, vp, err := parsePortSpec(spec)
				if err != nil {
					return err
				}
				r, err := forward.Start(app.Host, paths.GroupKeys(in.Group), app.InstDir, in.Group, in.Name, in.Port, "L", hp, fmt.Sprintf("localhost:%d", vp), bind)
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "forward: %s:%d -> %s:%d\n", r.Bind, r.HostPort, args[0], vp)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&bind, "bind", "127.0.0.1", "bind address for forwarded ports")
	cmd.Flags().IntVar(&socks, "socks", 0, "open a SOCKS proxy on this port (default 1080 when used without a value)")
	cmd.Flags().Lookup("socks").NoOptDefVal = "1080"
	return cmd
}

func newForwardsCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:               "forwards [name]",
		Short:             "List active port forwards",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: app.completeSandboxAlways,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Forwards are listed per instance, not per name: identity is
			// (group, name), so a bare name would fold two groups' members into
			// one row and attribute one's forwards to the other.
			var insts []*state.Instance
			if len(args) == 1 {
				in, err := app.resolve(args[0])
				if err != nil {
					return err
				}
				insts = []*state.Instance{in}
			} else {
				var err error
				if insts, err = state.List(app.InstDir); err != nil {
					return err
				}
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 2, ' ', 0)
			any := false
			fmt.Fprintln(tw, "NAME\tHOST\tKIND\tTARGET")
			for _, in := range insts {
				recs, _ := forward.List(app.InstDir, in.Group, in.Name)
				for _, r := range recs {
					any = true
					fmt.Fprintf(tw, "%s\t%s:%d\t%s\t%s\n", Ref(in), r.Bind, r.HostPort, r.Kind, r.Target)
				}
			}
			if !any {
				fmt.Fprintln(cmd.OutOrStdout(), "no active forwards")
				return nil
			}
			return tw.Flush()
		},
	}
}

func newUnforwardCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:               "unforward <name> [HOSTPORT|all]",
		Short:             "Tear down forwards for a sandbox",
		Args:              cobra.RangeArgs(1, 2),
		ValidArgsFunction: app.completeSandbox,
		RunE: func(cmd *cobra.Command, args []string) error {
			target := "all"
			if len(args) == 2 {
				target = args[1]
			}
			in, err := app.resolve(args[0])
			if err != nil {
				return err
			}
			n, err := forward.Remove(app.InstDir, in.Group, in.Name, target)
			if err != nil {
				return err
			}
			if n == 0 {
				return fmt.Errorf("no matching forward for %s (%s)", args[0], target)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "removed %d forward(s) for %s\n", n, args[0])
			return nil
		},
	}
}

func parsePortSpec(spec string) (hostPort, vmPort int, err error) {
	if before, after, ok := strings.Cut(spec, ":"); ok {
		hostPort, err = strconv.Atoi(before)
		if err == nil {
			vmPort, err = strconv.Atoi(after)
		}
	} else {
		hostPort, err = strconv.Atoi(spec)
		vmPort = hostPort
	}
	if err != nil || hostPort <= 0 || hostPort > 65535 || vmPort <= 0 || vmPort > 65535 {
		return 0, 0, fmt.Errorf("invalid port spec %q (use VMPORT or HOSTPORT:VMPORT)", spec)
	}
	return hostPort, vmPort, nil
}
