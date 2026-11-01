package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The OpenCode context hook is a plugin. It ships outside the profile and is linked in only for
// a machine with a hand-off line, so a member with no line starts exactly as it would without
// the feature: OpenCode 1 installed its plugin SDK from npm on the first start that found any
// plugin, and a profile that shipped the plugin failed every offline first turn (SBX-106).

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
// image, so a member with no line loads no plugin.
func TestOpenCodeProfileShipsNoPlugin(t *testing.T) {
	shippedOpenCodeHook(t)
	entries, err := os.ReadDir(filepath.Join("..", "..", "image", "rootfs", "home", ".cs-opencode", "plugins"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Errorf("image/rootfs/home/.cs-opencode/plugins ships %q; a plugin there loads in every OpenCode start, line or none", e.Name())
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

// TestOpenCodeHookIsAnOpenCode2Plugin: OpenCode 2 imports a plugin file and then loads it only
// when its default export is a definition with an id and a setup function. A module in OpenCode
// 1's shape, a named export returning hook names, is skipped with nothing on the turn's output,
// and the hand-off line is then never asked. The hooks it registers are OpenCode 2's.
func TestOpenCodeHookIsAnOpenCode2Plugin(t *testing.T) {
	src, err := os.ReadFile(shippedOpenCodeHook(t))
	if err != nil {
		t.Fatal(err)
	}
	code := string(src)
	for _, want := range []string{
		"export default {",
		`id: "cs.context-hook"`,
		"async setup(ctx: any)",
		`ctx.tool.hook("execute.after"`,
		`ctx.session.hook("compaction"`,
		"ctx.session.interrupt(",
	} {
		if !strings.Contains(code, want) {
			t.Errorf("the hook does not have %q, so OpenCode 2 would not run it as written", want)
		}
	}
	for _, old := range []string{`"tool.execute.after"`, `"experimental.session.compacting"`, "client.session."} {
		if strings.Contains(code, old) {
			t.Errorf("the hook still uses OpenCode 1's %s, which OpenCode 2 never calls", old)
		}
	}
}
