package cli

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	assets "github.com/codesweep-ai/sandbox"
	"github.com/spf13/cobra"
)

// newInstallAgentToolsCmd copies the bundled agent tools (the cs-claude/cs-codex
// launch wrappers + the remote-delegation families + docs) onto the host PATH.
func newInstallAgentToolsCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "install-agent-tools [dir]",
		Short: "Install the agent tools (cs-claude/cs-codex + remote families) on your PATH (default: ~/.local/bin)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dest := filepath.Join(app.Host.Home, ".local", "bin")
			if len(args) == 1 {
				dest = args[0]
			}
			// Host helpers come from the checkout when present, else the embedded copy.
			src, err := assets.HostHelpers(app.AssetDir)
			if err != nil {
				return fmt.Errorf("cannot find bundled tools: %w", err)
			}
			if err := os.MkdirAll(dest, 0o755); err != nil {
				return fmt.Errorf("cannot create %s: %w", dest, err)
			}
			entries, err := fs.ReadDir(src, ".")
			if err != nil {
				return err
			}
			n := 0
			for _, e := range entries {
				if e.IsDir() || e.Name() == "user-podman" { // user-podman is guest-only
					continue
				}
				data, err := fs.ReadFile(src, e.Name())
				if err != nil {
					return fmt.Errorf("install %s: %w", e.Name(), err)
				}
				mode := os.FileMode(0o755)
				if strings.HasSuffix(e.Name(), ".md") {
					mode = 0o644
				}
				if err := os.WriteFile(filepath.Join(dest, e.Name()), data, mode); err != nil {
					return fmt.Errorf("install %s: %w", e.Name(), err)
				}
				n++
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "installed %d agent tools into %s\n", n, dest)
			if !onPath(dest) {
				fmt.Fprintf(out, "note: %s is not on your PATH yet — add it:\n", dest)
				fmt.Fprintf(out, "    echo 'export PATH=\"%s:$PATH\"' >> ~/.bashrc && source ~/.bashrc\n", dest)
			}
			var miss []string
			for _, b := range []string{"claude", "codex"} {
				if !hasBinary(b) {
					miss = append(miss, b)
				}
			}
			if len(miss) > 0 {
				fmt.Fprintf(out, "note: %s not on PATH — cs-claude/cs-codex need the agent CLI(s) on the host,\n", strings.Join(miss, " "))
				fmt.Fprintln(out, "  or sign in inside an instance: cs-sandbox claude-login <name> / codex-login <name>")
			} else {
				fmt.Fprintln(out, "next: sign in once on the host — 'cs-claude' (/login) and 'cs-codex login' — so new instances inherit your auth")
			}
			return nil
		},
	}
}

func onPath(dir string) bool {
	abs, _ := filepath.Abs(dir)
	for _, p := range filepath.SplitList(os.Getenv("PATH")) {
		if pa, _ := filepath.Abs(p); pa == abs {
			return true
		}
	}
	return false
}

func hasBinary(name string) bool {
	for _, p := range filepath.SplitList(os.Getenv("PATH")) {
		if fi, err := os.Stat(filepath.Join(p, name)); err == nil && !fi.IsDir() {
			return true
		}
	}
	return false
}
