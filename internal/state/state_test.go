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
