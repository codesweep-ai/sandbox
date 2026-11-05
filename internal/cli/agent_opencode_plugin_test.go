package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The OpenCode context hook is a plugin. It ships outside the profile and is linked in only for
// a machine with a hand-off line or a token budget meter, so a member with neither starts
// exactly as it would without the feature: OpenCode 1 installed its plugin SDK from npm on the
// first start that found any plugin, and a profile that shipped the plugin failed every offline
// first turn (SBX-106).

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
// when CS_HANDOFF_TOKENS names a line or CS_METER_URL names the lender's meter, and leaves the
// profile alone otherwise.
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
		// A lent sandbox's meter, with no line: the plugin asks on the token budget.
		{"a meter", []string{"CS_METER_URL=http://cs-lender:2500/cs-sandbox/usage"}, true},
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

// openCodeHookHarness stands in for OpenCode's server, under Node. It loads the plugin, hands its
// execute.after hook one completed tool result of the session named on its command line, and
// prints what the model reads back from the tool and whether the plugin interrupted the session.
// Bun's one call the plugin makes, spawnSync, is played by Node's own. A session whose id starts
// with ses_child is a child session.
const openCodeHookHarness = `import { spawnSync } from "node:child_process"
import { pathToFileURL } from "node:url"
globalThis.Bun = {
  spawnSync: (argv, opts = {}) => ({
    stdout: spawnSync(argv[0], argv.slice(1), { env: opts.env ?? process.env }).stdout ?? Buffer.alloc(0),
  }),
}
const [plugin, sid] = process.argv.slice(2)
const hooks = {}
let interrupted = false
await (await import(pathToFileURL(plugin).href)).default.setup({
  tool: { hook: async (name, fn) => { hooks[name] = fn } },
  session: {
    hook: async (name, fn) => { hooks["session." + name] = fn },
    get: async ({ sessionID }) => ({ id: sessionID, parentID: sessionID.startsWith("ses_child") ? "ses_main" : undefined }),
    interrupt: async () => { interrupted = true },
  },
})
const result = { content: "ok" }
if (hooks["execute.after"]) await hooks["execute.after"]({ status: "completed", sessionID: sid, result })
console.log(JSON.stringify({ content: result.content, interrupted }))
`

// nodeForPlugin is a Node that loads TypeScript, which it does from 22.6 on, or skips the test.
func nodeForPlugin(t *testing.T) string {
	t.Helper()
	skipUnlessLinux(t)
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("the plugin runs under Node here, and there is no node on this host")
	}
	out, err := exec.Command(node, "--version").Output()
	v := regexp.MustCompile(`^v(\d+)\.(\d+)`).FindStringSubmatch(strings.TrimSpace(string(out)))
	if err != nil || v == nil {
		t.Skipf("node --version answered %q (%v)", out, err)
	}
	major, _ := strconv.Atoi(v[1])
	minor, _ := strconv.Atoi(v[2])
	if major < 22 || major == 22 && minor < 6 {
		t.Skipf("node %s cannot load the plugin's TypeScript, which takes 22.6 or later", v[0])
	}
	return node
}

