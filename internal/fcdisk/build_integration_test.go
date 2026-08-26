//go:build integration

// Integration test for the guest-kernel package resolution. It runs the real
// `dnf install` (download-only) that the kernel build performs, inside the built
// sandbox image, and asserts the pinned kernel-core NVR resolves. This is the
// regression guard for the two bugs behind the "dnf could not install
// kernel-core-<NVR>" failure: a DefaultKVerPin that aged out of the Fedora repos,
// and a bare (non-arch-qualified) spec that dnf5 resolves inconsistently.
//
//	go test -tags integration -run TestPinnedKernel ./internal/fcdisk/ -v
//
// Skips gracefully when podman or the sandbox image is unavailable.
package fcdisk

import (
	"context"
	"os"
	"os/exec"
	"testing"

	"github.com/codesweep-ai/sandbox/internal/run"
)

// image is the sandbox image the live tests run against. There is no default
// any more: the name carries the version of the cs-sandbox that built it, and a
// test binary carries no version to derive it from. `make test-integration` and
// `make test-smoke` both set the variable from the binary; a bare `go test`
// against these tags has to say which image it means.
func image(t *testing.T) string {
	t.Helper()
	v := os.Getenv("CS_SANDBOX_IMAGE")
	if v == "" {
		t.Skip("set CS_SANDBOX_IMAGE to the image to run against, or use `make test-integration`")
	}
	return v
}

func TestPinnedKernelResolvesLive(t *testing.T) {
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman not on PATH")
	}
	ctx := context.Background()
	r := &run.Exec{}
	if _, err := r.Run(ctx, run.Opts{ReadOnly: true}, "podman", "info"); err != nil {
		t.Skipf("podman unavailable: %v", err)
	}
	img := image(t)
	if _, err := r.Run(ctx, run.Opts{ReadOnly: true}, "podman", "image", "exists", img); err != nil {
		t.Skipf("image %s not built (run: cs-sandbox build) — %v", img, err)
	}

	// Exactly what buildFedoraBootArtifacts does: arch-qualified NEVRA,
	// download-only so we resolve+fetch without unpacking a kernel. A miss here
	// means the pin is stale or the spec form regressed.
	script := `set -e
FC_SPEC="kernel-core-` + DefaultKVerPin + `.$(uname -m)"
dnf install -y --setopt=install_weak_deps=False --downloadonly "$FC_SPEC" \
  gcc glibc-static cpio zstd xz gzip binutils file >/dev/null`
	if _, err := r.Run(ctx, run.Opts{}, "podman", "run", "--rm", "--user", "0:0",
		"--entrypoint", "/bin/bash", img, "-c", script); err != nil {
		t.Fatalf("pinned kernel-core-%s did not resolve in %s: %v\n"+
			"(bump DefaultKVerPin to a kernel-core NVR still in the F44 repos)", DefaultKVerPin, img, err)
	}
}
