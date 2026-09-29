//go:build !linux

package engine

import "errors"

// io_uring is Linux-only, and so is the firecracker engine — this file exists so
// the package still builds on the hosts that only ever use Podman.
func hostIOUring() (release, disabled string, probe error) {
	return "", "", errors.New("not Linux")
}
