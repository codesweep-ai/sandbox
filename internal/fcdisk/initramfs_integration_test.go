//go:build integration

// Integration test for the initramfs assembly. It runs the real
// initramfsBuildScript — the same constant buildFedoraBootArtifacts feeds to the
// build container — against the real guest init source, and asserts the archive
// it produces contains what a microVM needs to reach its root filesystem.
//
// The unit tests can only check the rebuild *decision*; this is what catches a
// broken script, a compiler error in initramfs-init.c, or a module that moved.
//
//	go test -tags integration -run TestInitramfsBuild ./internal/fcdisk/ -v
//
// Skips gracefully when podman or the sandbox image is unavailable.
package fcdisk

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codesweep-ai/sandbox/internal/run"
)

func TestInitramfsBuild(t *testing.T) {
	ctx := context.Background()
	r := &run.Exec{}
	if _, err := r.Run(ctx, run.Opts{ReadOnly: true}, "podman", "info"); err != nil {
		t.Skipf("podman unavailable: %v", err)
	}
	img := image()
	if _, err := r.Run(ctx, run.Opts{ReadOnly: true}, "podman", "image", "exists", img); err != nil {
		t.Skipf("image %s not built (run: cs-sandbox build) — %v", img, err)
	}

	src, err := os.ReadFile(filepath.Join("..", "..", "image", "guest", "initramfs-init.c"))
	if err != nil {
		t.Fatalf("reading initramfs source: %v", err)
	}

	// Mirror buildFedoraBootArtifacts: install the toolchain, pick the kernel
	// whose modules are in the image, then run the assembly script verbatim.
	script := `set -e
dnf install -y --setopt=install_weak_deps=False kernel-core gcc glibc-static cpio zstd xz gzip binutils file >/dev/null
KVER=$(ls -1 /lib/modules | head -1)
mkdir -p /artifacts
` + initramfsBuildScript + `
mkdir -p /probe && cd /probe && gzip -dc /artifacts/initrd.img | cpio -idm --quiet
echo "PROBE_INIT=$(file -b init 2>/dev/null || echo unknown)"
echo "PROBE_ENTRIES=$(find . -type f | sort | tr '\n' ' ')"
echo "PROBE_SIZE=$(stat -c %s /artifacts/initrd.img)"`

	res, err := r.Run(ctx, run.Opts{Env: []string{"FC_INITRAMFS_C=" + string(src)}},
		"podman", "run", "--rm", "--user", "0:0", "-e", "FC_INITRAMFS_C="+string(src),
		"--entrypoint", "/bin/bash", img, "-c", script)
	out := res.Stdout + res.Stderr
	if err != nil {
		t.Fatalf("initramfs build failed: %v\n%s", err, out)
	}
	t.Log(res.Stdout)

	// The static init and the one module that makes /dev/vda exist. Without
	// either, a microVM cannot reach its root filesystem at all.
	for _, want := range []string{"./init", "./modules/10-virtio_mmio.ko"} {
		if !strings.Contains(out, want) {
			t.Errorf("initramfs is missing %s\n%s", want, out)
		}
	}
	// Statically linked: the initramfs carries no libc for it to load. Assert on
	// the probe line specifically — "static" appears in package names too, and a
	// bare substring check over the whole log would pass vacuously.
	var probe string
	for line := range strings.SplitSeq(out, "\n") {
		if strings.HasPrefix(line, "PROBE_INIT=") {
			probe = line
		}
	}
	if !strings.Contains(probe, "statically linked") {
		t.Errorf("init is not statically linked: %q", probe)
	}
}
