package fcdisk

import (
	"context"
	"os"
	"path/filepath"
	"testing"

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
	if got.FCVersion != "v1.16.0" {
		t.Errorf("FCVersion = %q, want v1.16.0", got.FCVersion)
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

	// Lay down the four artifacts a microVM boots from.
	writeFile(t, c.FirecrackerBin())
	writeFile(t, c.Kernel())
	writeFile(t, c.Initrd())
	writeFile(t, c.BaseRootfs())
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
	bc := BuildConfig{Kernel: "fedora", KVerPin: pin}

	fresh := func(t *testing.T) Cache {
		c := Cache{Dir: t.TempDir()}
		writeFile(t, c.Kernel())
		writeFile(t, c.Initrd())
		writeFile(t, filepath.Join(c.Dir, "modules.tar"))
		if err := c.writeStamp("kver", pin+".x86_64"); err != nil {
			t.Fatal(err)
		}
		if err := c.writeStamp("kernel-mode", "fedora"); err != nil {
			t.Fatal(err)
		}
		if err := c.writeStamp("kver-pin", pin); err != nil {
			t.Fatal(err)
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
}

// TestEnsureKernelReusesFreshCache: when the cache is fresh, ensureKernel must
// return without shelling out (no podman build). The Fake records zero calls.
func TestEnsureKernelReusesFreshCache(t *testing.T) {
	const pin = "6.19.10-300.fc44"
	c := Cache{Dir: t.TempDir()}
	writeFile(t, c.Kernel())
	writeFile(t, c.Initrd())
	writeFile(t, filepath.Join(c.Dir, "modules.tar"))
	for k, v := range map[string]string{"kver": pin + ".x86_64", "kernel-mode": "fedora", "kver-pin": pin} {
		if err := c.writeStamp(k, v); err != nil {
			t.Fatal(err)
		}
	}
	f := run.NewFake()
	if err := c.ensureKernel(context.Background(), f, BuildConfig{Kernel: "fedora", KVerPin: pin, Image: "img"}); err != nil {
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

func writeFile(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}
