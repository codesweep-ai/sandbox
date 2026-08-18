package run

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExecIOPassthrough: the real runner forwards Stdin, Env, and Dir to the
// child, captures stdout, and reports a non-zero exit as an *ExitError carrying
// the code and argv.
func TestExecIOPassthrough(t *testing.T) {
	ctx := context.Background()
	e := &Exec{}

	// Stdin is fed to the process.
	if res, err := e.Run(ctx, Opts{Stdin: "hello\n"}, "cat"); err != nil || res.Stdout != "hello\n" {
		t.Errorf("Stdin passthrough = (%q, %v), want (\"hello\\n\", nil)", res.Stdout, err)
	}
	// Env is appended for the child to read.
	if res, err := e.Run(ctx, Opts{Env: []string{"CS_TEST_VAR=xyz"}}, "sh", "-c", "printf %s \"$CS_TEST_VAR\""); err != nil || res.Stdout != "xyz" {
		t.Errorf("Env passthrough = (%q, %v), want (\"xyz\", nil)", res.Stdout, err)
	}
	// Dir sets the working directory.
	tmp := t.TempDir()
	if res, err := e.Run(ctx, Opts{Dir: tmp}, "pwd"); err != nil || strings.TrimSpace(res.Stdout) != tmp {
		t.Errorf("Dir passthrough = (%q, %v), want %q", res.Stdout, err, tmp)
	}
}

// TestExecExitError: a non-zero exit surfaces as an *ExitError with the code and
// argv, and the Result still carries the exit code.
func TestExecExitError(t *testing.T) {
	res, err := (&Exec{}).Run(context.Background(), Opts{}, "sh", "-c", "exit 7")
	var ee *ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("err = %v, want *ExitError", err)
	}
	if ee.ExitCode != 7 || res.ExitCode != 7 {
		t.Errorf("exit code = (%d, %d), want 7/7", ee.ExitCode, res.ExitCode)
	}
	if len(ee.Argv) == 0 || ee.Argv[0] != "sh" {
		t.Errorf("ExitError.Argv = %v, want it to carry the argv", ee.Argv)
	}
}

func TestExecInteractiveExitErrorCarriesArgv(t *testing.T) {
	res, err := (&Exec{}).Run(context.Background(), Opts{Interactive: true}, "sh", "-c", "exit 9")
	var ee *ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("err = %v, want *ExitError", err)
	}
	if ee.ExitCode != 9 || res.ExitCode != 9 {
		t.Errorf("exit code = (%d, %d), want 9/9", ee.ExitCode, res.ExitCode)
	}
	if got := strings.Join(ee.Argv, " "); got != "sh -c exit 9" {
		t.Errorf("ExitError.Argv = %q, want the interactive command", got)
	}
}

