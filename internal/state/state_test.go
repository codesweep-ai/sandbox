package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestValidName pins the sandbox-name gate. Beyond keeping the name resolvable
// (it becomes a hostname / ssh alias / dnsmasq entry), it is what stops a name
// from escaping the instances dir via filepath.Join, or injecting a newline into
// the generated ~/.ssh/config.d/cs-sandbox.
func TestValidName(t *testing.T) {
	for _, ok := range []string{"feature", "web", "box-1", "a", "csgocli-fwd-731998", "X9"} {
		if err := ValidName(ok); err != nil {
			t.Errorf("ValidName(%q) unexpected error: %v", ok, err)
		}
	}
	bad := []string{
		"",                // empty
		"..",              // parent dir
		"../../tmp/pwned", // path traversal
		"a/b",             // separator
		"-lead", "trail-", // must start/end alphanumeric
		"has space",             //
		"x'y",                   // quote (shell-script interpolation)
		"x\ny",                  // newline — ssh_config directive injection
		"a.b",                   // dotted: the guest peer rule is "Host * !*.*"
		"под",                   // non-ASCII
		strings.Repeat("a", 64), // too long
	}
	for _, b := range bad {
		if err := ValidName(b); err == nil {
			t.Errorf("ValidName(%q) should fail", b)
		}
	}
}

func TestSaveLoad(t *testing.T) {
	dir := t.TempDir()
	in := &Instance{Name: "s1", Type: "agent", Engine: Podman, Port: 2202, Created: "t"}
	if err := Save(dir, in); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir, DefaultGroup, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "s1" || got.Port != 2202 {
		t.Errorf("load mismatch: %+v", got)
	}
}

func TestSaveLoadRejectInvalidNames(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, nil); err == nil {
		t.Error("Save(nil) should fail")
	}
	if err := Save(dir, &Instance{Name: "../escape"}); err == nil {
		t.Error("Save should reject a path-traversing name")
	}
	if _, err := Load(dir, DefaultGroup, "../escape"); err == nil {
		t.Error("Load should reject a path-traversing name")
	}
	if _, err := os.Stat(filepath.Join(dir, "..", "escape")); !os.IsNotExist(err) {
		t.Errorf("invalid save escaped the instance directory: %v", err)
	}
}

func TestList(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"b", "a"} {
		if err := Save(dir, &Instance{Name: n, Type: "agent", Engine: Podman, Port: 2200}); err != nil {
			t.Fatal(err)
		}
	}
	insts, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(insts) != 2 || insts[0].Name != "a" || insts[1].Name != "b" {
		t.Errorf("List should be sorted: %+v", insts)
	}
}

func TestListReportsCorruptState(t *testing.T) {
	dir := t.TempDir()
	badDir := Dir(dir, DefaultGroup, "broken")
	if err := os.MkdirAll(badDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badDir, "state.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := List(dir); err == nil || !strings.Contains(err.Error(), "broken") {
		t.Fatalf("List error = %v, want corrupt instance name", err)
	}

	// A retained data directory from `rm` has no state.json and is intentionally
	// not an active instance.
	if err := os.Remove(filepath.Join(badDir, "state.json")); err != nil {
		t.Fatal(err)
	}
	if got, err := List(dir); err != nil || len(got) != 0 {
		t.Fatalf("List retained data = (%v, %v), want empty", got, err)
	}
}

// TestListReturnsHealthyAlongsideCorrupt: a corrupt record must not hide the
// instances that did load. Port and VM-IP allocation call List best-effort
// (`insts, _ := List(...)`), so an empty list there means an already-claimed
// port or address gets handed out again.
func TestListReturnsHealthyAlongsideCorrupt(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, &Instance{Name: "healthy", Type: "agent", Engine: Podman, Port: 2203}); err != nil {
		t.Fatal(err)
	}
	badDir := Dir(dir, DefaultGroup, "broken")
	if err := os.MkdirAll(badDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badDir, "state.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := List(dir)
	if err == nil {
		t.Error("List should report the corrupt record")
	}
	if len(got) != 1 || got[0].Name != "healthy" || got[0].Port != 2203 {
		t.Fatalf("List = %+v, want the healthy instance returned alongside the error", got)
	}
}

