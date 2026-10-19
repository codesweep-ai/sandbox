package proc

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// The comm field is the process name in parentheses, and a name may itself
// hold spaces and parentheses. Counting fields from the start of the line
// would read the wrong field for such a process.
func TestParseStatCountsFromTheLastParen(t *testing.T) {
	tail := " S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 987654 20 21"
	for _, comm := range []string{"(ssh)", "(a b)", "(x) S 9 (y)", "())"} {
		s, ok := parseStat("4242 " + comm + tail)
		if !ok || s.state != 'S' || s.start != "987654" {
			t.Errorf("comm %s: parseStat = %+v, %v; want state S, start 987654", comm, s, ok)
		}
	}
	for _, bad := range []string{"", "4242 no paren", "4242 (short) S 1 2"} {
		if _, ok := parseStat(bad); ok {
			t.Errorf("parseStat(%q) succeeded on a malformed line", bad)
		}
	}
}

func start(t *testing.T, argv ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(argv[0], argv[1:]...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd
}

func isSleep(argv []string) bool { return len(argv) > 0 && filepath.Base(argv[0]) == "sleep" }

func TestFindTheRecordedProcess(t *testing.T) {
	cmd := start(t, "sleep", "60")
	id := Of(cmd.Process.Pid)
	if id.Start == "" {
		t.Fatal("Of read no start time for a live child")
	}
	h := Find(id, isSleep)
	if h == nil {
		t.Fatal("Find did not recognise the process it was given")
	}
	defer h.Close()
	if !h.Verified() {
		t.Error("a match on the start time reports unverified")
	}
	if err := h.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	var ws syscall.WaitStatus
	if _, err := syscall.Wait4(cmd.Process.Pid, &ws, 0, nil); err != nil || ws.Signal() != syscall.SIGTERM {
		t.Errorf("wait = %v, %v; want terminated by SIGTERM", ws, err)
	}
	// Reaped, the pid is free for reuse, and the handle must not reach it.
	if h.Running() {
		t.Error("a reaped process still reads as running")
	}
	if h.Signal(0) == nil {
		t.Error("a signal through the handle reached a reaped process's pid")
	}
}

// What a stale record meets: the pid is held, but not by the process recorded.
func TestFindRefusesAnotherProcess(t *testing.T) {
	cmd := start(t, "sleep", "60")
	pid := cmd.Process.Pid
	for name, tc := range map[string]struct {
		id    Ident
		match Match
	}{
		"another start time":          {Ident{PID: pid, Start: "1"}, isSleep},
		"another argv":                {Of(pid), func([]string) bool { return false }},
		"no start time, another argv": {Ident{PID: pid}, func([]string) bool { return false }},
	} {
		if Find(tc.id, tc.match) != nil || Alive(tc.id, tc.match) {
			t.Errorf("%s: Find took the process for the recorded one", name)
		}
	}
	if h := Find(Ident{PID: pid}, isSleep); h == nil || h.Verified() {
		t.Error("a record with no start time must match on argv alone, and report unverified")
	}
}

// A zombie has exited. Only its parent's wait is outstanding, so it is not
// running, though signal 0 still finds its pid.
func TestAZombieIsNotRunning(t *testing.T) {
	cmd := start(t, "true")
	id := Of(cmd.Process.Pid)
	for range 100 {
		if s, _ := statOf(id.PID); s.state == 'Z' {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if s, _ := statOf(id.PID); s.state != 'Z' {
		t.Skip("the child did not become a zombie in time")
	}
	if Alive(id, nil) {
		t.Error("Alive reports a zombie as running")
	}
}

func TestPidFileRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fc.pid")
	want := Ident{PID: 4242, Start: "987654"}
	if err := WriteFile(p, want); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadFile(p); err != nil || got != want {
		t.Errorf("ReadFile = %+v, %v; want %+v", got, err, want)
	}
	// A record from before start times holds the pid alone.
	if err := os.WriteFile(p, []byte("4242\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadFile(p); err != nil || got != (Ident{PID: 4242}) {
		t.Errorf("ReadFile of a pid-only record = %+v, %v", got, err)
	}
}

// The VMM's argv names its config by the path the launch had, which for a
// relative instances directory is relative.
func TestVMMArgvMatchesTheInstanceConfig(t *testing.T) {
	idir := t.TempDir()
	cfg := filepath.Join(idir, VMMConfig)
	m := VMMArgv(idir)
	if !m([]string{"podman", "unshare", "--rootless-netns", "fc", "--config-file", cfg}) {
		t.Error("the instance's own VMM argv did not match")
	}
	if m([]string{"podman", "unshare", "fc", "--config-file", filepath.Join(t.TempDir(), VMMConfig)}) {
		t.Error("another instance's VMM matched")
	}
	if m([]string{"sleep", "60"}) {
		t.Error("an unrelated argv matched")
	}
	wd, _ := os.Getwd()
	rel, err := filepath.Rel(wd, idir)
	if err != nil {
		t.Skip(err)
	}
	if !VMMArgv(rel)([]string{"fc", "--config-file", cfg}) {
		t.Error("a relative instance dir did not match its absolute config path")
	}
}

// Where pidfds are refused, the identity is checked again right before the
// signal goes by pid.
func TestSignalWithoutAPidfdRechecksTheIdentity(t *testing.T) {
	cmd := start(t, "sleep", "60")
	stale := &Handle{id: Ident{PID: cmd.Process.Pid, Start: "1"}, match: isSleep, fd: -1}
	if err := stale.Signal(syscall.SIGTERM); err != syscall.ESRCH {
		t.Errorf("Signal on a reused pid = %v, want ESRCH", err)
	}
	h := &Handle{id: Of(cmd.Process.Pid), match: isSleep, fd: -1}
	if !h.Running() {
		t.Fatal("the recorded process does not read as running")
	}
	if err := h.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	var ws syscall.WaitStatus
	if _, err := syscall.Wait4(cmd.Process.Pid, &ws, 0, nil); err != nil || ws.Signal() != syscall.SIGTERM {
		t.Errorf("wait = %v, %v; want terminated by SIGTERM", ws, err)
	}
}
