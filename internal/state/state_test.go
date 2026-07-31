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

func TestValidNetwork(t *testing.T) {
	for _, ok := range []string{DefaultNetwork, "campaign-a", "X9"} {
		if err := ValidNetwork(ok); err != nil {
			t.Errorf("ValidNetwork(%q) unexpected error: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "a/b", "campaign.a", "-lead", strings.Repeat("a", 64)} {
		if err := ValidNetwork(bad); err == nil {
			t.Errorf("ValidNetwork(%q) should fail", bad)
		}
	}
}

func TestNetworkNameBackwardsCompatibility(t *testing.T) {
	if got := NetworkName(&Instance{}); got != DefaultNetwork {
		t.Errorf("old record network = %q, want %q", got, DefaultNetwork)
	}
	if got := NetworkName(&Instance{Network: "campaign-a"}); got != "campaign-a" {
		t.Errorf("persisted network = %q, want campaign-a", got)
	}
}

func TestSaveLoad(t *testing.T) {
	dir := t.TempDir()
	in := &Instance{Name: "s1", Type: "agent", Engine: Podman, Port: 2202, Created: "t"}
	if err := Save(dir, in); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "s1" || got.Port != 2202 {
		t.Errorf("load mismatch: %+v", got)
	}
}

func TestSaveLoadNetwork(t *testing.T) {
	dir := t.TempDir()
	in := &Instance{Name: "s1", Type: "agent", Engine: Firecracker, Network: "campaign-a"}
	if err := Save(dir, in); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Network != "campaign-a" {
		t.Errorf("network = %q, want campaign-a", got.Network)
	}
	if err := Save(dir, &Instance{Name: "bad", Network: "not/a/network"}); err == nil {
		t.Error("Save should reject an invalid network")
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
	if _, err := Load(dir, "../escape"); err == nil {
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
	badDir := filepath.Join(dir, "broken")
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
	badDir := filepath.Join(dir, "broken")
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