// TestListSkipsNonInstanceDirs: a directory that cannot be an instance (a name
// the tool would never create) is not corruption — skip it silently rather than
// failing every command that lists sandboxes.
func TestListSkipsNonInstanceDirs(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, &Instance{Name: "good", Type: "agent", Engine: Podman, Port: 2204}); err != nil {
		t.Fatal(err)
	}
	for _, stray := range []string{"not.an.instance", "my_backup", ".hidden"} {
		if err := os.MkdirAll(filepath.Join(dir, stray), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	got, err := List(dir)
	if err != nil {
		t.Fatalf("stray directories should be skipped, got error: %v", err)
	}
	if len(got) != 1 || got[0].Name != "good" {
		t.Fatalf("List = %+v, want just the real instance", got)
	}
}

// TestGroupQualifiedRecords: identity is (group, name), so the same name can be
// saved in two groups without either overwriting the other, and List reports
// both — grouped and sorted.
// Two names that each pass their own validator can still compose into a socket
// path over the AF_UNIX limit, because <instances>/<group>/<name>/ spends one
// shared budget. Left unchecked the overflow is silent: socat truncates, binds
// the shortened name and exits 0, and the operator gets a readiness timeout
// pointing at a serial.log that was never written.
func TestValidInstancePathBoundsTheSharedSocketBudget(t *testing.T) {
	// Room to spare: an ordinary layout is accepted.
	if err := ValidInstancePath("/home/dev/.local/share/cs-sandbox/instances", "cache-redis", "api"); err != nil {
		t.Errorf("ordinary names rejected: %v", err)
	}
	// Each label is individually legal at 63 characters, and both are refused
	// together — which is the whole point of the check.
	long := strings.Repeat("a", 63)
	if err := ValidName(long); err != nil {
		t.Fatalf("precondition: %q must be a legal name: %v", long, err)
	}
	if err := ValidGroup(long); err != nil {
		t.Fatalf("precondition: %q must be a legal group: %v", long, err)
	}
	err := ValidInstancePath("/home/dev/.local/share/cs-sandbox/instances", long, long)
	if err == nil {
		t.Fatal("two 63-character labels must not compose into a legal socket path")
	}
	// The message has to carry the numbers: the operator cannot act on "too
	// long" without knowing by how much.
	for _, want := range []string{"AF_UNIX limit", "108", "shorten"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
	// An empty group means the default group, exactly as everywhere else.
	if err := ValidInstancePath("/i", "", "api"); err != nil {
		t.Errorf("empty group should mean %q: %v", DefaultGroup, err)
	}
}

// The budget is only as good as the name it measures: it has to cover every
// socket the instance directory holds, or the check under-measures and the
// silent truncation it exists to prevent comes back.
func TestSocketBudgetCoversEveryInstanceSocket(t *testing.T) {
	if len(instanceSockets) == 0 {
		t.Fatal("no instance sockets declared")
	}
	for _, sock := range instanceSockets {
		if len(sock) > len(longestSocketName) {
			t.Errorf("socket %q is longer than the budgeted %q", sock, longestSocketName)
		}
	}
	// sun_path counts its terminator, so 107 bytes is the longest path that
	// fits and 108 is one too many. Walk the root length across that edge.
	const group, name = "g", "n"
	for pad := 1; pad < 120; pad++ {
		root := "/" + strings.Repeat("d", pad)
		full := filepath.Join(root, group, name, longestSocketName)
		err := ValidInstancePath(root, group, name)
		switch {
		case len(full) == sunPathMax-1 && err != nil:
			t.Fatalf("path of %d bytes rejected; %d is the last that fits", len(full), sunPathMax-1)
		case len(full) == sunPathMax && err == nil:
			t.Fatalf("path of %d bytes accepted; the limit is %d", len(full), sunPathMax)
		}
	}
}

// The host source repository sits outside every group, so the branch a fetch
// lands on has to carry the group — otherwise two groups running the same
// fixture both target refs/heads/cs-sandbox/<name> and the second fetch is
// rejected as a non-fast-forward.
func TestBranchNameCarriesTheGroup(t *testing.T) {
	// The default group keeps the bare form: it is what every doc example shows
	// and what existing host repos already have.
	if got := BranchName(DefaultGroup, "api"); got != "cs-sandbox/api" {
		t.Errorf("default group branch = %q, want cs-sandbox/api", got)
	}
	if got := BranchName("", "api"); got != "cs-sandbox/api" {
		t.Errorf("empty group must mean the default group, got %q", got)
	}
	// A named group qualifies, so two groups' same-named members differ.
	a, b := BranchName("cache-redis", "api"), BranchName("cache-memcached", "api")
	if a == b {
		t.Fatalf("two groups' branches collide: %q", a)
	}
	if a != "cs-sandbox/api.cache-redis" {
		t.Errorf("grouped branch = %q, want cs-sandbox/api.cache-redis", a)
	}
}

// The group is APPENDED, not nested. Nested (cs-sandbox/<group>/<name>) puts a
// directory where a ref may already exist: a default-group sandbox `api` owns
// refs/heads/cs-sandbox/api, and git then refuses refs/heads/cs-sandbox/api/x
// with "cannot lock ref". Appended, the two are siblings.
func TestBranchNameCannotConflictWithADefaultGroupBranch(t *testing.T) {
	bare := BranchName(DefaultGroup, "api") // a sandbox named api
	grouped := BranchName("api", "worker")  // a GROUP named api
	if strings.HasPrefix(grouped, bare+"/") {
		t.Fatalf("%q nests under %q; git cannot hold both", grouped, bare)
	}
	// Same shape the CLI uses for a reference, so the branch names its sandbox.
	if grouped != "cs-sandbox/"+ObjectName("api", "worker") {
		t.Errorf("branch %q should carry the canonical reference spelling", grouped)
	}
}

func TestGroupQualifiedRecords(t *testing.T) {
	dir := t.TempDir()
	for _, g := range []string{"cache-redis", "cache-memory"} { // saved out of order on purpose
		if err := Save(dir, &Instance{Name: "worker", Group: g, Type: "agent", Engine: Podman}); err != nil {
			t.Fatal(err)
		}
	}
	for _, g := range []string{"cache-redis", "cache-memory"} {
		in, err := Load(dir, g, "worker")
		if err != nil {
			t.Fatalf("load worker in %s: %v", g, err)
		}
		if in.Group != g {
			t.Errorf("worker in %s loaded with group %q", g, in.Group)
		}
	}
	got, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("List = %d records, want both groups' worker", len(got))
	}
	// Grouped and sorted, so a group's members stay adjacent wherever listed.
	if got[0].Group != "cache-memory" || got[1].Group != "cache-redis" {
		t.Errorf("List not sorted by group: %s, %s", got[0].Group, got[1].Group)
	}
	// A record found under the wrong group directory is a corrupt record, not a
	// silently-relocated sandbox.
	if err := os.MkdirAll(Dir(dir, "cache-none", "worker"), 0o700); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(Dir(dir, "cache-redis", "worker"), "state.json"))
	if err := os.WriteFile(filepath.Join(Dir(dir, "cache-none", "worker"), "state.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir, "cache-none", "worker"); err == nil {
		t.Error("a record whose group does not match its location must not load")
	}
}

// TestGroupRecords: groups round-trip and list independently of their members.
func TestGroupRecords(t *testing.T) {
	dir := t.TempDir()
	if err := SaveGroup(dir, &Group{Name: "cache-redis", Created: "2026-01-01T00:00:00Z", TapPrefix: "fd0001", GWPort: 2400}); err != nil {
		t.Fatal(err)
	}
	g, err := LoadGroup(dir, "cache-redis")
	if err != nil {
		t.Fatal(err)
	}
	if g.TapPrefix != "fd0001" || g.GWPort != 2400 {
		t.Errorf("group round-trip lost fields: %+v", g)
	}
	if _, err := LoadGroup(dir, "nope"); err == nil {
		t.Error("loading an absent group should fail")
	}
	// A plain instance directory is not a group.
	if err := Save(dir, &Instance{Name: "solo", Group: "cache-redis", Engine: Podman}); err != nil {
		t.Fatal(err)
	}
	groups, err := ListGroups(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].Name != "cache-redis" {
		t.Errorf("ListGroups = %+v, want just cache-redis", groups)
	}
	if err := ValidGroup("bad name"); err == nil {
		t.Error("group names must be a single DNS label")
	}
	if NetworkName(DefaultGroup) != "cs-sandbox-net" || NetworkName("cache-redis") != "cs-sandbox-cache-redis" {
		t.Error("network naming changed unexpectedly")
	}
}
