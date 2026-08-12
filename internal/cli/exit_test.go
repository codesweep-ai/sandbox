package cli

import (
	"errors"
	"fmt"
	"testing"

	"github.com/codesweep-ai/sandbox/internal/run"
)

// A command that ran inside the sandbox and failed carries its status out, so
// `exec box test -f /missing` answers 1 instead of being reported as a
// cs-sandbox error.
func TestSandboxedExitCarriesTheCommandsStatus(t *testing.T) {
	for _, tc := range []struct {
		name string
		exit int
		want int
	}{
		{"plain failure", 1, 1},
		{"specific status", 7, 7},
		{"command not found", 127, 127},
		// A signal-killed child reports -1, which os.Exit would render as 255.
		{"killed by a signal", -1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := sandboxedExit(&run.ExitError{Argv: []string{"ssh", "box"}, ExitCode: tc.exit})
			var status *ExitStatus
			if !errors.As(err, &status) {
				t.Fatalf("got %v, want an *ExitStatus", err)
			}
			if status.Code != tc.want {
				t.Errorf("exit code = %d, want %d", status.Code, tc.want)
			}
		})
	}
}

// Only a non-zero exit is the command's own answer. A sandbox that does not
// exist, or a runner that could not start the process, is a cs-sandbox failure
// and has to stay printable.
func TestSandboxedExitLeavesRealFailuresAlone(t *testing.T) {
	sentinel := errors.New("no such sandbox: box")
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"nil", nil},
		{"unrelated failure", sentinel},
		{"wrapped unrelated failure", fmt.Errorf("exec: %w", sentinel)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := sandboxedExit(tc.err)
			if !errors.Is(got, tc.err) {
				t.Fatalf("got %v, want it to pass through as %v", got, tc.err)
			}
			var status *ExitStatus
			if errors.As(got, &status) {
				t.Errorf("got an *ExitStatus (%v); it would be silently swallowed", got)
			}
		})
	}
}

// The conversion has to survive wrapping, since the engines return the runner's
// error through their own call stack.
func TestSandboxedExitSeesAWrappedExitError(t *testing.T) {
	err := sandboxedExit(fmt.Errorf("exec: %w", &run.ExitError{ExitCode: 3}))
	var status *ExitStatus
	if !errors.As(err, &status) {
		t.Fatalf("got %v, want an *ExitStatus", err)
	}
	if status.Code != 3 {
		t.Errorf("exit code = %d, want 3", status.Code)
	}
}
