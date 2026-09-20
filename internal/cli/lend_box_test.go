package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codesweep-ai/sandbox/internal/engine"
	"github.com/codesweep-ai/sandbox/internal/lend"
	"github.com/codesweep-ai/sandbox/internal/run"
	"github.com/codesweep-ai/sandbox/internal/state"
)

// The whole isolation claim, read off the command line: the lender is attached
// to ONE group's network and answers to a name on it, so a sandbox in another
// group has no interface to reach it on. Pinned because every part of this line
// is load-bearing and none of it is visible from anywhere else.
func TestLenderBoxJoinsOneNetworkAndNothingElse(t *testing.T) {
	argv := lenderBoxArgv("cs-sandbox-net-lender", lenderBoxSpec{
		Network: "cs-sandbox-net",
		Image:   "localhost/sandbox:test",
		Home:    "/home/dev",
		InstDir: "/home/dev/.local/share/cs-sandbox/instances",
	})
	line := strings.Join(argv, " ")

	for _, want := range []string{
		"--network cs-sandbox-net",
		"--network-alias " + lend.GuestName,
		"--label cs-sandbox.lender=1",
		"--entrypoint cs-sandbox",
		"lender --addr " + lend.DefaultBind + " --callers network",
		// Read-only, both: the lender reads a credential per request and a loan
		// table it does not own (R150, R151).
		"-v /home/dev:/home/dev:ro",
		"-v /home/dev/.local/share/cs-sandbox/instances:/home/dev/.local/share/cs-sandbox/instances:ro",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("the lender command is missing %q:\n%s", want, line)
		}
	}
	// NOTHING is published. A -p here would put the credential swap back on the
	// host's port space, which is the whole thing this design removes.
	if slices.Contains(argv, "-p") || slices.Contains(argv, "--publish") {
		t.Errorf("the lender publishes a host port:\n%s", line)
	}
}

// A host-built binary is handed in rather than the image's, because otherwise
// the lender under test is whatever version the image was built at — and the
// slim CI image carries no cs-sandbox at all.
//
// It runs the STAGED copy, never the source. Mounting the live file maps it in
// the container, and then `go build -o` onto it fails with ETXTBSY for as long
// as any lender is up.
func TestLenderBoxRunsTheStagedCopyNotTheLiveBinary(t *testing.T) {
	spec := lenderBoxSpec{Network: "n", Image: "img", Bin: "/build/cs-sandbox", Stage: "/root/n/.lender/cs-sandbox"}
	line := strings.Join(lenderBoxArgv("n-lender", spec), " ")
	if !strings.Contains(line, "--entrypoint /root/n/.lender/cs-sandbox") {
		t.Errorf("the staged copy is not what runs:\n%s", line)
	}
	if strings.Contains(line, "/build/cs-sandbox") {
		t.Errorf("the source binary reached the container:\n%s", line)
	}
}

// The copy lands under the instances root, which is already mounted, so it
// needs no mount of its own. A second -v for it would be a second thing to keep
// in step with the path the entrypoint names.
func TestTheStagedCopyNeedsNoMountOfItsOwn(t *testing.T) {
	spec := lenderBoxSpec{
		Network: "n", Image: "img", InstDir: "/root",
		Bin: "/build/cs-sandbox", Stage: "/root/n/.lender/cs-sandbox",
	}
	argv := lenderBoxArgv("n-lender", spec)
	mounts := 0
	for i, a := range argv {
		if a == "-v" && i+1 < len(argv) {
			mounts++
		}
	}
	if mounts != 2 {
		t.Errorf("want exactly the home and instances mounts, got %d:\n%s", mounts, strings.Join(argv, " "))
	}
}

