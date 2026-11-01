package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The OpenCode context hook is a plugin, and OpenCode installs its plugin SDK from npm on the
// first start that finds any plugin in a config directory, holding that start's first session
// until the install is done. In a CI replay the network is a recorder that answers no such
// request, so a profile that shipped the plugin failed every OpenCode cell on its first turn
// (SBX-106). The plugin therefore ships outside the profile and is linked in only for a
// machine with a hand-off line.

// shippedOpenCodeHook is where the plugin lives in the image's home seed.
func shippedOpenCodeHook(t *testing.T) string {
	t.Helper()
	p := filepath.Join("..", "..", "image", "rootfs", "home", ".local", "share", "cs-opencode", "plugins", "cs-context-hook.ts")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("the OpenCode context hook is not where the wrapper links it from: %v", err)
	}
	return p
}

// TestOpenCodeProfileShipsNoPlugin: the profile's plugins directory holds nothing in the
// image, so a member with no line starts without an SDK install.
func TestOpenCodeProfileShipsNoPlugin(t *testing.T) {
	shippedOpenCodeHook(t)
	entries, err := os.ReadDir(filepath.Join("..", "..", "image", "rootfs", "home", ".cs-opencode", "plugins"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Errorf("image/rootfs/home/.cs-opencode/plugins ships %q; a plugin there makes every OpenCode start install the plugin SDK", e.Name())
	}
}

// TestOpenCodeWrapperLinksTheHookOnlyWithALine: cs-opencode links the hook into the profile
// when CS_HANDOFF_TOKENS names a line, and leaves the profile alone otherwise.
func TestOpenCodeWrapperLinksTheHookOnlyWithALine(t *testing.T) {
	skipUnlessLinux(t)
	hook, err := os.ReadFile(shippedOpenCodeHook(t))
	if err != nil {
		t.Fatal(err)
	}
	newHome := func(t *testing.T, withHook bool) (home, bin, linked string) {
		t.Helper()
		home = t.TempDir()
		bin = filepath.Join(home, "bin")
		if err := os.MkdirAll(bin, 0o700); err != nil {
			t.Fatal(err)
		}
		writeStub(t, bin, "opencode", "#!/bin/sh\nexit 0\n")
		if withHook {
			src := filepath.Join(home, ".local", "share", "cs-opencode", "plugins", "cs-context-hook.ts")
			if err := os.MkdirAll(filepath.Dir(src), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(src, hook, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return home, bin, filepath.Join(home, ".cs-opencode", "plugins", "cs-context-hook.ts")
	}

	for _, tc := range []struct {
		name string
		env  []string
		want bool
	}{
		{"no line", nil, false},
		{"line of zero", []string{"CS_HANDOFF_TOKENS=0"}, false},
		{"a line", []string{"CS_HANDOFF_TOKENS=120000"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, bin, linked := newHome(t, true)
			out, exit := runScriptStdin(t, home, bin, tc.env, "", "cs-opencode")
			if exit != 0 {
				t.Fatalf("cs-opencode exit %d: %s", exit, out)
			}
			_, err := os.Lstat(linked)
			if got := err == nil; got != tc.want {
				t.Fatalf("hook linked into the profile = %v, want %v:\n%s", got, tc.want, out)
			}
			if !tc.want {
				return
			}
			target, err := os.Readlink(linked)
			if err != nil {
				t.Fatalf("the hook in the profile is not a link to the shipped one: %v", err)
			}
			if want := filepath.Join(home, ".local", "share", "cs-opencode", "plugins", "cs-context-hook.ts"); target != want {
				t.Errorf("link target = %q, want %q", target, want)
			}
			// A second start with the line finds the link and changes nothing.
			if out, exit := runScriptStdin(t, home, bin, tc.env, "", "cs-opencode"); exit != 0 {
				t.Fatalf("second start exit %d: %s", exit, out)
			}
		})
	}

	t.Run("a line but no shipped hook", func(t *testing.T) {
		home, bin, linked := newHome(t, false)
		out, exit := runScriptStdin(t, home, bin, []string{"CS_HANDOFF_TOKENS=120000"}, "", "cs-opencode")
		if exit != 0 {
			t.Fatalf("a missing hook must not stop the agent; exit %d: %s", exit, out)
		}
		if !strings.Contains(out, "no context hook") {
			t.Errorf("the missing hook was not reported:\n%s", out)
		}
		if _, err := os.Lstat(linked); err == nil {
			t.Error("a link was made to a hook that does not exist")
		}
	})
}
