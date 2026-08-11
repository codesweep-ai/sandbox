package fcdisk

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codesweep-ai/sandbox/internal/lock"
	"github.com/codesweep-ai/sandbox/internal/run"
)

// TestDefaulted fills empty fields from the defaults and leaves set fields alone.
func TestDefaulted(t *testing.T) {
	got := BuildConfig{}.Defaulted()
	if got.Kernel != "fedora" {
		t.Errorf("Kernel = %q, want fedora", got.Kernel)
	}
	if got.KVerPin != DefaultKVerPin {
		t.Errorf("KVerPin = %q, want default %q", got.KVerPin, DefaultKVerPin)
	}
	if got.RootfsGB != 14 {
		t.Errorf("RootfsGB = %d, want 14", got.RootfsGB)
	}
	if got.FCVersion != DefaultFCVersion {
		t.Errorf("FCVersion = %q, want default %q", got.FCVersion, DefaultFCVersion)
	}

	// Explicit values win over the defaults.
	set := BuildConfig{Kernel: "fedora", KVerPin: "9.9.9-1.fc44", RootfsGB: 20, FCVersion: "v2"}.Defaulted()
	if set.KVerPin != "9.9.9-1.fc44" || set.RootfsGB != 20 || set.FCVersion != "v2" {
		t.Errorf("Defaulted overwrote explicit fields: %+v", set)
	}
}

// TestDefaultedHostKernelNoPin: host mode never gets a fedora KVerPin default —
// the pin only applies to fedora-built kernels.
func TestDefaultedHostKernelNoPin(t *testing.T) {
	got := BuildConfig{Kernel: "host"}.Defaulted()
	if got.KVerPin != "" {
		t.Errorf("host-kernel KVerPin = %q, want empty", got.KVerPin)
	}
}

// TestDefaultKVerPinIsGA guards against regressing to a churning updates-repo NVR:
// the default must be a concrete fc44 GA kernel-core release string.
func TestDefaultKVerPinFormat(t *testing.T) {
	if DefaultKVerPin == "" {
		t.Fatal("DefaultKVerPin is empty")
	}
	if filepath.Ext(DefaultKVerPin) != ".fc44" {
		t.Errorf("DefaultKVerPin = %q, want a *.fc44 NVR", DefaultKVerPin)
	}
}

// TestVerifyArtifacts errors on the first missing artifact and passes when the
// full boot set is present.
func TestVerifyArtifacts(t *testing.T) {
	dir := t.TempDir()
	c := Cache{Dir: dir}

	// Nothing present yet -> error naming the firecracker binary (checked first).
	if err := c.VerifyArtifacts(); err == nil {
		t.Fatal("VerifyArtifacts on empty cache = nil, want error")
	}

	// Lay down the four artifacts a microVM boots from. The base rootfs has to
	// carry an ext4 superblock — a bare file is the placeholder case, covered by
	// TestVerifyArtifactsRejectsPlaceholderRootfs.
	writeFile(t, c.FirecrackerBin())
	writeFile(t, c.Kernel())
	writeFile(t, c.Initrd())
	writeExt4(t, c.BaseRootfs())
	if err := c.VerifyArtifacts(); err != nil {
		t.Fatalf("VerifyArtifacts with all artifacts = %v, want nil", err)
	}

	// Remove one -> error again.
	if err := os.Remove(c.Initrd()); err != nil {
		t.Fatal(err)
	}
	if err := c.VerifyArtifacts(); err == nil {
		t.Error("VerifyArtifacts with missing initrd = nil, want error")
	}
}

