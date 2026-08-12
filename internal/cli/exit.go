package cli

import (
	"errors"
	"fmt"

	"github.com/codesweep-ai/sandbox/internal/run"
)

// ExitStatus reports that a command the user asked to run INSIDE a sandbox
// exited non-zero. That status is the command's own answer, not a cs-sandbox
// failure: `exec box test -f /missing` returning 1 is the result the caller
// asked for, and scripts branch on it. main maps this to the same exit code
// without printing anything, the way ssh and podman exec do.
//
// Nothing is lost by staying quiet: in-sandbox commands run interactively, so
// whatever the command wrote to stderr already reached the terminal. Wrapping
// it in "cs-sandbox: command failed (exit 1): ssh -i /home/... -o HostKeyAlias
// ..." only buries the real message under the argv we happened to build.
type ExitStatus struct{ Code int }

func (e *ExitStatus) Error() string {
	return fmt.Sprintf("sandboxed command exited %d", e.Code)
}

// sandboxedExit converts a non-zero exit from an in-sandbox command into an
// ExitStatus so its status reaches the caller. Everything else passes through
// untouched and is reported the usual way — a sandbox that does not exist, or a
// runner that could not start the process at all, is a cs-sandbox failure and
// the user needs to read about it.
func sandboxedExit(err error) error {
	var ee *run.ExitError
	if !errors.As(err, &ee) {
		return err
	}
	code := ee.ExitCode
	// A child killed by a signal reports -1, which is not a usable exit status;
	// report a plain failure instead of letting os.Exit render it as 255.
	if code <= 0 {
		code = 1
	}
	return &ExitStatus{Code: code}
}
