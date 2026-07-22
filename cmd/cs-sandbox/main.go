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
		// doctor already printed its report; exit nonzero without re-printing.
		if !errors.Is(err, cli.ErrChecksFailed) {
			fmt.Fprintln(os.Stderr, "cs-sandbox: "+err.Error())
		}
		os.Exit(1)
	}
}
