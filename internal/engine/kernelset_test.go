package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codesweep-ai/sandbox/internal/fcconfig"
	"github.com/codesweep-ai/sandbox/internal/fcdisk"
)

// TestCreateNamesTheKeyedKernelSet: the kernel and initrd a create writes into
// run.json are its own build's keyed set, so a stop and start boots the kernel
// the sandbox's disk was made for, whatever another build later puts at the
// top of the cache.
func TestCreateNamesTheKeyedKernelSet(t *testing.T) {
	t.Setenv("CS_SANDBOX_FC_KVER", "")
	t.Setenv("CS_SANDBOX_FC_KERNEL", "")
	cache := t.TempDir()
	k := NewFirecracker(Deps{FCCache: cache}).KernelSet()
	c := fcdisk.Cache{Dir: cache}
	if k.Kernel() == c.Kernel() || k.Initrd() == c.Initrd() {
		t.Fatalf("create names the unkeyed set %s", k.Dir)
	}
	if filepath.Dir(k.Dir) != filepath.Join(cache, "kernels") ||
		!strings.HasPrefix(filepath.Base(k.Dir), strings.TrimPrefix(fcdisk.DefaultKVerPin, "koji:")+".") {
		t.Errorf("kernel set %s is not the default kernel's folder under kernels/", k.Dir)
	}
}

// TestPinKernelSetMovesOneSandboxAtItsStart: a sandbox made before keyed sets
// names the unkeyed paths. At its next launch, where those names link to a
// keyed set, its run.json is pointed at that set: the same files it booted, now
// immune to an older build rewriting the top of the cache. A run.json whose
// unkeyed files are no keyed set's is left exactly as it was.
func TestPinKernelSetMovesOneSandboxAtItsStart(t *testing.T) {
	cache := t.TempDir()
	c := fcdisk.Cache{Dir: cache}
	for _, f := range []string{"vmlinux.elf", "initrd.img"} {
		if err := os.WriteFile(filepath.Join(cache, f), []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeRun := func(t *testing.T) string {
		t.Helper()
		idir := t.TempDir()
		cfg := fcconfig.Build(fcconfig.Spec{KernelPath: c.Kernel(), InitrdPath: c.Initrd(), RootfsPath: "/r", SeedPath: "/s"})
		if err := cfg.WriteFile(filepath.Join(idir, "run.json")); err != nil {
			t.Fatal(err)
		}
		return idir
	}
	boots := func(t *testing.T, idir string) (string, string) {
		t.Helper()
		cfg, err := fcconfig.ReadFile(filepath.Join(idir, "run.json"))
		if err != nil {
			t.Fatal(err)
		}
		return cfg.BootSource.KernelImagePath, cfg.BootSource.InitrdPath
	}

	// The unkeyed files are an older build's own: nothing to move to.
	idir := writeRun(t)
	if err := pinKernelSet(idir, c); err != nil {
		t.Fatal(err)
	}
	if k, i := boots(t, idir); k != c.Kernel() || i != c.Initrd() {
		t.Errorf("run.json moved to %s %s with no keyed set behind the unkeyed names", k, i)
	}

	// The unkeyed names are links to a keyed set's files.
	set := filepath.Join(cache, "kernels", "7.2.5-200.fc44.x86_64-fedora-0123abcd")
	if err := os.MkdirAll(set, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"vmlinux.elf", "initrd.img"} {
		if err := os.Link(filepath.Join(cache, f), filepath.Join(set, f)); err != nil {
			t.Fatal(err)
		}
	}
	if err := pinKernelSet(idir, c); err != nil {
		t.Fatal(err)
	}
	if k, i := boots(t, idir); k != filepath.Join(set, "vmlinux.elf") || i != filepath.Join(set, "initrd.img") {
		t.Errorf("run.json boots %s %s, want the keyed set its unkeyed names link to", k, i)
	}
}