// runOpenCodeHook runs the shipped plugin, as OpenCode's server would, on one tool result of
// session sid, with HOME and PATH pointed at the fake home. It returns what the model reads back
// from the tool, and whether the plugin interrupted the session.
func runOpenCodeHook(t *testing.T, home, bin string, env []string, sid string) (content string, interrupted bool) {
	t.Helper()
	node := nodeForPlugin(t)
	harness := filepath.Join(t.TempDir(), "harness.mjs")
	if err := os.WriteFile(harness, []byte(openCodeHookHarness), 0o600); err != nil {
		t.Fatal(err)
	}
	plugin, err := filepath.Abs(shippedOpenCodeHook(t))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, "--experimental-strip-types", "--no-warnings", harness, plugin, sid)
	cmd.Env = append(append(os.Environ(), "HOME="+home, "PATH="+bin+":/usr/bin:/bin", "TMUX_PANE=",
		"CS_HANDOFF_TOKENS=", "CS_METER_URL=", "CS_METER_TOKEN="), env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("the plugin did not run under node: %v\n%s", err, stderr.String())
	}
	// The plugin catches its own exceptions, so that a tool call never fails, and logs them.
	if strings.Contains(stderr.String(), "cs-context-hook:") {
		t.Fatalf("the plugin threw: %s", stderr.String())
	}
	var got struct {
		Content     string `json:"content"`
		Interrupted bool   `json:"interrupted"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("the harness printed %q: %v", out, err)
	}
	return got.Content, got.Interrupted
}

// TestTheOpenCodeHookAsksOnTheBudgetThenInterrupts: the plugin's budget line, run as OpenCode's
// server runs it. Under the ask line, and on a reading that fails, nothing is said. Past it, the
// budget's own ask is appended to every tool result until the handoff file is newer than the
// first ask, then the agent is told once more to end its turn, and after two more tool calls the
// plugin interrupts the session. The ask is recorded apart from a context ask, for the driver. A
// child session is skipped before any reading is taken.
func TestTheOpenCodeHookAsksOnTheBudgetThenInterrupts(t *testing.T) {
	home, bin := agentHome(t, ".cs-opencode-remote")
	m := newMeter(t, answer(readingUnder))
	handoff := filepath.Join(home, "work", "handoff.md")
	env := append(m.env(), "CS_HANDOFF_FILE="+handoff)
	const sid = openCodeTestSessionID
	if got, _ := runOpenCodeHook(t, home, bin, env, "ses_child04198268affeeKLgivDf"); got != "ok" || m.readings() != 0 {
		t.Fatalf("a child session's tool result was read or asked: %q, %d readings", got, m.readings())
	}
	if got, _ := runOpenCodeHook(t, home, bin, env, sid); got != "ok" {
		t.Fatalf("under the ask line the plugin appended %q", got)
	}
	m.set(meterAnswer{http.StatusServiceUnavailable, "restarting"})
	if got, _ := runOpenCodeHook(t, home, bin, env, sid); got != "ok" {
		t.Fatalf("with no reading the plugin appended %q", got)
	}
	m.set(answer(readingSpent))
	ask := "ok\n\n[budget] This sandbox has used its cached-input token budget. Write your handoff to " +
		handoff + ", as your instructions describe, then end your turn."
	for i := range 2 {
		if got, stopped := runOpenCodeHook(t, home, bin, env, sid); got != ask || stopped {
			t.Fatalf("call %d past the ask line: the model reads %q (interrupted %v), not the ask", i+1, got, stopped)
		}
	}
	rec := readBudgetAsk(t, home)
	if rec.Sid != sid || len(rec.Types) != 1 || rec.Types[0] != "cached" || rec.Used["cached"] != 1_000_412_330 ||
		rec.Limits["cached"] != 1_000_000_000 || rec.At == 0 {
		t.Fatalf("the budget ask record reads %+v", rec)
	}
	if _, err := os.Stat(filepath.Join(home, ".cs-turns", "handoff-ask")); err == nil {
		t.Fatalf("the budget's ask was recorded as a context ask")
	}
	writeHandoffFile(t, home, time.Now())
	for i := range 2 {
		if got, stopped := runOpenCodeHook(t, home, bin, env, sid); got != "ok\n\n[budget] Your handoff is written. End your turn now." || stopped {
			t.Fatalf("call %d after the handoff: the model reads %q (interrupted %v), not the nudge", i+1, got, stopped)
		}
	}
	if got, stopped := runOpenCodeHook(t, home, bin, env, sid); !stopped || got != "ok" {
		t.Fatalf("after two more tool calls the model reads %q, interrupted %v; the plugin interrupts the session", got, stopped)
	}
}

// TestTheOpenCodeHookPutsTheBudgetFirst: where both lines would ask on one tool result, the agent
// reads the budget's ask, as in the hook Claude Code and Codex run. The plugin takes its context
// reading from the turn driver, which a script plays here.
func TestTheOpenCodeHookPutsTheBudgetFirst(t *testing.T) {
	home, bin := agentHome(t, ".cs-opencode-remote")
	context := filepath.Join(home, "context")
	writeStub(t, bin, "cs-opencode-turn", "#!/bin/sh\ncat "+context+"\n")
	reads := func(tokens int) {
		t.Helper()
		if err := os.WriteFile(context, fmt.Appendf(nil, "tokens=%d window=- compactions=0 compacted=- model=m\n", tokens), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	m := newMeter(t, answer(readingUnder))
	env := append(handoffEnv(home), m.env()...)
	reads(50_000)
	if got, _ := runOpenCodeHook(t, home, bin, env, openCodeTestSessionID); got != "ok" {
		t.Fatalf("under both lines the plugin appended %q", got)
	}
	reads(61_000)
	if got, _ := runOpenCodeHook(t, home, bin, env, openCodeTestSessionID); !strings.Contains(got, "[context]") {
		t.Fatalf("over the context line alone the plugin appended %q, not the context ask", got)
	}
	m.set(answer(readingSpent))
	if got, _ := runOpenCodeHook(t, home, bin, env, openCodeTestSessionID); !strings.Contains(got, "[budget]") || strings.Contains(got, "[context]") {
		t.Fatalf("over both lines the plugin appended %q; want the budget's ask alone", got)
	}
}

// TestTheOpenCodeHookKeepsANoBudgetAnswer: a lent sandbox with no budget still has a meter. The
// lender's no-budget answer is kept for 30 s, and the plugin asks the lender nothing while it is
// fresh. Once it is stale, the next tool result reads again.
func TestTheOpenCodeHookKeepsANoBudgetAnswer(t *testing.T) {
	home, bin := agentHome(t, ".cs-opencode-remote")
	m := newMeter(t, answer(readingNoBudget))
	for i := range 2 {
		if got, _ := runOpenCodeHook(t, home, bin, m.env(), openCodeTestSessionID); got != "ok" {
			t.Fatalf("call %d with no budget: the plugin appended %q", i+1, got)
		}
	}
	if n := m.readings(); n != 1 {
		t.Fatalf("%d readings while the no-budget answer was fresh; want the first one alone", n)
	}
	none := filepath.Join(home, ".cs-turns", "budget-none")
	if err := os.WriteFile(none, fmt.Appendf(nil, "%d\n", time.Now().Unix()-31), 0o600); err != nil {
		t.Fatal(err)
	}
	runOpenCodeHook(t, home, bin, m.env(), openCodeTestSessionID)
	if n := m.readings(); n != 2 {
		t.Fatalf("%d readings; a no-budget answer 31 s old is read again", n)
	}
}
