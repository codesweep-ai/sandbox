//go:build integration

// Integration test for the koji provenance of the guest kernel. It runs the real
// build against a `koji:` pin — download, digest check, unpack, depmod, unwrap,
// initramfs — and asserts what a microVM actually needs out of it.
//
// The unit tests can only check the shell this emits. This is what catches what
// only appears once an RPM is unpacked for real: a kernel whose modules live in a
// package we did not fetch, or the missing depmod indexes that let a guest boot,
// print its ready marker, and then panic on its first modprobe.
//
//	go test -tags integration -run TestKojiKernel ./internal/fcdisk/ -v
//
// Skips gracefully when podman, the sandbox image, or a committed digest for this
// host's arch is unavailable.
package fcdisk

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codesweep-ai/sandbox/internal/run"
)

// pinnedKojiNVR is the NVR this test builds: DefaultKVerPin itself, so the test
// exercises what a user actually gets rather than a copy that can drift from it.
// It deliberately does not pick an arbitrary kojiDigests entry — map order is not
// deterministic, and the table now holds more than one NVR per arch.
func pinnedKojiNVR(t *testing.T) string {
	t.Helper()
	pin := parseKernelPin(DefaultKVerPin)
	if !pin.Koji {
		t.Skipf("DefaultKVerPin = %q is not a koji pin", DefaultKVerPin)
	}
	arch, err := fcArch()
	if err != nil {
		t.Skipf("unsupported arch: %v", err)
	}
	if kojiSums(pin, arch) == "" {
		t.Skipf("kojiDigests has no entry for %s on %s", pin.NVR, arch)
	}
	return pin.NVR
}

func TestKojiKernelBuildLive(t *testing.T) {
	ctx := context.Background()
	r := &run.Exec{}
	if _, err := r.Run(ctx, run.Opts{ReadOnly: true}, "podman", "info"); err != nil {
		t.Skipf("podman unavailable: %v", err)
	}
	img := image(t)
	if _, err := r.Run(ctx, run.Opts{ReadOnly: true}, "podman", "image", "exists", img); err != nil {
		t.Skipf("image %s not built (run: cs-sandbox build) — %v", img, err)
	}
	nvr := pinnedKojiNVR(t)

	c := Cache{Dir: t.TempDir(), Progress: func(s string) { t.Log(s) }}
	if err := c.ensureKernel(ctx, r, BuildConfig{
		Image:        img,
		Kernel:       "fedora",
		KVerPin:      kojiPinPrefix + nvr,
		InitramfsSrc: filepath.Join("..", "..", "image", "guest", "initramfs-init.c"),
	}); err != nil {
		t.Fatalf("koji kernel build for %s: %v", nvr, err)
	}

	// The pin is the whole point: a build that quietly took whatever the repos
	// were serving would satisfy every other check below.
	if got := strings.TrimSpace(readTestFile(t, c.stampPath("kver"))); !strings.HasPrefix(got, nvr) {
		t.Errorf("kver = %q, want the pinned %s", got, nvr)
	}

	// R119: firecracker boots an uncompressed ELF, never the packaged bzImage.
	out, err := exec.Command("file", "-b", c.Kernel()).Output()
	if err != nil {
		t.Fatalf("file(1) on the kernel: %v", err)
	}
	if !strings.Contains(string(out), "ELF") {
		t.Errorf("vmlinux.elf did not unwrap to an ELF: %s", out)
	}

	names := tarNames(t, filepath.Join(c.Dir, "modules.tar"))
	// Without the depmod indexes the guest boots, reaches FC-VM-READY, and then
	// panics when socat — PID 1 — finds no /dev/vsock to open. rpm2cpio runs no
	// scriptlets, so nothing but our own depmod call puts these here.
	if !containsSubstring(names, "modules.dep.bin") {
		t.Error("modules.tar has no modules.dep.bin — modprobe would resolve nothing in the guest")
	}
	// kernel-core ships vmlinuz and no modules. This one comes from the second
	// package, which dnf used to pull in by dependency and koji does not.
	if !containsSubstring(names, "virtio_mmio.ko") {
		t.Error("modules.tar has no virtio_mmio.ko — the initramfs cannot mount root")
	}

	// Only the unverified path leaves this behind, so its absence is how we know
	// the download was checked against the committed digest (R120c) and did not
	// silently fall back — an arch or naming slip in the lookup would do exactly
	// that while still producing a working kernel.
	if _, err := os.Stat(c.stampPath(kojiSumsFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s present: a pin with a committed digest took the unverified path", kojiSumsFile)
	}

	for _, f := range []string{"vmlinux.elf", "initrd.img", "modules.tar"} {
		fi, err := os.Stat(filepath.Join(c.Dir, f))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Size() == 0 {
			t.Errorf("%s is empty", f)
		}
		t.Logf("%-12s %d bytes", f, fi.Size())
	}
}

func readTestFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// tarNames lists the entries of an uncompressed tar.
func tarNames(t *testing.T, p string) []string {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var names []string
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return names
		}
		if err != nil {
			t.Fatalf("reading %s: %v", p, err)
		}
		names = append(names, h.Name)
	}
}

func containsSubstring(names []string, want string) bool {
	for _, n := range names {
		if strings.Contains(n, want) {
			return true
		}
	}
	return false
}
