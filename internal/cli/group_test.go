package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codesweep-ai/sandbox/internal/hostenv"
	"github.com/codesweep-ai/sandbox/internal/run"

	"github.com/codesweep-ai/sandbox/internal/state"
)

// TestResolveBareNameIsDefaultGroupOnly: a bare reference means the default
// group and never anything else. Resolving it to whichever group happened to
// hold the name uniquely made a reference's meaning depend on the rest of the
// host — `worker` worked until some unrelated group created its own `worker`.
// Being predictable matters more here than saving five keystrokes.
func TestResolveBareNameIsDefaultGroupOnly(t *testing.T) {
	dir := t.TempDir()
	for _, g := range []string{"cache-redis", "cache-memory"} {
		if err := state.SaveGroup(dir, &state.Group{Name: g, Created: "2026-01-01T00:00:00Z"}); err != nil {
			t.Fatal(err)
		}
		if err := state.Save(dir, &state.Instance{
			Name: "worker", Group: g, Type: "agent", Engine: state.Podman, Port: 2200,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Unique host-wide, but NOT in the default group.
	if err := state.Save(dir, &state.Instance{
		Name: "solo", Group: "cache-redis", Type: "agent", Engine: state.Podman, Port: 2202,
	}); err != nil {
		t.Fatal(err)
	}
	if err := state.Save(dir, &state.Instance{
		Name: "plain", Group: state.DefaultGroup, Type: "agent", Engine: state.Podman, Port: 2203,
	}); err != nil {
		t.Fatal(err)
	}
	app := &App{InstDir: dir}

	// Qualified always works and picks the right one.
	for _, g := range []string{"cache-redis", "cache-memory"} {
		in, err := app.resolve("worker." + g)
		if err != nil {
			t.Fatalf("resolve worker.%s: %v", g, err)
		}
		if in.Group != g {
			t.Errorf("resolve worker.%s gave group %q", g, in.Group)
		}
	}
	// A bare name resolves in the default group.
	if in, err := app.resolve("plain"); err != nil || in.Group != state.DefaultGroup {
		t.Errorf("bare default-group name should resolve: %v %+v", err, in)
	}
	// Unique host-wide is NOT enough: uniqueness is a property of the moment,
	// not of the reference.
	_, err := app.resolve("solo")
	if err == nil {
		t.Fatal("a bare name must not reach a non-default group, even when unique")
	}
	if !strings.Contains(err.Error(), "solo.cache-redis") {
		t.Errorf("error should point at the qualified reference: %v", err)
	}
	// Ambiguous bare name: refuses, and names every candidate.
	_, err = app.resolve("worker")
	if err == nil {
		t.Fatal("bare name must not resolve outside the default group")
	}
	for _, want := range []string{"worker.cache-redis", "worker.cache-memory"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should list %q: %v", want, err)
		}
	}
}

// TestTapPrefixIsAllocatedNotHashed: interface names are host-global, so two
// groups must never derive the same tap prefix. Allocation makes that
// impossible; hashing the group name only made it improbable.
func TestTapPrefixIsAllocatedNotHashed(t *testing.T) {
	dir := t.TempDir()
	app := &App{InstDir: dir}
	seen := map[string]string{}
	for _, g := range []string{"alpha", "beta", "gamma"} {
		p, err := app.allocTapPrefix(g)
		if err != nil {
			t.Fatal(err)
		}
		if prev, dup := seen[p]; dup {
			t.Fatalf("groups %q and %q both got tap prefix %q", prev, g, p)
		}
		seen[p] = g
		if err := state.SaveGroup(dir, &state.Group{Name: g, TapPrefix: p, Created: "2026-01-01T00:00:00Z"}); err != nil {
			t.Fatal(err)
		}
	}
}

// TestLsJSONShape pins the machine-readable inventory. `ref` is the field a
// caller feeds back into other commands: a bare name is not unique across
// groups, so scripting on `name` is a bug waiting to happen.
func TestLsJSONShape(t *testing.T) {
	dir := t.TempDir()
	for _, g := range []string{"cache-redis", "cache-memory"} {
		if err := state.SaveGroup(dir, &state.Group{Name: g, Created: "2026-01-01T00:00:00Z"}); err != nil {
			t.Fatal(err)
		}
		if err := state.Save(dir, &state.Instance{
			Name: "worker", Group: g, Type: "agent", Engine: state.Podman,
			Port: 2200, Created: "2026-01-01T00:00:00Z",
		}); err != nil {
			t.Fatal(err)
		}
	}
	app := &App{InstDir: dir, Runner: run.NewFake()}
	var buf bytes.Buffer
	if err := runLsJSON(context.Background(), app, &buf); err != nil {
		t.Fatal(err)
	}
	var items []lsItem
	if err := json.Unmarshal(buf.Bytes(), &items); err != nil {
		t.Fatalf("ls --json is not valid JSON: %v\n%s", err, buf.String())
	}
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2:\n%s", len(items), buf.String())
	}
	refs := map[string]lsItem{}
	for _, it := range items {
		refs[it.Ref] = it
	}
	for _, g := range []string{"cache-redis", "cache-memory"} {
		it, ok := refs["worker."+g]
		if !ok {
			t.Fatalf("missing ref worker.%s:\n%s", g, buf.String())
		}
		if it.Name != "worker" || it.Group != g {
			t.Errorf("worker.%s = %+v", g, it)
		}
		if it.Network != state.NetworkName(g) {
			t.Errorf("worker.%s network = %q, want %q", g, it.Network, state.NetworkName(g))
		}
	}
}

// TestGroupRmRefusesWhileMembersExist: a group owns its network, keys and
// gateway, so removing it while sandboxes still use them would strand live
// members on artifacts that are about to disappear.
func TestGroupRmRefusesWhileMembersExist(t *testing.T) {
	dir := t.TempDir()
	if err := state.SaveGroup(dir, &state.Group{Name: "cache-redis", Created: "2026-01-01T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if err := state.Save(dir, &state.Instance{
		Name: "worker", Group: "cache-redis", Type: "agent", Engine: state.Podman,
	}); err != nil {
		t.Fatal(err)
	}
	app := &App{InstDir: dir, Runner: run.NewFake()}
	err := app.removeGroup(context.Background(), "cache-redis", false, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "still has 1 sandbox") {
		t.Fatalf("group rm must refuse while members exist, got %v", err)
	}
	if _, err := state.LoadGroup(dir, "cache-redis"); err != nil {
		t.Error("a refused group rm must leave the group intact")
	}
	// An unknown group is an error, not a silent success.
	if err := app.removeGroup(context.Background(), "nope", false, io.Discard); err == nil {
		t.Error("removing an unknown group should fail")
	}
}

// TestCommandsPassBareNamesToEngines: commands take a QUALIFIED ref from the
// user but engines take a BARE name — their Deps carries the group and they
// qualify podman object names themselves. Handing an engine "box.cache-redis" made
// it address "box.cache-redis.cache-redis", which matches nothing; because the removal
// path ignores podman errors, `destroy` reported success while removing
// nothing at all.
func TestCommandsPassBareNamesToEngines(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CS_SANDBOX_INSTANCES_DIR", dir) // the root command resolves state dirs from the env
	if err := state.SaveGroup(dir, &state.Group{Name: "cache-redis", Created: "2026-01-01T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if err := state.Save(dir, &state.Instance{
		Name: "box", Group: "cache-redis", Type: "agent", Engine: state.Podman, Port: 2200,
	}); err != nil {
		t.Fatal(err)
	}
	app := &App{InstDir: dir, TierDir: t.TempDir()}
	f, err := runRoot(t, app, "destroy", "box.cache-redis", "-f") // runRoot owns the fake runner
	if err != nil {
		t.Fatalf("destroy: %v", err)
	}
	// The object podman is asked to remove must be qualified exactly once.
	if !f.Contains("podman rm -f box.cache-redis") {
		t.Errorf("destroy did not address the right container: %v", f.Rendered())
	}
	if f.Contains("box.cache-redis.cache-redis") {
		t.Errorf("engine was handed an already-qualified ref: %v", f.Rendered())
	}
	// And the record is gone, so it cannot resurface in `ls`.
	if _, err := state.Load(dir, "cache-redis", "box"); err == nil {
		t.Error("destroy left the state record behind")
	}
}

// `group ls --json` is the machine-readable group inventory. Without it a
// consumer that manages groups has to parse the human table, or match the prose
// of an error to find out whether a group exists at all — the exact coupling
// `ls --json` was added to remove for sandboxes.
func TestGroupLsJSONIsAStableInventory(t *testing.T) {
	dir := t.TempDir()
	if err := state.SaveGroup(dir, &state.Group{
		Name: "cache-redis", Created: "2026-01-01T00:00:00Z", TapPrefix: "fd0001", GWPort: 2401,
	}); err != nil {
		t.Fatal(err)
	}
	if err := state.Save(dir, &state.Instance{
		Name: "worker", Group: "cache-redis", Type: "agent", Engine: state.Podman, Port: 2200,
	}); err != nil {
		t.Fatal(err)
	}
	app := &App{InstDir: dir, Runner: run.NewFake()}
	cmd := newGroupLsCmd(app)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var items []groupItem
	if err := json.Unmarshal(out.Bytes(), &items); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out.String())
	}
	if len(items) != 1 {
		t.Fatalf("items = %+v, want exactly the one group", items)
	}
	got := items[0]
	// Network is derived, not stored, so a consumer must not have to derive it
	// itself — that is how the two drift apart.
	want := groupItem{
		Name: "cache-redis", Network: "cs-sandbox-cache-redis",
		Gateway: 2401, Members: 1, Created: "2026-01-01T00:00:00Z",
	}
	if got != want {
		t.Errorf("group item = %+v, want %+v", got, want)
	}
}

