// Command cs-sandbox manages named dev sandboxes (user/agent types) as rootless
// Firecracker microVMs or podman containers.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/codesweep-ai/sandbox/internal/cli"
)

func main() {
	if err := cli.NewRootCmd().Execute(); err != nil {
		var status *cli.ExitStatus
		switch {
		case errors.As(err, &status):
			// A command run inside a sandbox exited non-zero. Hand that status
			// back unchanged and say nothing: it is the command's own result,
			// and its output already reached the terminal.
			os.Exit(status.Code)
		case errors.Is(err, cli.ErrChecksFailed):
			// doctor already printed its report; exit nonzero without re-printing.
		default:
			fmt.Fprintln(os.Stderr, "cs-sandbox: "+err.Error())
		}
		os.Exit(1)
	}
}
