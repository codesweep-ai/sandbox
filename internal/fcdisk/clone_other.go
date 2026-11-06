//go:build !linux

package fcdisk

import (
	"errors"
	"os"
)

// cloneFile is Linux-only, as the firecracker engine is, so a full copy
// stands in everywhere else.
var cloneFile = func(_, _ *os.File) error { return errors.ErrUnsupported }
