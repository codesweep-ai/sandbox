package forward

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/codesweep-ai/sandbox/internal/hostcfg"
	"github.com/codesweep-ai/sandbox/internal/hostenv"
	"github.com/codesweep-ai/sandbox/internal/proc"

	"github.com/codesweep-ai/sandbox/internal/state"
)

// TestForwardArgs pins the ssh argv for both a local (-L) forward and a SOCKS
// (-D) proxy.
func TestForwardArgs(t *testing.T) {
	h := hostenv.Host{User: "dev"}

	local := forwardArgs(h, "/tier", state.DefaultGroup, "box", hostcfg.Route{Port: 2200}, "L", 18099, "localhost:80", "127.0.0.1")
	for _, want := range []string{"-N", "-L", "127.0.0.1:18099:localhost:80", "dev@127.0.0.1"} {
		if !slices.Contains(local, want) {
			t.Errorf("local forward args missing %q: %v", want, local)
		}
	}
	if slices.Contains(local, "-D") {
		t.Errorf("local forward should not use -D: %v", local)
	}

	socks := forwardArgs(h, "/tier", state.DefaultGroup, "box", hostcfg.Route{Port: 2200}, "D", 1080, "socks", "127.0.0.1")
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
		args := forwardArgs(h, "/tier", group, "api", hostcfg.Route{Port: 2200}, "L", 18099, "localhost:80", "127.0.0.1")
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
	// A live forward.
	livePath := filepath.Join(d, "18099")
	live := &Record{Kind: "L", HostPort: 18099, Target: "localhost:80", Bind: "127.0.0.1"}
	startFakeSSH(t, live)
	if err := writeRecord(livePath, live); err != nil {
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
	// A live forward whose ssh Remove will SIGTERM.
	r := &Record{Kind: "L", HostPort: 18099, Target: "localhost:80", Bind: "127.0.0.1"}
	child := startFakeSSH(t, r)
	if err := writeRecord(filepath.Join(d, "18099"), r); err != nil {
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
	if !signalled(child, syscall.SIGTERM) {
		t.Error("Remove did not SIGTERM the forward's ssh")
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
	// A record that old predates start times too, so it is matched on argv.
	r := &Record{Kind: "L", HostPort: 18099, Target: "localhost:80", Bind: "127.0.0.1"}
	child := startFakeSSH(t, r)
	r.Start = ""
	rec := filepath.Join(legacy, "18099")
	if err := writeRecord(rec, r); err != nil {
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
	// And the process it recorded is signalled, not left running.
	if !signalled(child, syscall.SIGTERM) {
		t.Error("legacy forward's process was not terminated by SIGTERM")
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

// startFakeSSH starts a stand-in for r's ssh child and records its pid and
// start time in r. Its argv is what identifies it: ssh, -N, and r's forward
// spec. A shell loop rather than one sleep keeps that argv in place, as
// startFakeVMM explains in the engine's tests.
func startFakeSSH(t *testing.T, r *Record) *exec.Cmd {
	t.Helper()
	flag, spec := forwardSpec(r.Kind, r.HostPort, r.Target, r.Bind)
	cmd := exec.Command("sh", "-c", "while :; do sleep 1; done", "ssh", "-N", flag, spec)
	cmd.Args[0] = "ssh"
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	id := proc.Of(cmd.Process.Pid)
	r.PID, r.Start = id.PID, id.Start
	return cmd
}

// signalled reaps cmd and reports whether sig ended it. Reaping is what
// observes the death: a probe of the pid would still find the zombie.
func signalled(cmd *exec.Cmd, sig syscall.Signal) bool {
	var exit *exec.ExitError
	if !errors.As(cmd.Wait(), &exit) {
		return false
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	return ok && status.Signaled() && status.Signal() == sig
}

// needProc skips a test of telling two holders of one pid apart. Without /proc
// there is no start time or argv to tell them by, and R170 judges a record by
// its pid alone.
func needProc(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("no /proc here: a record is judged by its pid alone")
	}
}

// startBystander starts a process of this user's that is no forward, the kind
// a pid recorded before a reboot can name afterwards. The channel closes if it
// exits.
func startBystander(t *testing.T) (*exec.Cmd, <-chan struct{}) {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	gone := make(chan struct{})
	go func() { _, _ = cmd.Process.Wait(); close(gone) }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-gone })
	return cmd, gone
}

func stillRunning(t *testing.T, gone <-chan struct{}) {
	t.Helper()
	select {
	case <-gone:
		t.Fatal("an unrelated process holding a forward's recorded pid was killed")
	case <-time.After(300 * time.Millisecond):
	}
}

// writeRaw writes a record by hand, so a test can give it any start time,
// including none, as a record from before start times were kept has.
func writeRaw(t *testing.T, path string, pid int, start string) {
	t.Helper()
	rec := "pid=" + strconv.Itoa(pid) + "\nkind=L\nhostport=18099\ntarget=localhost:80\nbind=127.0.0.1\n"
	if start != "" {
		rec += "start=" + start + "\n"
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(rec), 0o600); err != nil {
		t.Fatal(err)
	}
}

// SBX-002: a forward record survives a reboot, and its pid can then name any
// process of yours. `forwards` must drop the record, and `unforward`, `rm` and
// `destroy` must not signal the process.
func TestAReusedForwardPidIsDeadAndNeverSignalled(t *testing.T) {
	needProc(t)
	for _, start := range []string{"", "1"} { // no start time, and one the bystander does not have
		instDir := t.TempDir()
		rec := filepath.Join(dir(instDir, "grp", "box"), "18099")
		cmd, gone := startBystander(t)

		writeRaw(t, rec, cmd.Process.Pid, start)
		if list, _ := List(instDir, "grp", "box"); len(list) != 0 {
			t.Errorf("start=%q: List = %+v, want the stale record treated as dead", start, list)
		}
		if _, err := os.Stat(rec); !os.IsNotExist(err) {
			t.Errorf("start=%q: the stale record was not collected", start)
		}

		writeRaw(t, rec, cmd.Process.Pid, start)
		if n, _ := Remove(instDir, "grp", "box", "all"); n != 0 {
			t.Errorf("start=%q: Remove tore down %d forwards, want 0", start, n)
		}

		writeRaw(t, rec, cmd.Process.Pid, start)
		writeRaw(t, filepath.Join(legacyDir(instDir, "box.grp"), "18099"), cmd.Process.Pid, start)
		KillAll(instDir, "grp", "box")
		stillRunning(t, gone)
	}
}
