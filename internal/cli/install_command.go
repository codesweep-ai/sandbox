package cli

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	assets "github.com/codesweep-ai/sandbox"
	"github.com/spf13/cobra"
)

// newInstallAgentToolsCmd copies the bundled agent tools (the
// cs-claude/cs-codex/cs-opencode launch wrappers + the remote-delegation
// families + docs) onto the host PATH.
func newInstallAgentToolsCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "install-agent-tools [dir]",
		Short: "Install the agent tools (cs-claude/cs-codex/cs-opencode + remote families) on your PATH (default: ~/.local/bin)",
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
				if e.IsDir() {
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
			for _, b := range []string{"claude", "codex", "opencode"} {
				if !hasBinary(b) {
					miss = append(miss, b)
				}
			}
			if len(miss) > 0 {
				fmt.Fprintf(out, "note: %s not on PATH — cs-claude/cs-codex/cs-opencode need the agent CLI(s) on the host,\n", strings.Join(miss, " "))
				fmt.Fprintln(out, "  or sign in inside an instance: cs-sandbox agent-login claude <name>")
			} else {
				fmt.Fprintln(out, "next: sign in once on the host — 'cs-claude' (/login), 'cs-codex login', 'cs-opencode providers login' — so new instances inherit your auth")
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

// newAgentToolsCmd publishes what `install-agent-tools` would install, as
// name -> sha256 of the file this build carries.
//
// It exists because the campaign layer needs those hashes and must not derive
// them itself. A member's tools come from the image's home skeleton, which is
// this same directory, so cs-sandbox is the only thing that can say what a
// healthy member should be running — and the alternative, cs-campaign reaching
// into this module for the bytes, makes every rootfs rearrangement here a
// compile error there.
//
// The hashes are of the SHIPPED copy, never of what happens to be on PATH:
// this is the reference a PATH or a member is compared against, so reading it
// from PATH would make any drifted host agree with itself.
func newAgentToolsCmd(app *App) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "agent-tools",
		Short: "List the agent tools this build ships, with their sha256",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			src, err := assets.HostHelpers(app.AssetDir)
			if err != nil {
				return fmt.Errorf("cannot find bundled tools: %w", err)
			}
			entries, err := fs.ReadDir(src, ".")
			if err != nil {
				return err
			}
			tools := map[string]string{}
			var names []string
			for _, e := range entries {
				// Docs ship here too and install 0644; they are not the harness.
				if e.IsDir() || strings.HasSuffix(e.Name(), ".md") {
					continue
				}
				data, err := fs.ReadFile(src, e.Name())
				if err != nil {
					return fmt.Errorf("read %s: %w", e.Name(), err)
				}
				tools[e.Name()] = fmt.Sprintf("%x", sha256.Sum256(data))
				names = append(names, e.Name())
			}
			sort.Strings(names)
			out := cmd.OutOrStdout()
			if asJSON {
				encoded, err := json.MarshalIndent(struct {
					Version string            `json:"version"`
					Tools   map[string]string `json:"tools"`
				}{buildVersion(), tools}, "", "  ")
				if err != nil {
					return err
				}
				_, err = fmt.Fprintln(out, string(encoded))
				return err
			}
			for _, name := range names {
				fmt.Fprintf(out, "%s  %s\n", tools[name], name)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON: the build version and every tool's sha256")
	return cmd
}