// TestExitErrorError pins the failure message format callers surface to users.
func TestExitErrorError(t *testing.T) {
	e := &ExitError{Argv: []string{"podman", "ps"}, ExitCode: 2, Stderr: " boom\n"}
	want := "command failed (exit 2): podman ps: boom"
	if got := e.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

// TestOutput: success returns trimmed stdout; any error yields "".
func TestOutput(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	f.OnStdout("podman version", "  1.2.3 \n")
	f.On("git rev-parse", Result{Stdout: "ignored"}, errors.New("boom"))

	if got := Output(ctx, f, "podman", "version"); got != "1.2.3" {
		t.Errorf("Output(success) = %q, want 1.2.3", got)
	}
	if got := Output(ctx, f, "git", "rev-parse", "HEAD"); got != "" {
		t.Errorf("Output(error) = %q, want empty", got)
	}
	if got := Output(ctx, f, "unmatched", "cmd"); got != "" {
		t.Errorf("Output(default) = %q, want empty", got)
	}
	if len(f.Calls) == 0 {
		t.Fatal("Output did not invoke the runner")
	}
}

// TestFakeRecording: On matches in insertion order, Calls/Rendered/Contains
// reflect what ran.
func TestFakeRecording(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	f.On("volume ls", Result{Stdout: "first"}, nil)
	f.On("volume", Result{Stdout: "second"}, nil) // broader, must lose to the earlier match

	res, _ := f.Run(ctx, Opts{}, "podman", "volume", "ls")
	if res.Stdout != "first" {
		t.Errorf("first-match-wins broken: got %q, want first", res.Stdout)
	}
	res, _ = f.Run(ctx, Opts{}, "podman", "volume", "create", "x")
	if res.Stdout != "second" {
		t.Errorf("broader match: got %q, want second", res.Stdout)
	}

	if len(f.Calls) != 2 {
		t.Fatalf("Calls = %d, want 2", len(f.Calls))
	}
	if r := f.Rendered(); r[0] != "podman volume ls" {
		t.Errorf("Rendered[0] = %q", r[0])
	}
	if !f.Contains("volume create x") {
		t.Error("Contains should find the create call")
	}
	if f.Contains("network rm") {
		t.Error("Contains should not find an absent call")
	}
}

// TestExecEmptyArgv: Run rejects an empty argv.
func TestExecEmptyArgv(t *testing.T) {
	if _, err := (&Exec{}).Run(context.Background(), Opts{}); err == nil {
		t.Error("empty argv should error")
	}
}

// TestExecDryRun: under DryRun a mutation is printed and skipped, but a ReadOnly
// query still executes (its output is needed to render the mutation).
func TestExecDryRun(t *testing.T) {
	ctx := context.Background()
	var printed [][]string
	e := &Exec{DryRun: true, Printer: func(a []string) { printed = append(printed, a) }}

	// Mutation: skipped, empty result, but printed.
	res, err := e.Run(ctx, Opts{}, "echo", "mutate")
	if err != nil || res.Stdout != "" {
		t.Errorf("dry-run mutation = (%q, %v), want empty/no-op", res.Stdout, err)
	}
	// ReadOnly: actually runs even under DryRun.
	res, err = e.Run(ctx, Opts{ReadOnly: true}, "echo", "query")
	if err != nil {
		t.Fatalf("dry-run read-only errored: %v", err)
	}
	if res.Stdout != "query\n" {
		t.Errorf("read-only under dry-run should execute: got %q", res.Stdout)
	}
	if len(printed) != 2 {
		t.Errorf("both calls should be printed, got %d", len(printed))
	}
}

// TestExecStdoutFile: StdoutFile hands the child the file as its stdout, so the
// output never passes through this process — Result.Stdout stays empty and the
// bytes land on disk. What needs this is the image-store tar, which is the whole
// sandbox image and does not belong in memory. Stderr is still captured, because
// that is what an error has to report.
func TestExecStdoutFile(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "out.bin")
	res, err := (&Exec{}).Run(context.Background(), Opts{StdoutFile: dst},
		"sh", "-c", "printf payload; printf oops >&2")
	if err != nil {
		t.Fatalf("Run = %v", err)
	}
	if res.Stdout != "" {
		t.Errorf("Result.Stdout = %q, want empty when it went to a file", res.Stdout)
	}
	if res.Stderr != "oops" {
		t.Errorf("Result.Stderr = %q, want %q", res.Stderr, "oops")
	}
	b, err := os.ReadFile(dst)
	if err != nil || string(b) != "payload" {
		t.Fatalf("file = (%q, %v), want (\"payload\", nil)", b, err)
	}
}

// TestExecStdoutFileTruncates: the file is replaced, never appended to. A build
// that reran over a stale temp would otherwise tar a valid archive onto the end
// of an old one and hand the result to mke2fs.
func TestExecStdoutFileTruncates(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "out.bin")
	if err := os.WriteFile(dst, []byte("stale-and-longer"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (&Exec{}).Run(context.Background(), Opts{StdoutFile: dst}, "printf", "new"); err != nil {
		t.Fatalf("Run = %v", err)
	}
	if b, err := os.ReadFile(dst); err != nil || string(b) != "new" {
		t.Fatalf("file = (%q, %v), want (\"new\", nil)", b, err)
	}
}

// TestExecStdoutFileUnwritable: a path that cannot be created fails before the
// command runs, rather than running it and dropping the output.
func TestExecStdoutFileUnwritable(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "no-such-dir", "out.bin")
	if _, err := (&Exec{}).Run(context.Background(), Opts{StdoutFile: dst}, "printf", "x"); err == nil {
		t.Fatal("Run = nil, want an error for an uncreatable stdout file")
	}
}

// TestFakeStdoutFile: the fake honours StdoutFile the way Exec does. Without it
// a caller that streams its output is untestable through the seam — the canned
// bytes would arrive as Result.Stdout, which is the one thing the real runner
// does not do.
func TestFakeStdoutFile(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "out.bin")
	f := NewFake().OnStdout("tar", "archive-bytes")
	res, err := f.Run(context.Background(), Opts{StdoutFile: dst}, "tar", "-cf", "-", ".")
	if err != nil {
		t.Fatalf("Run = %v", err)
	}
	if res.Stdout != "" {
		t.Errorf("Result.Stdout = %q, want empty", res.Stdout)
	}
	if b, rerr := os.ReadFile(dst); rerr != nil || string(b) != "archive-bytes" {
		t.Fatalf("file = (%q, %v), want (\"archive-bytes\", nil)", b, rerr)
	}
}