// `group ls -q` is the scripting form, and it means the same thing here as it
// does for sandboxes: the bare references, one per line, with nothing else on
// the line to strip. Without the local flag, -q would be the root's "silence
// output" flag, which does not touch stdout — so the table would come out
// verbatim and a pipeline would be reading column headers.
func TestGroupLsQuietPrintsNamesOnly(t *testing.T) {
	dir := t.TempDir()
	for _, g := range []string{"cache-redis", "web"} {
		if err := state.SaveGroup(dir, &state.Group{
			Name: g, Created: "2026-01-01T00:00:00Z", TapPrefix: "fd0001", GWPort: 2401,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := state.Save(dir, &state.Instance{
		Name: "worker", Group: "cache-redis", Type: "agent", Engine: state.Podman, Port: 2200,
	}); err != nil {
		t.Fatal(err)
	}
	app := &App{InstDir: dir, Runner: run.NewFake()}
	cmd := newGroupLsCmd(app)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"-q"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "cache-redis\nweb\n"; got != want {
		t.Errorf("group ls -q = %q, want %q", got, want)
	}
}

// -q and --json ask for two different renderings of the same listing, so asking
// for both is a mistake worth naming rather than silently resolving one way.
func TestGroupLsQuietAndJSONAreMutuallyExclusive(t *testing.T) {
	app := &App{InstDir: t.TempDir(), Runner: run.NewFake()}
	cmd := newGroupLsCmd(app)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"-q", "--json"})
	if err := cmd.Execute(); err == nil {
		t.Error("group ls -q --json was accepted")
	}
}

// An empty host still answers, and answers with a JSON array rather than
// nothing: a consumer probing for a group must be able to tell "no groups" from
// "this build has no such command".
func TestGroupLsJSONOnAnEmptyHostIsAnEmptyArray(t *testing.T) {
	app := &App{InstDir: t.TempDir(), Runner: run.NewFake()}
	cmd := newGroupLsCmd(app)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != "[]" {
		t.Errorf("empty host = %q, want []", out.String())
	}
	var items []groupItem
	if err := json.Unmarshal(out.Bytes(), &items); err != nil || len(items) != 0 {
		t.Errorf("empty output must decode as an empty slice: %v", err)
	}
}

// TestGroupRmRetiresItsLeg: a removed group must take its host-route leg with
// it, and before its gateway goes.
//
// netavark tears a network's bridge down when the last container leaves it, and
// declines while anything else is still attached. A leg that outlives its group
// therefore pins the bridge, which keeps the address of the subnet it was built
// for; podman then hands that interface name to the next network it creates and
// netavark adopts the interface as it finds it. That group's members come up
// pointing at a gateway which does not exist — no DNS, no outbound, no error.
func TestGroupRmRetiresItsLeg(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CS_SANDBOX_FC_NET", filepath.Join(t.TempDir(), "net"))
	t.Setenv("CS_SANDBOX_TIER_DIR", filepath.Join(t.TempDir(), "keys"))
	if err := state.SaveGroup(dir, &state.Group{
		Name: "cache-redis", TapPrefix: "fd0007", GWPort: 2400, Created: "2026-01-01T00:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	f := run.NewFake()
	app := &App{InstDir: dir, Runner: f, Host: hostenv.Host{User: "dev", Home: t.TempDir()}}
	if err := app.removeGroup(context.Background(), "cache-redis", false, io.Discard); err != nil {
		t.Fatal(err)
	}
	leg, gateway := -1, -1
	for i, line := range f.Rendered() {
		switch {
		case strings.Contains(line, "ip link del hr0007n"):
			leg = i
		case strings.Contains(line, "rm -f cs-sandbox-cache-redis-keepalive"):
			gateway = i
		}
	}
	if leg < 0 {
		t.Fatalf("group rm did not retire the group's leg:\n%s", strings.Join(f.Rendered(), "\n"))
	}
	if gateway < 0 {
		t.Fatalf("group rm did not remove the gateway:\n%s", strings.Join(f.Rendered(), "\n"))
	}
	if leg > gateway {
		t.Errorf("the leg must go before the gateway, or the bridge is still pinned when netavark "+
			"gets its one chance to remove it (leg=%d gateway=%d):\n%s", leg, gateway, strings.Join(f.Rendered(), "\n"))
	}
}