// Staging replaces the name rather than the file, so a lender still running on
// the previous copy keeps the inode it has mapped — and the write cannot fail
// with ETXTBSY.
func TestStagingReplacesTheNameNotTheFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, []byte("second build"), 0o755); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(dir, "grp", ".lender", "cs-sandbox")
	if err := os.MkdirAll(filepath.Dir(stage), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stage, []byte("first build"), 0o755); err != nil {
		t.Fatal(err)
	}
	held, err := os.Open(stage) // stands in for the container's mapping
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	b := lenderBox{Spec: lenderBoxSpec{Bin: src, Stage: stage}}
	if err := b.stage(); err != nil {
		t.Fatalf("stage: %v", err)
	}
	got, err := os.ReadFile(stage)
	if err != nil || string(got) != "second build" {
		t.Errorf("the stage holds %q (%v), want the new build", got, err)
	}
	// The open handle still reads what it was opened on: a running lender is
	// unaffected by the next create staging over it.
	old := make([]byte, len("first build"))
	if _, err := held.ReadAt(old, 0); err != nil || string(old) != "first build" {
		t.Errorf("the held copy reads %q (%v), want the build it was started with", old, err)
	}
	if fi, err := os.Stat(stage); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("the staged copy is not executable: %v %v", fi, err)
	}
}

// Every create in a group stages the lender while it is down, and a matrix
// starts several creates at once. Each has to land a complete copy. With one
// shared temporary name, one create's rename took the file another was writing
// or about to rename, which failed that create with ENOENT (SBX-040).
func TestCreatesStagingTogetherEachLandACompleteCopy(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	// Big enough that one create is still writing when another renames.
	want := bytes.Repeat([]byte("lender build\n"), 256<<10)
	if err := os.WriteFile(src, want, 0o755); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(dir, "grp", ".lender", "cs-sandbox")
	b := lenderBox{Spec: lenderBoxSpec{Bin: src, Stage: stage}}

	const creates = 8
	for round := range 10 {
		var wg sync.WaitGroup
		start := make(chan struct{})
		errs := make(chan error, creates)
		for range creates {
			wg.Go(func() {
				<-start
				errs <- b.stage()
			})
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("round %d: stage: %v", round, err)
			}
		}
		got, err := os.ReadFile(stage)
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("round %d: the staged copy holds %d bytes, want the complete %d", round, len(got), len(want))
		}
	}
	// Only the stage itself is left: a create that renamed its copy leaves no
	// temporary file behind.
	entries, err := os.ReadDir(filepath.Dir(stage))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(stage) {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the lender directory holds %v, want only %s", names, filepath.Base(stage))
	}
}

// Ensure adopts what is already there. Every create in a group calls it, and a
// lender that were replaced on each one would drop the connections the sandbox
// before it is holding open.
func TestEnsureAdoptsARunningLender(t *testing.T) {
	fake := run.NewFake()
	fake.OnStdout("container inspect", "true\n")
	b := lenderBox{Runner: fake, Spec: lenderBoxSpec{Network: "cs-sandbox-net", Image: "img"}}
	base, err := b.ensure(context.Background())
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if base != "http://"+lend.GuestName+":2500" {
		t.Errorf("base = %q", base)
	}
	if fake.Contains("podman run") {
		t.Errorf("a running lender was replaced:\n%s", strings.Join(fake.Rendered(), "\n"))
	}
}

// The name a sandbox reaches the lender by is a name a sandbox may not have.
// Two packages have to agree on it, and this is where both are visible.
func TestTheLendersNameIsReservedForIt(t *testing.T) {
	if err := state.ValidName(lend.GuestName); err == nil {
		t.Fatalf("a sandbox may be called %q, which would take the alias from the lender", lend.GuestName)
	}
}

// A loopback upstream meant "this host" while the lender was a host process. It
// means the container itself now, so it is moved onto the name a container
// reaches the host by — and the move is reported, because an upstream is where
// a real credential goes.
func TestALoopbackUpstreamIsMovedOntoTheHostAndSaidSo(t *testing.T) {
	got, note := lenderUpstream("http://127.0.0.1:8080/c/anthropic/build")
	if want := "http://host.containers.internal:8080/c/anthropic/build"; got != want {
		t.Errorf("upstream = %q, want %q", got, want)
	}
	if !strings.Contains(note, "host.containers.internal") || !strings.Contains(note, "127.0.0.1") {
		t.Errorf("the move was not reported in a way that names both ends: %q", note)
	}
}

