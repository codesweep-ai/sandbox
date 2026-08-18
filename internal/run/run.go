// Package run is the single seam through which every external command
// (podman, firecracker, ssh, git, ip, dnsmasq, socat, …) is executed.
//
// Routing all subprocess work through one Runner interface gives the whole
// tool a single mock point (unit tests assert argv;
// integration tests use the real runner) and one place for logging, timeouts,
// context cancellation, and --dry-run/--print-commands rendering.
package run

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Result is the outcome of a completed command.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Runner executes external commands. Implementations: Exec (real) and the
// recording fake in package run's fake.go used by unit tests.
type Runner interface {
	// Run executes argv[0] with argv[1:] and returns its captured output.
	// A non-zero exit is reported via the returned error (an *ExitError);
	// callers that tolerate failure inspect Result.ExitCode instead.
	Run(ctx context.Context, opts Opts, argv ...string) (Result, error)
}

// Opts carries per-invocation options that don't belong in argv.
type Opts struct {
	Stdin string   // fed to the process stdin
	Env   []string // extra environment (appended to os.Environ() unless EnvClean)
	Dir   string   // working directory (empty = inherit)
	// Interactive attaches the child directly to the parent's stdio (for ssh,
	// podman exec -it, etc.). Stdout/Stderr are then not captured.
	Interactive bool
	// ReadOnly marks a query with no side effects (network/volume/image inspect,
	// git config reads). Under DryRun these still execute so a --dry-run of a
	// mutating command can resolve real values and reach the mutation it would
	// print, while the mutation itself is printed-and-skipped.
	ReadOnly bool
	// StdoutFile, when set, writes the process's stdout straight to that path
	// instead of capturing it, and leaves Result.Stdout empty. For output that
	// does not belong in memory: the image-store tar is the whole sandbox image,
	// gigabytes of it, and capturing that cost twice its size in RAM — once in
	// the buffer and again in the string copied out of it.
	//
	// The file is created (truncating an existing one) before the process starts
	// and handed over as its stdout fd, so the bytes never pass through this
	// process at all. Ignored when Interactive is set, which owns stdio.
	StdoutFile string
}

// ExitError reports a non-zero exit from a command.
type ExitError struct {
	Argv     []string
	ExitCode int
	Stderr   string
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("command failed (exit %d): %s: %s",
		e.ExitCode, strings.Join(e.Argv, " "), strings.TrimSpace(e.Stderr))
}

// Exec is the production Runner backed by os/exec.
type Exec struct {
	// Printer, if set, is called with the rendered argv before each run
	// (wired to --print-commands / --dry-run).
	Printer func(argv []string)
	// DryRun, when true, prints and skips execution (returns an empty Result).
	DryRun bool
}

// Run implements Runner.
func (e *Exec) Run(ctx context.Context, opts Opts, argv ...string) (Result, error) {
	if len(argv) == 0 {
		return Result{}, errors.New("run: empty argv")
	}
	// Read-only queries execute even under DryRun (they have no side effects and
	// their output is needed to render the mutation that would follow).
	if e.DryRun && !opts.ReadOnly {
		if e.Printer != nil {
			e.Printer(argv)
		}
		return Result{}, nil
	}
	if e.Printer != nil {
		e.Printer(argv)
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = opts.Dir
	if len(opts.Env) > 0 {
		cmd.Env = append(os.Environ(), opts.Env...)
	}
	if opts.Stdin != "" {
		cmd.Stdin = strings.NewReader(opts.Stdin)
	}
	if opts.Interactive {
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		err := cmd.Run()
		return interactiveResult(err, argv)
	}
	var out, errb bytes.Buffer
	cmd.Stderr = &errb
	cmd.Stdout = &out
	var sink *os.File
	if opts.StdoutFile != "" {
		f, ferr := os.Create(opts.StdoutFile)
		if ferr != nil {
			return Result{}, fmt.Errorf("run %s: stdout file: %w", argv[0], ferr)
		}
		sink = f
		cmd.Stdout = f
	}
	err := cmd.Run()
	if sink != nil {
		// Closed before the error is reported, and its failure preferred to a
		// clean exit: a full disk shows up here and nowhere else, and a
		// truncated tar would otherwise be built into a disk that mounts and is
		// quietly missing layers.
		if cerr := sink.Close(); cerr != nil && err == nil {
			return Result{Stderr: errb.String()}, fmt.Errorf("run %s: stdout file: %w", argv[0], cerr)
		}
	}
	res := Result{Stdout: out.String(), Stderr: errb.String()}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			res.ExitCode = ee.ExitCode()
			return res, &ExitError{
				Argv:     append([]string(nil), argv...),
				ExitCode: res.ExitCode,
				Stderr:   res.Stderr,
			}
		}
		return res, fmt.Errorf("run %s: %w", argv[0], err)
	}
	return res, nil
}

func interactiveResult(err error, argv []string) (Result, error) {
	if err == nil {
		return Result{}, nil
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return Result{ExitCode: ee.ExitCode()}, &ExitError{
			Argv:     append([]string(nil), argv...),
			ExitCode: ee.ExitCode(),
		}
	}
	return Result{}, fmt.Errorf("run %s: %w", argv[0], err)
}

// Output is a convenience wrapper returning trimmed stdout for a successful
// command, discarding a non-zero exit as empty (a best-effort read: a failed
// command yields "" rather than an error).
func Output(ctx context.Context, r Runner, argv ...string) string {
	res, err := r.Run(ctx, Opts{ReadOnly: true}, argv...)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(res.Stdout)
}
