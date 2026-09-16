//go:build integration

// Integration test for the guest-kernel package resolution: it asserts that
// DefaultKVerPin is still fetchable by the provenance it names.
//
// For a bare NVR that means the real `dnf install` (download-only) the kernel
// build performs, inside the built sandbox image — the regression guard for the
// two bugs behind the "dnf could not install kernel-core-<NVR>" failure: a pin
// that aged out of the Fedora repos, and a bare (non-arch-qualified) spec that
// dnf5 resolves inconsistently.
//
// For a `koji:` pin the repos are irrelevant by design, and what can rot instead
// is the NVR itself: a bump to a build that does not exist, or whose release
// number is a typo, 404s. That needs no container, so it is checked directly.
//
//	go test -tags integration -run TestPinnedKernel ./internal/fcdisk/ -v
//
// Skips gracefully when podman or the sandbox image is unavailable.
package fcdisk

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"testing"
	"time"

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
	pin := parseKernelPin(DefaultKVerPin)
	if pin.Koji {
		testKojiPinReachable(t, pin)
		return
	}
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
FC_SPEC="kernel-core-` + pin.NVR + `.$(uname -m)"
dnf install -y --setopt=install_weak_deps=False --downloadonly "$FC_SPEC" \
  gcc glibc-static cpio zstd xz gzip binutils file >/dev/null`
	if _, err := r.Run(ctx, run.Opts{}, "podman", "run", "--rm", "--user", "0:0",
		"--entrypoint", "/bin/bash", img, "-c", script); err != nil {
		t.Fatalf("pinned kernel-core-%s did not resolve in %s: %v\n"+
			"(bump DefaultKVerPin to a kernel-core NVR still in the F44 repos, or pin it as koji:<nvr>)", pin.NVR, img, err)
	}
}

// testKojiPinReachable checks that every RPM the koji pin names is actually
// served. A committed digest proves a file's identity, not its existence: a pin
// bumped to an NVR koji never built fails with a 404 minutes into a build, in a
// container, behind a curl error.
func testKojiPinReachable(t *testing.T, pin kernelPin) {
	t.Helper()
	version, release, ok := pin.versionRelease()
	if !ok {
		t.Fatalf("DefaultKVerPin = %q is not koji:<version>-<release>", DefaultKVerPin)
	}
	arch, err := fcArch()
	if err != nil {
		t.Skipf("unsupported arch: %v", err)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	for _, p := range kernelRPMs {
		url := fmt.Sprintf("%s/%s/%s/%s/%s-%s.%s.rpm", kojiPkgBase, version, release, arch, p, pin.NVR, arch)
		req, err := http.NewRequest(http.MethodHead, url, http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Skipf("koji unreachable (offline?): %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: HTTP %d — DefaultKVerPin names a build koji does not serve", url, resp.StatusCode)
		}
	}
}