// Anything that is not loopback is left exactly as the caller wrote it: a
// recorder on another machine is a documented shape (R147), and rewriting one
// would send a credential somewhere its owner did not name.
func TestANonLoopbackUpstreamIsLeftAlone(t *testing.T) {
	for _, u := range []string{
		"http://recorder.example:8080/c/anthropic/build",
		"https://gateway.internal/v1",
		"http://cs-vcr:8080/c/openai/build",
	} {
		if got, note := lenderUpstream(u); got != u || note != "" {
			t.Errorf("lenderUpstream(%q) = %q, %q — want it untouched", u, got, note)
		}
	}
}

// The MTU has to be stated when the lender comes up, not inherited.
//
// A network created before it was pinned still hands the first container onto
// its bridge pasta's 65520 uplink MTU, against a bridge of 1500, and nothing
// anywhere reports the mismatch. Small packets all pass, so the lender answers
// every check that would be run against it. The first packet over 1500 bytes is
// dropped in silence, and a TLS ClientHello is the first thing that big: the
// lender takes the loan, swaps in the real credential, and then cannot finish a
// handshake. The agent above it retries "API error" against a credential that
// was never wrong, which is the most expensive shape this failure could take.
func TestEnsureStatesTheLendersMTU(t *testing.T) {
	fake := run.NewFake()
	fake.OnStdout("{{.State.Pid}}", "4242\n")
	fake.OnStdout("container inspect", "true\n")
	b := lenderBox{Runner: fake, Spec: lenderBoxSpec{Network: "cs-sandbox-net", Image: "img"}}
	if _, err := b.ensure(context.Background()); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	line := strings.Join(fake.Rendered(), "\n")
	if !strings.Contains(line, "ip link set dev eth0 mtu "+engine.BridgeMTU) {
		t.Errorf("the lender's MTU was left to inference:\n%s", line)
	}
}

// TestReachOpensAConnectionAndSendsNoRequest: doctor asks whether the lender can reach an
// upstream, and it asks that of every group on the host. A request would be counted by a
// recorder in front of the provider, and one it never recorded fails a replay. A status of
// 400 or more would also read as a dead upstream, which is what a healthy provider answers
// a bare GET with. So nothing HTTP runs: the connection opening is the whole answer.
func TestReachOpensAConnectionAndSendsNoRequest(t *testing.T) {
	fake := run.NewFake()
	b := lenderBox{Runner: fake, Spec: lenderBoxSpec{Network: "cs-sandbox-net", Image: "img"}}
	if why := b.reach(context.Background(), "https://api.example.com/v1"); why != "" {
		t.Fatalf("a connection that opened was reported as %q", why)
	}
	line := strings.Join(fake.Rendered(), "\n")
	if strings.Contains(line, "curl") {
		t.Errorf("the upstream was sent a request:\n%s", line)
	}
	// The scheme decides the port when the address names none.
	if !strings.Contains(line, "reach api.example.com 443") {
		t.Errorf("want a connection to api.example.com:443:\n%s", line)
	}
}

