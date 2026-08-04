package forward

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/codesweep-ai/sandbox/internal/hostenv"

	"github.com/codesweep-ai/sandbox/internal/state"
)

// TestForwardArgs pins the ssh argv for both a local (-L) forward and a SOCKS
// (-D) proxy.
func TestForwardArgs(t *testing.T) {
	h := hostenv.Host{User: "dev"}

	local := forwardArgs(h, "/tier", state.DefaultGroup, "box", 2200, "L", 18099, "localhost:80", "127.0.0.1")
	for _, want := range []string{"-N", "-L", "127.0.0.1:18099:localhost:80", "dev@127.0.0.1"} {
		if !slices.Contains(local, want) {
			t.Errorf("local forward args missing %q: %v", want, local)
		}
	}
	if slices.Contains(local, "-D") {
		t.Errorf("local forward should not use -D: %v", local)
	}

	socks := forwardArgs(h, "/tier", state.DefaultGroup, "box", 2200, "D", 1080, "socks", "127.0.0.1")
	for _, want := range []string{"-N", "-D", "127.0.0.1:1080", "dev@127.0.0.1"} {
		if !slices.Contains(socks, want) {
			t.Errorf("socks args missing %q: %v", want, socks)
		}
	}
	if slices.Contains(socks, "-L") {
		t.Errorf("socks forward should not use -L: %v", socks)
	}
}

// A forward must key known_hosts by the host-global object name. Two groups
// running the same fixture otherwise share one entry, and the second forward
// fails "host key changed" — under BatchMode, with no one to accept the new key.
func TestForwardArgsKeysHostKeyOnTheObjectName(t *testing.T) {
	h := hostenv.Host{User: "dev"}
	seen := map[string]string{}
	for _, group := range []string{"cache-redis", "cache-memory", state.DefaultGroup} {
		args := forwardArgs(h, "/tier", group, "api", 2200, "L", 18099, "localhost:80", "127.0.0.1")
		i := slices.Index(args, "-o")
		alias := ""
		for ; i >= 0 && i < len(args)-1; i++ {
			if args[i] == "-o" && strings.HasPrefix(args[i+1], "HostKeyAlias=") {
				alias = strings.TrimPrefix(args[i+1], "HostKeyAlias=")
				break
			}
		}
		if want := state.ObjectName(group, "api"); alias != want {
			t.Errorf("group %q: HostKeyAlias %q, want %q", group, alias, want)
		}
		if other, dup := seen[alias]; dup {
			t.Errorf("groups %q and %q collide on HostKeyAlias %q", other, group, alias)
		}
		seen[alias] = group
	}
}

// TestRecordRoundTrip: writeRecord then readRecord preserves all fields.
func TestRecordRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "18099")
	want := &Record{PID: 4242, Kind: "L", HostPort: 18099, Target: "localhost:80", Bind: "127.0.0.1"}
	if err := writeRecord(p, want); err != nil {
		t.Fatal(err)
	}
	got, err := readRecord(p)
	if err != nil {
		t.Fatal(err)
	}
	if got != *want {
		t.Errorf("round-trip = %+v, want %+v", got, *want)
	}
}

// TestListGCsDeadRecords: List returns only forwards whose process is alive,
// removes the dead ones, and ignores .log files.
func TestListGCsDeadRecords(t *testing.T) {
	instDir := t.TempDir()
	d := dir(instDir, "grp", "box")
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	// A live forward — this test process is alive.
	livePath := filepath.Join(d, "18099")
	if err := writeRecord(livePath, &Record{PID: os.Getpid(), Kind: "L", HostPort: 18099, Target: "localhost:80"}); err != nil {
		t.Fatal(err)
	}
	// A dead forward — spawn and reap a child so its PID is definitely gone.
	c := exec.Command("true")
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	deadPID := c.Process.Pid
	_ = c.Wait()
	deadPath := filepath.Join(d, "18100")
	if err := writeRecord(deadPath, &Record{PID: deadPID, Kind: "L", HostPort: 18100}); err != nil {
		t.Fatal(err)
	}
	// A stray .log file must be ignored.
	if err := os.WriteFile(filepath.Join(d, "18099.log"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	list, err := List(instDir, "grp", "box")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].HostPort != 18099 {
		t.Fatalf("List = %+v, want only the live 18099", list)
	}
	if _, err := os.Stat(deadPath); !os.IsNotExist(err) {
		t.Error("List should have GC'd the dead record file")
	}
}

// TestRemove tears down a live forward: kills its process, drops the record, and
// counts it.
func TestRemove(t *testing.T) {
	instDir := t.TempDir()
	d := dir(instDir, "grp", "box")
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	// A live child whose PID Remove will SIGTERM (not this test process).
	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	if err := writeRecord(filepath.Join(d, "18099"), &Record{PID: child.Process.Pid, Kind: "L", HostPort: 18099}); err != nil {
		t.Fatal(err)
	}

	n, err := Remove(instDir, "grp", "box", "all")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("Remove(all) = %d, want 1", n)
	}
	if _, err := os.Stat(filepath.Join(d, "18099")); !os.IsNotExist(err) {
		t.Error("Remove should have deleted the record file")
	}
	// Nothing left to list.
	if list, _ := List(instDir, "grp", "box"); len(list) != 0 {
		t.Errorf("List after Remove = %+v, want empty", list)
	}
}

