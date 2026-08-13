package doctor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codesweep-ai/sandbox/internal/run"
)

// TestEnsureDirCreatesAndRemovesExactly: the probe must be able to run in a
// location that does not exist yet (before the first build) without leaving
// anything behind — and without removing anything it did not create.
func TestEnsureDirCreatesAndRemovesExactly(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "a", "b", "c")

	undo, err := ensureDir(target)
	if err != nil {
		t.Fatalf("ensureDir: %v", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("ensureDir did not create %s: %v", target, err)
	}
	undo()
	for _, p := range []string{target, filepath.Join(root, "a", "b"), filepath.Join(root, "a")} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s survived the cleanup (err=%v), want removed", p, err)
		}
	}
	// The pre-existing root is not ours to delete.
	if _, err := os.Stat(root); err != nil {
		t.Errorf("cleanup removed the pre-existing root: %v", err)
	}
}

// TestEnsureDirLeavesExistingAlone: an already-present directory must survive
// the probe — this runs against the real artifact cache and instances tree.
func TestEnsureDirLeavesExistingAlone(t *testing.T) {
	dir := t.TempDir()
	keep := filepath.Join(dir, "keep")
	if err := os.WriteFile(keep, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	undo, err := ensureDir(dir)
	if err != nil {
		t.Fatalf("ensureDir: %v", err)
	}
	undo()
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("cleanup damaged a pre-existing directory: %v", err)
	}
}

// TestEnsureDirKeepsDirectoriesThatGainedContent: cleanup removes only what is
// still empty, so a concurrent create that lands in a directory the probe made
// does not lose it.
func TestEnsureDirKeepsDirectoriesThatGainedContent(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "x", "y")
	undo, err := ensureDir(target)
	if err != nil {
		t.Fatalf("ensureDir: %v", err)
	}
	occupied := filepath.Join(root, "x", "someone-elses-file")
	if err := os.WriteFile(occupied, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	undo()
	if _, err := os.Stat(occupied); err != nil {
		t.Errorf("cleanup removed a directory that had gained content: %v", err)
	}
}

// TestReflinkWorksProbesTheRealPair: the probe copies from the cache dir to the
// instances dir with --reflink=always (--auto would succeed on a plain copy and
// prove nothing), and leaves neither file behind.
func TestReflinkWorksProbesTheRealPair(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	f := run.NewFake()
	if !reflinkWorks(context.Background(), f, src, dst) {
		t.Fatalf("reflinkWorks = false on a runner that succeeds:\n%s", f)
	}
	if len(f.Calls) != 1 {
		t.Fatalf("ran %d commands, want 1:\n%s", len(f.Calls), f)
	}
	got := strings.Join(f.Calls[0], " ")
	if !strings.HasPrefix(got, "cp --reflink=always ") {
		t.Errorf("probe = %q, want a cp --reflink=always", got)
	}
	if !strings.Contains(got, src) || !strings.Contains(got, dst) {
		t.Errorf("probe %q does not copy from %s to %s", got, src, dst)
	}
	for _, d := range []string{src, dst} {
		ents, err := os.ReadDir(d)
		if err != nil {
			t.Fatal(err)
		}
		if len(ents) != 0 {
			t.Errorf("probe left %d file(s) in %s", len(ents), d)
		}
	}
}

// TestReflinkWorksFalseWhenCpFails: cp refusing the clone (ext4, or a source and
// destination on different filesystems) is the whole signal.
func TestReflinkWorksFalseWhenCpFails(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	f := run.NewFake().On("cp --reflink=always", run.Result{}, &run.ExitError{
		Argv: []string{"cp"}, ExitCode: 1, Stderr: "failed to clone: Operation not supported",
	})
	if reflinkWorks(context.Background(), f, src, dst) {
		t.Error("reflinkWorks = true although cp failed")
	}
}

// TestReflinkCheckReportsCostNotCapability: the warning has to carry the number
// that decides whether anyone cares — the base rootfs's real size, per sandbox —
// and stay informational, since nothing is broken without reflink.
func TestReflinkCheckReportsCostNotCapability(t *testing.T) {
	cache, inst := t.TempDir(), t.TempDir()
	// A base rootfs with ~2 MiB of real data.
	if err := os.WriteFile(filepath.Join(cache, "base-rootfs.ext4"),
		[]byte(strings.Repeat("a", 2<<20)), 0o600); err != nil {
		t.Fatal(err)
	}
	d := Deps{FCCache: cache, InstDir: inst, Runner: run.NewFake().
		On("cp --reflink=always", run.Result{}, &run.ExitError{Argv: []string{"cp"}, ExitCode: 1})}

	status, msg := reflinkCheck(context.Background(), d)
	if status != HM {
		t.Errorf("status = %v, want HM (informational — sandboxes still work)", status)
	}
	for _, want := range []string{"no reflink", "GiB"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q: %s", want, msg)
		}
	}
	// One clause, like every other check in the report.
	if len(msg) > 90 {
		t.Errorf("message is %d chars, too long for a doctor line: %s", len(msg), msg)
	}

	// And the healthy case says what it buys, not just "ok".
	d.Runner = run.NewFake()
	if status, msg := reflinkCheck(context.Background(), d); status != OK ||
		!strings.Contains(msg, "reflink") {
		t.Errorf("healthy check = (%v, %q), want OK mentioning reflink", status, msg)
	}
}

// TestReflinkCheckUnconfigured: with no paths to probe the check says so rather
// than claiming either answer.
func TestReflinkCheckUnconfigured(t *testing.T) {
	status, msg := reflinkCheck(context.Background(), Deps{Runner: run.NewFake()})
	if status != HM || !strings.Contains(msg, "unchecked") {
		t.Errorf("unconfigured check = (%v, %q), want HM/unchecked", status, msg)
	}
}