// TestReachSaysWhyTheConnectionFailed: the reason is what sends a reader to the right hop,
// so it has to be the kernel's, with bash's script name and path taken off the front.
func TestReachSaysWhyTheConnectionFailed(t *testing.T) {
	for _, tc := range []struct {
		name string
		res  run.Result
		want string
	}{
		{"refused", run.Result{ExitCode: 1, Stderr: "reach: connect: Connection refused\n" +
			"reach: line 1: /dev/tcp/host.containers.internal/8080: Connection refused\n"}, "connection refused"},
		{"unknown name", run.Result{ExitCode: 1, Stderr: "reach: line 1: host.containers.internal: Name or service not known\n" +
			"reach: line 1: /dev/tcp/host.containers.internal/8080: Invalid argument\n"}, "name or service not known"},
		{"timed out", run.Result{ExitCode: 124}, "no connection within 5s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := run.NewFake()
			fake.On("/dev/tcp", tc.res, errors.New("exit"))
			b := lenderBox{Runner: fake, Spec: lenderBoxSpec{Network: "cs-sandbox-net", Image: "img"}}
			if why := b.reach(context.Background(), "http://host.containers.internal:8080/c/openai/x"); why != tc.want {
				t.Errorf("reach = %q; want %q", why, tc.want)
			}
		})
	}
}

// TestStoppingALenderKeepsItsLog: the lender's log is the one record of what a provider
// answered a whole group, and podman deletes it with the container. A throttle nobody saw at
// the time can only be found afterwards if the log outlived the group. The order is the
// contract: the log is read before the container is removed.
func TestStoppingALenderKeepsItsLog(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "lender-logs", "g")
	fake := run.NewFake()
	fake.On("podman logs", run.Result{Stderr: "level=INFO msg=answered slot=openai status=429\n"}, nil)
	b := lenderBox{Runner: fake, Spec: lenderBoxSpec{Network: "cs-sandbox-net", Image: "img", LogDir: dir}}
	if err := b.stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	kept, _ := filepath.Glob(filepath.Join(dir, "*.log"))
	if len(kept) != 1 {
		t.Fatalf("want one kept log under %s, got %v", dir, kept)
	}
	if got, _ := os.ReadFile(kept[0]); !strings.Contains(string(got), "status=429") {
		t.Errorf("the kept log does not hold the lender's lines: %q", got)
	}
	if info, _ := os.Stat(kept[0]); info.Mode().Perm() != 0o600 {
		t.Errorf("the kept log is %v; want owner-only, it names every sandbox and path", info.Mode().Perm())
	}
	calls := fake.Rendered()
	logs := slices.IndexFunc(calls, func(c string) bool { return strings.Contains(c, "podman logs") })
	rm := slices.IndexFunc(calls, func(c string) bool { return strings.Contains(c, "podman rm -f") })
	if logs < 0 || rm < 0 || logs > rm {
		t.Errorf("the log has to be read before the container is removed:\n%s", strings.Join(calls, "\n"))
	}
}

// TestStaleLenderLogsAreAgedOut: a kept log is counted per group, and a group or an instance
// root that is gone is never stopped again, so nothing would ever visit its directory. A test
// run makes a root per run. Old logs go whichever lender stops next, with the directories
// they empty, and a recent log of another group is left alone.
func TestStaleLenderLogsAreAgedOut(t *testing.T) {
	root := filepath.Join(t.TempDir(), "lender-logs")
	write := func(rel string, age time.Duration) string {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		when := time.Now().Add(-age)
		if err := os.Chtimes(p, when, when); err != nil {
			t.Fatal(err)
		}
		return p
	}
	stale := write("deadroot/gone/20260101T000000Z.log", 30*24*time.Hour)
	recent := write("otherroot/team/20260919T000000Z.log", 24*time.Hour)

	fake := run.NewFake()
	fake.On("podman logs", run.Result{Stderr: "level=INFO msg=answered\n"}, nil)
	b := lenderBox{Runner: fake, Spec: lenderBoxSpec{Network: "n", Image: "img", LogDir: filepath.Join(root, "default", "g")}}
	if err := b.stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("a 30 day old log was kept: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "deadroot")); !os.IsNotExist(err) {
		t.Errorf("the emptied root directory was left behind: %v", err)
	}
	if _, err := os.Stat(recent); err != nil {
		t.Errorf("a day old log of another group was removed: %v", err)
	}
	if kept, _ := filepath.Glob(filepath.Join(root, "default", "g", "*.log")); len(kept) != 1 {
		t.Errorf("the log just kept is missing: %v", kept)
	}
}