// TestIsExt4 tells a real filesystem from the truncate placeholder an
// interrupted build leaves behind — the state that made a corrupt cache look
// fresh, since both are 14 GiB files that exist.
func TestIsExt4(t *testing.T) {
	dir := t.TempDir()

	placeholder := filepath.Join(dir, "placeholder.ext4")
	if err := os.Truncate(placeholder, 0); err != nil {
		if err := os.WriteFile(placeholder, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Truncate(placeholder, 1<<30); err != nil {
		t.Fatal(err)
	}
	if isExt4(placeholder) {
		t.Error("isExt4(sparse placeholder) = true, want false")
	}

	img := filepath.Join(dir, "real.ext4")
	data := make([]byte, 0x440)
	data[0x438], data[0x439] = 0x53, 0xEF // ext4 magic, little-endian
	if err := os.WriteFile(img, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if !isExt4(img) {
		t.Error("isExt4(image with the ext4 magic) = false, want true")
	}

	if isExt4(filepath.Join(dir, "absent.ext4")) {
		t.Error("isExt4(missing file) = true, want false")
	}
}

// TestVerifyArtifactsRejectsPlaceholderRootfs: the corruption that shipped a
// garbage disk to a microVM must be caught here, not at boot.
func TestVerifyArtifactsRejectsPlaceholderRootfs(t *testing.T) {
	c := Cache{Dir: t.TempDir()}
	writeFile(t, c.FirecrackerBin())
	writeFile(t, c.Kernel())
	writeFile(t, c.Initrd())
	writeFile(t, c.BaseRootfs()) // present, but not a filesystem

	err := c.VerifyArtifacts()
	if err == nil {
		t.Fatal("VerifyArtifacts with a placeholder base rootfs = nil, want error")
	}
	if !strings.Contains(err.Error(), "not a filesystem") {
		t.Errorf("error = %v, want it to name the placeholder", err)
	}
}

// TestEnsureBaseRootfsRejectsFreshStampOverPlaceholder: even a stamp that
// matches must not let a placeholder through — this is exactly the state a
// racing build and create left in a real cache.
func TestEnsureBaseRootfsRejectsFreshStampOverPlaceholder(t *testing.T) {
	c := Cache{Dir: t.TempDir()}
	writeFile(t, c.BaseRootfs())
	// Stamp for a zero BuildConfig: no image, no kver, no init hash.
	if err := c.writeStamp("base-rootfs.stamp", "||fedora|"); err != nil {
		t.Fatal(err)
	}
	// A matching stamp over a real filesystem is reused (nothing to do, no error).
	// Over the placeholder it must instead try to rebuild — and with no image
	// configured that surfaces as the actionable "cannot build" error.
	err := c.ensureBaseRootfs(context.Background(), run.NewFake(), BuildConfig{Kernel: "fedora"})
	if err == nil {
		t.Fatal("ensureBaseRootfs reused a placeholder because the stamp matched")
	}
	if !strings.Contains(err.Error(), "cannot build") {
		t.Errorf("error = %v, want the missing-build-inputs error", err)
	}
}

// TestStampRoundTrip: writeStamp then readStamp returns the trimmed value; a
// missing stamp reads as "".
func TestStampRoundTrip(t *testing.T) {
	c := Cache{Dir: t.TempDir()}
	if got := c.readStamp("nope"); got != "" {
		t.Errorf("readStamp(missing) = %q, want empty", got)
	}
	if err := c.writeStamp("kver-pin", "7.1.4-200.fc44"); err != nil {
		t.Fatal(err)
	}
	if got := c.readStamp("kver-pin"); got != "7.1.4-200.fc44" {
		t.Errorf("readStamp = %q, want 7.1.4-200.fc44 (trimmed)", got)
	}
}

// TestKernelRebuildReason is the core rebuild-decision logic: a fully-populated,
// mode+pin-matching cache is reused (""), and every staleness condition — missing
// artifact, wrong mode, and (the regression we fixed) a changed pinned NVR —
// forces a rebuild.
func TestKernelRebuildReason(t *testing.T) {
	const pin = "6.19.10-300.fc44"
	src := filepath.Join(t.TempDir(), "initramfs-init.c")
	if err := os.WriteFile(src, []byte("int main(void){return 0;}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bc := BuildConfig{Kernel: "fedora", KVerPin: pin, InitramfsSrc: src}

	fresh := func(t *testing.T) Cache {
		c := Cache{Dir: t.TempDir()}
		writeFile(t, c.Kernel())
		writeFile(t, c.Initrd())
		writeFile(t, filepath.Join(c.Dir, "modules.tar"))
		for k, v := range map[string]string{
			"kver":          pin + ".x86_64",
			"kernel-mode":   "fedora",
			"kver-pin":      pin,
			"initramfs-src": initramfsStamp(bc),
		} {
			if err := c.writeStamp(k, v); err != nil {
				t.Fatal(err)
			}
		}
		return c
	}

	// Fresh cache -> reuse.
	if r := fresh(t).kernelRebuildReason(bc); r != "" {
		t.Errorf("fresh cache reason = %q, want reuse", r)
	}

	// Pin change -> rebuild (the DefaultKVerPin/CS_SANDBOX_FC_KVER regression).
	c := fresh(t)
	if err := c.writeStamp("kver-pin", "7.1.4-202.fc44"); err != nil {
		t.Fatal(err)
	}
	if r := c.kernelRebuildReason(bc); r == "" {
		t.Error("changed pin reason = reuse, want rebuild")
	}

	// Missing artifact -> rebuild.
	c = fresh(t)
	if err := os.Remove(c.Kernel()); err != nil {
		t.Fatal(err)
	}
	if r := c.kernelRebuildReason(bc); r == "" {
		t.Error("missing vmlinux reason = reuse, want rebuild")
	}

	// Wrong kernel-mode stamp -> rebuild.
	c = fresh(t)
	if err := c.writeStamp("kernel-mode", "host"); err != nil {
		t.Fatal(err)
	}
	if r := c.kernelRebuildReason(bc); r == "" {
		t.Error("non-fedora mode reason = reuse, want rebuild")
	}

	// Editing the initramfs source invalidates the cached initrd.img, even
	// though every other input still matches.
	c = fresh(t)
	if r := c.kernelRebuildReason(bc); r != "" {
		t.Fatalf("precondition: fresh cache reason = %q, want reuse", r)
	}
	if err := os.WriteFile(bc.InitramfsSrc, []byte("int main(void){return 1;}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := c.kernelRebuildReason(bc); r == "" {
		t.Error("edited initramfs source = reuse, want rebuild")
	}
}

// TestEnsureKernelReusesFreshCache: when the cache is fresh, ensureKernel must
// return without shelling out (no podman build). The Fake records zero calls.
func TestEnsureKernelReusesFreshCache(t *testing.T) {
	const pin = "6.19.10-300.fc44"
	src := filepath.Join(t.TempDir(), "initramfs-init.c")
	if err := os.WriteFile(src, []byte("int main(void){return 0;}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bc := BuildConfig{Kernel: "fedora", KVerPin: pin, Image: "img", InitramfsSrc: src}
	c := Cache{Dir: t.TempDir()}
	writeFile(t, c.Kernel())
	writeFile(t, c.Initrd())
	writeFile(t, filepath.Join(c.Dir, "modules.tar"))
	for k, v := range map[string]string{
		"kver":          pin + ".x86_64",
		"kernel-mode":   "fedora",
		"kver-pin":      pin,
		"initramfs-src": initramfsStamp(bc),
	} {
		if err := c.writeStamp(k, v); err != nil {
			t.Fatal(err)
		}
	}
	f := run.NewFake()
	if err := c.ensureKernel(context.Background(), f, bc); err != nil {
		t.Fatalf("ensureKernel(fresh) = %v, want nil", err)
	}
	if len(f.Calls) != 0 {
		t.Errorf("ensureKernel(fresh) shelled out %d times: %s", len(f.Calls), f)
	}
}

// TestEnsureKernelHostModeMissing: host mode with no cached artifacts returns the
// actionable "unsupported" error rather than trying to build.
func TestEnsureKernelHostModeMissing(t *testing.T) {
	c := Cache{Dir: t.TempDir()}
	f := run.NewFake()
	err := c.ensureKernel(context.Background(), f, BuildConfig{Kernel: "host"})
	if err == nil {
		t.Fatal("host-mode ensureKernel with no artifacts = nil, want error")
	}
	if len(f.Calls) != 0 {
		t.Errorf("host-mode ensureKernel shelled out: %s", f)
	}
}

// TestFCDigestsPinned: every architecture the downloader supports must have a
// committed digest for the pinned release, or that arch silently degrades to
// trusting the checksum served next to the tarball.
func TestFCDigestsPinned(t *testing.T) {
	for _, arch := range []string{"x86_64", "aarch64"} {
		d, ok := fcDigests[arch]
		if !ok {
			t.Errorf("no pinned digest for %s (firecracker %s)", arch, DefaultFCVersion)
			continue
		}
		if len(d) != 64 {
			t.Errorf("fcDigests[%s] = %q, want a 64-char sha256", arch, d)
		}
		if _, err := hex.DecodeString(d); err != nil {
			t.Errorf("fcDigests[%s] is not hex: %v", arch, err)
		}
	}
}

// TestFCRefreshReason is the decision that makes a version bump take effect: a
// cache holding the pinned release is reused, while a missing binary, an
// unstamped one (predating version tracking), and a stale stamp each force a
// re-download.
func TestFCRefreshReason(t *testing.T) {
	bc := BuildConfig{}.Defaulted()

	fresh := func(t *testing.T) Cache {
		c := Cache{Dir: t.TempDir()}
		writeFile(t, c.FirecrackerBin())
		if err := c.writeStamp("fc-version", bc.FCVersion); err != nil {
			t.Fatal(err)
		}
		return c
	}

	if r := fresh(t).fcRefreshReason(bc); r != "" {
		t.Errorf("cache at the pinned version reason = %q, want reuse", r)
	}

	// Version bump (DefaultFCVersion / CS_SANDBOX_FC_VERSION) -> re-download.
	c := fresh(t)
	if err := c.writeStamp("fc-version", "v0.0.1"); err != nil {
		t.Fatal(err)
	}
	if r := c.fcRefreshReason(bc); r == "" {
		t.Error("changed pinned version reason = reuse, want re-download")
	}

	// Binary from before version tracking (no stamp) -> re-download, which also
	// re-verifies it against the pinned digest.
	c = fresh(t)
	if err := os.Remove(c.stampPath("fc-version")); err != nil {
		t.Fatal(err)
	}
	if r := c.fcRefreshReason(bc); r == "" {
		t.Error("unstamped binary reason = reuse, want re-download")
	}

	// No binary at all -> re-download.
	c = fresh(t)
	if err := os.Remove(c.FirecrackerBin()); err != nil {
		t.Fatal(err)
	}
	if r := c.fcRefreshReason(bc); r == "" {
		t.Error("missing binary reason = reuse, want re-download")
	}
}

// TestEnsureFirecrackerBinReusesFreshCache: a cache already at the pinned
// version must not curl anything.
func TestEnsureFirecrackerBinReusesFreshCache(t *testing.T) {
	bc := BuildConfig{}.Defaulted()
	c := Cache{Dir: t.TempDir()}
	writeFile(t, c.FirecrackerBin())
	if err := c.writeStamp("fc-version", bc.FCVersion); err != nil {
		t.Fatal(err)
	}
	f := run.NewFake()
	if err := c.ensureFirecrackerBin(context.Background(), f, bc); err != nil {
		t.Fatalf("ensureFirecrackerBin(fresh) = %v, want nil", err)
	}
	if len(f.Calls) != 0 {
		t.Errorf("ensureFirecrackerBin(fresh) shelled out %d times: %s", len(f.Calls), f)
	}
}

// TestFCWantDigestPinned: the pinned release is verified against the in-repo
// digest without asking the network for a checksum at all.
func TestFCWantDigestPinned(t *testing.T) {
	arch, err := fcArch()
	if err != nil {
		t.Skipf("unsupported test architecture: %v", err)
	}
	c := Cache{Dir: t.TempDir()}
	f := run.NewFake()
	got, pinned, err := c.fcWantDigest(context.Background(), f, BuildConfig{}.Defaulted(), arch, "base", "t.tgz")
	if err != nil {
		t.Fatalf("fcWantDigest = %v, want nil", err)
	}
	if !pinned || got != fcDigests[arch] {
		t.Errorf("fcWantDigest = (%q, %v), want the pinned %q", got, pinned, fcDigests[arch])
	}
	if len(f.Calls) != 0 {
		t.Errorf("pinned digest should need no network: %s", f)
	}
}

// TestFCWantDigestOverride: an overridden CS_SANDBOX_FC_VERSION has no committed
// digest, so it falls back to the published checksum — parsing the first field —
// and reports pinned=false so the mismatch message names the weaker source.
func TestFCWantDigestOverride(t *testing.T) {
	c := Cache{Dir: t.TempDir()}
	const sum = "aa04e26952d4e158085778c6230a0b383d2619c319182e27eaa9d61a212e92d6"
	f := run.NewFake().OnStdout("sha256.txt", sum+"  firecracker-v9.9.9-x86_64.tgz\n")
	got, pinned, err := c.fcWantDigest(context.Background(), f, BuildConfig{FCVersion: "v9.9.9"}.Defaulted(), "x86_64", "base", "t.tgz")
	if err != nil {
		t.Fatalf("fcWantDigest = %v, want nil", err)
	}
	if got != sum || pinned {
		t.Errorf("fcWantDigest = (%q, %v), want (%q, false)", got, pinned, sum)
	}
}

// TestFCWantDigestFetchFails: an unreachable checksum must be a clear error, not
// an empty "want" that surfaces later as a bogus mismatch.
func TestFCWantDigestFetchFails(t *testing.T) {
	c := Cache{Dir: t.TempDir()}
	f := run.NewFake().On("sha256.txt", run.Result{ExitCode: 22}, errors.New("curl: (22) 404"))
	_, _, err := c.fcWantDigest(context.Background(), f, BuildConfig{FCVersion: "v9.9.9"}.Defaulted(), "x86_64", "base", "t.tgz")
	if err == nil {
		t.Fatal("fcWantDigest with an unreachable checksum = nil, want error")
	}
	if !strings.Contains(err.Error(), "published checksum") {
		t.Errorf("error = %v, want it to name the failed checksum fetch", err)
	}
}

// TestEnsureFirecrackerBinDigestMismatch: a tarball that does not match the
// pinned digest is deleted and never installed, and no version stamp is written
// (so the next build retries rather than trusting the cache).
func TestEnsureFirecrackerBinDigestMismatch(t *testing.T) {
	arch, err := fcArch()
	if err != nil {
		t.Skipf("unsupported test architecture: %v", err)
	}
	bc := BuildConfig{}.Defaulted()
	c := Cache{Dir: t.TempDir()}
	// The Fake's `curl -o` is a no-op, so plant the "downloaded" tarball itself.
	tgz := filepath.Join(c.Dir, fmt.Sprintf("firecracker-%s-%s.tgz", bc.FCVersion, arch))
	writeFile(t, tgz)

	f := run.NewFake()
	err = c.ensureFirecrackerBin(context.Background(), f, bc)
	if err == nil {
		t.Fatal("ensureFirecrackerBin with a mismatched tarball = nil, want error")
	}
	if !strings.Contains(err.Error(), "pinned in fcDigests") {
		t.Errorf("error = %v, want it to name the pinned digest as the source", err)
	}
	if exists(tgz) {
		t.Error("mismatched tarball was left in the cache")
	}
	if exists(c.FirecrackerBin()) {
		t.Error("mismatched tarball was installed as the firecracker binary")
	}
	if v := c.FirecrackerVersion(); v != "" {
		t.Errorf("fc-version stamp = %q after a failed download, want empty", v)
	}
	if f.Contains("tar -xzf") {
		t.Errorf("unpacked a tarball that failed verification: %s", f)
	}
}

// TestInstallBinOverRunningBinary is the ETXTBSY regression: refreshing the
// cached firecracker binary must work while a microVM is still executing the old
// one. Copying onto the target in place fails with "text file busy"; renaming
// into place swaps the directory entry and leaves the running process on its
// inode.
func TestInstallBinOverRunningBinary(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "firecracker")

	// A "VMM" that runs until killed, standing in for a booted microVM.
	old := "#!/bin/sh\nexec sleep 60\n"
	if err := os.WriteFile(dst, []byte(old), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(dst)
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot exec the stand-in binary here: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	src := filepath.Join(dir, "new-release")
	if err := os.WriteFile(src, []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := installBin(src, dst); err != nil {
		t.Fatalf("installBin over a running binary = %v, want nil", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) == old {
		t.Error("installBin left the old binary in place")
	}
	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&0o111 == 0 {
		t.Errorf("installed binary mode = %v, want executable", fi.Mode())
	}
	// No temp file left behind for the next build to trip over.
	if exists(dst + ".new") {
		t.Error("installBin left its temp file in the cache")
	}
}

// TestWithArtifactLockSerializes: the cache is exclusive. This is the race that
// corrupted a real cache — a create finishing its base rootfs while a build was
// truncating a new one, leaving a fresh-looking stamp over an empty disk.
func TestWithArtifactLockSerializes(t *testing.T) {
	c := Cache{Dir: t.TempDir()}
	other := lock.NewAt(filepath.Join(c.Dir, artifactLock))
	if err := other.Acquire(); err != nil {
		t.Fatal(err)
	}

	entered := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- c.withArtifactLock(func() error { close(entered); return nil })
	}()

	select {
	case <-entered:
		t.Fatal("withArtifactLock ran fn while another process held the cache")
	case err := <-done:
		t.Fatalf("withArtifactLock returned early: %v", err)
	case <-time.After(150 * time.Millisecond):
		// Still blocked, as it must be.
	}

	other.Release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("withArtifactLock after release = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("withArtifactLock never proceeded after the holder released")
	}
}

// TestWithArtifactLockReports: a caller that has to wait is told why, rather
// than appearing to hang — the lock is held across multi-minute rootfs builds.
func TestWithArtifactLockReports(t *testing.T) {
	var said []string
	c := Cache{Dir: t.TempDir(), Progress: func(s string) { said = append(said, s) }}

	// Uncontended: no waiting line.
	if err := c.withArtifactLock(func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if len(said) != 0 {
		t.Errorf("uncontended lock said %q, want silence", said)
	}

	other := lock.NewAt(filepath.Join(c.Dir, artifactLock))
	if err := other.Acquire(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.withArtifactLock(func() error { return nil }) }()
	// Give the goroutine time to report and block, then let it through.
	time.Sleep(150 * time.Millisecond)
	other.Release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(said) != 1 || !strings.Contains(said[0], "waiting") {
		t.Errorf("contended lock said %q, want a waiting notice", said)
	}
}

// TestEnsureArtifactsTakesTheLock: the guarantee has to hold at the real entry
// point, not just in the helper. EnsureArtifacts fails here (the fake runner
// downloads nothing) — what matters is that it locked the cache on the way.
func TestEnsureArtifactsTakesTheLock(t *testing.T) {
	c := Cache{Dir: t.TempDir()}
	if err := c.EnsureArtifacts(context.Background(), run.NewFake(), BuildConfig{}); err == nil {
		t.Fatal("EnsureArtifacts on an empty cache with a fake runner = nil, want error")
	}
	if !exists(filepath.Join(c.Dir, artifactLock)) {
		t.Error("EnsureArtifacts did not take the artifact lock")
	}
}

// writeExt4 lays down a file that passes isExt4 — enough superblock for the
// magic check, standing in for a real base rootfs.
func writeExt4(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 0x440)
	data[0x438], data[0x439] = 0x53, 0xEF
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}
