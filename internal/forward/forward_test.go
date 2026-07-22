package forward

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/codesweep-ai/sandbox/internal/hostenv"
)

// TestForwardArgs pins the ssh argv for both a local (-L) forward and a SOCKS
// (-D) proxy.
func TestForwardArgs(t *testing.T) {
	h := hostenv.Host{User: "dev"}

	local := forwardArgs(h, "/tier", "box", 2200, "L", 18099, "localhost:80", "127.0.0.1")
	for _, want := range []string{"-N", "-L", "127.0.0.1:18099:localhost:80", "dev@127.0.0.1"} {
		if !slices.Contains(local, want) {
			t.Errorf("local forward args missing %q: %v", want, local)
		}
	}
	if slices.Contains(local, "-D") {
		t.Errorf("local forward should not use -D: %v", local)
	}

	socks := forwardArgs(h, "/tier", "box", 2200, "D", 1080, "socks", "127.0.0.1")
	for _, want := range []string{"-N", "-D", "127.0.0.1:1080", "dev@127.0.0.1"} {
		if !slices.Contains(socks, want) {
			t.Errorf("socks args missing %q: %v", want, socks)
		}
	}
	if slices.Contains(socks, "-L") {
		t.Errorf("socks forward should not use -L: %v", socks)
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
	d := dir(instDir, "box")
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

	list, err := List(instDir, "box")
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
	d := dir(instDir, "box")
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

	n, err := Remove(instDir, "box", "all")
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
	if list, _ := List(instDir, "box"); len(list) != 0 {
		t.Errorf("List after Remove = %+v, want empty", list)
	}
}