// Records live inside the instance's own directory, not at a path derived from
// whatever reference the caller typed. Keying on the reference put them beside
// the group directories at the instances root, where `group rm` could not
// reclaim them and a destroy spelled differently could not find them.
func TestRecordsLiveInsideTheInstanceDirectory(t *testing.T) {
	instDir := t.TempDir()
	got := dir(instDir, "cache-redis", "api")
	want := filepath.Join(state.Dir(instDir, "cache-redis", "api"), "forwards")
	if got != want {
		t.Fatalf("forwards dir = %q, want %q", got, want)
	}
	// Specifically NOT the qualified ref at the instances root.
	if stray := filepath.Join(instDir, "api.cache-redis", "forwards"); got == stray {
		t.Fatalf("forwards dir is still keyed on the reference: %q", got)
	}
}

// An upgrade must not strand a live ssh process. Forwards recorded by an older
// build sit at the pre-identity path, invisible to every command; teardown
// sweeps them so the process is killed and the stray directory reclaimed.
func TestKillAllSweepsPreIdentityRecords(t *testing.T) {
	instDir := t.TempDir()
	legacy := legacyDir(instDir, "api.cache-redis")
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	reaped := false
	t.Cleanup(func() {
		if !reaped {
			_ = child.Process.Kill()
			_ = child.Wait()
		}
	})
	rec := filepath.Join(legacy, "18099")
	if err := writeRecord(rec, &Record{PID: child.Process.Pid, Kind: "L", HostPort: 18099}); err != nil {
		t.Fatal(err)
	}

	KillAll(instDir, "cache-redis", "api")

	if _, err := os.Stat(rec); !os.IsNotExist(err) {
		t.Error("legacy record survived teardown")
	}
	// The stray <instances>/<ref>/ directory goes too — it is the leak the
	// old keying produced, and nothing else will ever reclaim it.
	if _, err := os.Stat(filepath.Join(instDir, "api.cache-redis")); !os.IsNotExist(err) {
		t.Error("stray reference-keyed directory survived teardown")
	}
	// And the process it recorded is signalled, not left running. Asserted by
	// reaping it: alive() is a kill(pid, 0) probe, which still succeeds for an
	// unreaped zombie, so polling it here could never observe the death.
	err := child.Wait()
	reaped = true
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("legacy forward's process exited normally (%v); it should have been signalled", err)
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGTERM {
		t.Errorf("legacy forward's process wait status = %v, want terminated by SIGTERM", exit)
	}
}

// A default-group member could be addressed bare, so its legacy records sit
// under the bare name; both spellings have to be swept.
func TestKillAllSweepsBareLegacyPathForDefaultGroup(t *testing.T) {
	instDir := t.TempDir()
	legacy := legacyDir(instDir, "api")
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeRecord(filepath.Join(legacy, "18099"), &Record{PID: -1, Kind: "L", HostPort: 18099}); err != nil {
		t.Fatal(err)
	}
	KillAll(instDir, state.DefaultGroup, "api")
	if _, err := os.Stat(filepath.Join(instDir, "api")); !os.IsNotExist(err) {
		t.Error("bare legacy directory survived teardown for a default-group member")
	}
}
