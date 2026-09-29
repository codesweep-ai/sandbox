package engine

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/codesweep-ai/sandbox/internal/fcconfig"
)

// TestChooseDiskIOEngine: Async only where io_uring can be set up and its workers
// count against the VM's cgroup, and every Sync says why (SBX-031).
func TestChooseDiskIOEngine(t *testing.T) {
	for _, tc := range []struct {
		name, release string
		probe         error
		disabled      string
		want, why     string
	}{
		{"a current kernel with io_uring", "7.2.7-200.fc44.x86_64", nil, "0", IOEngineAsync, ""},
		{"a kernel with no sysctl for it", "5.15.0-91-generic", nil, "", IOEngineAsync, ""},
		{"a kernel before 5.12", "5.10.0-28-amd64", nil, "", IOEngineSync, "the kernel is 5.10.0-28-amd64"},
		{"the sysctl disables it", "6.8.0-45-generic", syscall.EPERM, "2", IOEngineSync, "kernel.io_uring_disabled is 2"},
		{"the sysctl limits it to a group", "6.8.0-45-generic", syscall.EPERM, "1", IOEngineSync, "kernel.io_uring_disabled is 1"},
		{"a seccomp profile refuses it", "6.8.0-45-generic", syscall.ENOSYS, "0", IOEngineSync, "cannot be set up here"},
	} {
		got, why := chooseDiskIOEngine(tc.release, tc.disabled, tc.probe)
		if got != tc.want || !strings.Contains(why, tc.why) {
			t.Errorf("%s: got %s (%q), want %s with a reason holding %q", tc.name, got, why, tc.want, tc.why)
		}
	}
}

func TestKernelAtLeast(t *testing.T) {
	for release, want := range map[string]bool{
		"5.12.0": true, "5.12-rc1": true, "5.11.22": false, "6.0.0": true,
		"4.18.0-553.el8_10.x86_64": false, "7.2.7-200.fc44.x86_64": true, "": false, "garbage": false,
	} {
		if got := kernelAtLeast(release, 5, 12); got != want {
			t.Errorf("kernelAtLeast(%q, 5, 12) = %v, want %v", release, got, want)
		}
	}
}

// TestSetDiskIOEngineRewritesEveryDrive: the engine is put on every disk at each
// launch, so a sandbox made before the host changed boots on what it can do now.
func TestSetDiskIOEngineRewritesEveryDrive(t *testing.T) {
	idir := t.TempDir()
	cfg := fcconfig.Build(fcconfig.Spec{RootfsPath: "r", SeedPath: "s", StoreDisks: []string{"st"}, VCPUs: 1, MemMiB: 256})
	if err := cfg.WriteFile(filepath.Join(idir, "run.json")); err != nil {
		t.Fatal(err)
	}
	if err := setDiskIOEngine(idir); err != nil {
		t.Fatal(err)
	}
	want, _ := DiskIOEngine()
	got, err := fcconfig.ReadFile(filepath.Join(idir, "run.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range got.Drives {
		if d.IOEngine != want {
			t.Errorf("drive %s io_engine = %q, want %q", d.DriveID, d.IOEngine, want)
		}
	}
	if err := setDiskIOEngine(filepath.Join(idir, "missing")); err == nil || !os.IsNotExist(err) {
		t.Errorf("no run.json: got %v, want not-exist", err)
	}
}
