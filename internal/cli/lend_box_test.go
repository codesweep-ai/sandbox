package cli

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

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
