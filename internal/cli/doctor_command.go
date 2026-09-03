package cli

import (
	"errors"
	"fmt"
	"os"

	assets "github.com/codesweep-ai/sandbox"
	"github.com/codesweep-ai/sandbox/internal/doctor"
	"github.com/codesweep-ai/sandbox/internal/fcdisk"
	"github.com/spf13/cobra"
)

// ErrChecksFailed is returned by `doctor` when it finds issues. It has already
// printed its report, so main maps it to exit code 1 without re-printing.
var ErrChecksFailed = errors.New("host checks failed")

func newDoctorCmd(app *App) *cobra.Command {
	engine := ""
	slim := false
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check host prerequisites",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if engine == "" {
				engine = autoEngine(app.Host.IsMacOS) // same default `create` would pick
			}
			// Retarget exactly as `build --slim` does, and for the same reason
			// the flag exists there: the two variants are separate images with
			// separate artifacts, and a report is about one of them.
			//
			// Without this the flag did not exist and the default answered for
			// the shipped image, so somebody working on the slim tier was told
			// their host was ready while the rootfs their sandboxes would boot
			// was missing. That is the diagnosis this command exists to give,
			// delivered about the wrong image.
			//
			// An explicit CS_SANDBOX_IMAGE still wins, as it does for build:
			// naming a reference is how CI pins a run to one, and a flag must
			// not quietly report on something else.
			if slim && os.Getenv("CS_SANDBOX_IMAGE") == "" {
				ref, err := imageRef(slimImageRepo)
				if err != nil {
					return err
				}
				app.Image = ref
			}
			switch engine {
			case "podman", "firecracker":
			default:
				return errors.New("--engine must be podman or firecracker")
			}
			fc := fcdisk.Cache{Dir: app.FCCache}
			hr := app.hostRoute()
			// Both prefer the checkout over the embedded copy, exactly as
			// install-agent-tools and the image build do. Run from a checkout,
			// doctor therefore answers "is PATH what this tree would install",
			// which is the question a contributor editing a tool has.
			//
			// Best effort: a build that cannot read either leaves the check
			// unmade rather than failing a diagnosis of everything else.
			bundled, _ := assets.HostHelpers(app.AssetDir)
			pins, _ := assets.ToolPins(app.AssetDir)
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
				FCCache:        fc.Dir,
				InstDir:        app.InstDir,

				Lend: app.lendState(),

				BundledTools: bundled,
				ToolPins:     pins,
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
	cmd.Flags().BoolVar(&slim, "slim", false,
		"check the slimmed CI image and its artifacts instead of the shipped ones")
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
