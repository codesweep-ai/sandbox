package cli

import (
	"errors"
	"fmt"

	"github.com/codesweep-ai/sandbox/internal/doctor"
	"github.com/codesweep-ai/sandbox/internal/fcdisk"
	"github.com/spf13/cobra"
)

// ErrChecksFailed is returned by `doctor` when it finds issues. It has already
// printed its report, so main maps it to exit code 1 without re-printing.
var ErrChecksFailed = errors.New("host checks failed")

func newDoctorCmd(app *App) *cobra.Command {
	engine := ""
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check host prerequisites",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if engine == "" {
				engine = autoEngine(app.Host.IsMacOS) // same default `create` would pick
			}
			switch engine {
			case "podman", "firecracker":
			default:
				return fmt.Errorf("--engine must be podman or firecracker")
			}
			fc := fcdisk.Cache{Dir: app.FCCache}
			hr := app.hostRoute()
			d := doctor.Deps{
				Runner:  app.Runner,
				User:    app.Host.User,
				TierDir: app.TierDir,
				Image:   app.Image,
				Network: app.Network,
				IsMacOS: app.Host.IsMacOS,
				IsWSL:   app.Host.IsWSL,

				HostRouteOn:   hr.Active(),
				HostRouteLegs: hr.HostLegs(),

				FCBinPath:      fc.FirecrackerBin(),
				FCVersionPin:   envOr("CS_SANDBOX_FC_VERSION", fcdisk.DefaultFCVersion),
				FCVersionCache: fc.FirecrackerVersion(),
			}
			rep := doctor.Diagnose(cmd.Context(), engine, d)
			printReport(cmd.OutOrStdout(), rep)
			if rep.Issues > 0 {
				return ErrChecksFailed
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&engine, "engine", "", "engine to check: podman | firecracker (default: same as create)")
	return cmd
}

func printReport(w interface{ Write([]byte) (int, error) }, rep *doctor.Report) {
	const (
		green  = "\033[32m"
		red    = "\033[31m"
		yellow = "\033[33m"
		reset  = "\033[0m"
	)
	badge := func(s doctor.Status) string {
		switch s {
		case doctor.OK:
			return "  " + green + "ok" + reset + "  "
		case doctor.NO:
			return "  " + red + "NO" + reset + "  "
		default:
			return "  " + yellow + "??" + reset + "  "
		}
	}
	fmt.Fprintf(w, "cs-sandbox doctor — %s engine\n\n", rep.Engine)
	for _, g := range rep.Groups {
		fmt.Fprintf(w, "%s:\n", g.Title)
		for _, c := range g.Checks {
			fmt.Fprintf(w, "%s%s\n", badge(c.Status), c.Message)
		}
		fmt.Fprintln(w)
	}
	if rep.Issues == 0 {
		hint := ""
		if rep.Engine == "podman" {
			hint = " --engine podman"
		}
		fmt.Fprintf(w, "All good — try: cs-sandbox create <name>%s\n", hint)
	} else {
		fmt.Fprintf(w, "%d issue(s) to fix above.\n", rep.Issues)
	}
}
