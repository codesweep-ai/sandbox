//go:build integration

// Integration test for the base rootfs build. The unit tests assert the commands
// the Runner is handed; only a real build says whether the image arrives intact
// in the ext4, and that is the whole question this path answers. The export
// build it replaced got two of these wrong — it dropped every file capability
// the image carried, and its `chmod u+rX` pre-pass rewrote the modes of the
// files the invoking user could not read — which is why they are asserted here
// rather than assumed.
//
//	go test -tags integration -run TestBaseRootfs ./internal/fcdisk/ -v
//
// It costs one image-sized sparse file under TMPDIR, and about a minute on the
// shipped image. Skips gracefully without podman, an image, or the disk tools.
package fcdisk

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codesweep-ai/sandbox/internal/run"
)

// statInRootfs returns what debugfs prints for one path in img. A missing path
// is "" rather than a failure: these assertions name real files in the sandbox
// image, and a test that hard-fails on one absent from a --slim or a private
// image would be asserting the image rather than the build.
func statInRootfs(t *testing.T, img, path string) string {
	t.Helper()
	out, _ := exec.Command("debugfs", "-R", "stat "+path, img).Output()
	if strings.Contains(string(out), "File not found") {
		return ""
	}
	return string(out)
}

func TestBaseRootfsFromMountedImage(t *testing.T) {
	img := image(t)
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman not on PATH")
	}
	requireDiskTools(t)

	dir := t.TempDir()
	c := Cache{Dir: dir, Progress: func(m string) { t.Log(m) }}
	bc := BuildConfig{Image: img, InitPath: filepath.Join(dir, "fc-init"), Kernel: "fedora"}
	if err := os.WriteFile(bc.InitPath, []byte("#!/bin/sh\nexec /sbin/init\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A stand-in for modules.tar. The real one is 105 MB of guest modules, packed
	// as root inside the kernel-build container; this asserts where they land and
	// who owns them there, not what is in them — hence --owner/--group.
	mods := filepath.Join(dir, "modsrc", "0.0.0-test")
	if err := os.MkdirAll(mods, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mods, "modules.dep"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	modTar := filepath.Join(dir, "modules.tar")
	if out, err := exec.Command("tar", "-C", filepath.Join(dir, "modsrc"), "--owner=0", "--group=0", "-cf", modTar, "0.0.0-test").CombinedOutput(); err != nil {
		t.Fatalf("packing the modules stand-in: %v\n%s", err, out)
	}

	rootfs := filepath.Join(dir, "rootfs.ext4")
	if out, err := exec.Command("truncate", "-s", "32G", rootfs).CombinedOutput(); err != nil {
		t.Fatalf("truncate: %v\n%s", err, out)
	}
	build := filepath.Join(dir, "build")
	if err := os.MkdirAll(build, 0o755); err != nil {
		t.Fatal(err)
	}
	err := c.buildBaseRootfsMounted(context.Background(), &run.Exec{}, bc, rootfs, build, modTar, "")
	if errors.Is(err, errNoOverlayMount) {
		t.Skipf("this host takes the export fallback: %v", err)
	}
	if err != nil {
		t.Fatalf("buildBaseRootfsMounted: %v", err)
	}

	// The root inode is mke2fs's own rather than the source tree's, which is why
	// BuildExt4DirOwnedBy has to say it separately with `debugfs sif`. Here it
	// has to come out root-owned without being told.
	root := statInRootfs(t, rootfs, "<2>")
	if !strings.Contains(root, "User:     0   Group:     0") {
		t.Errorf("the filesystem root is not root-owned:\n%s", root)
	}

	for _, tc := range []struct {
		path, mode, why string
	}{
		{"/fc-init", "0755", "the init the guest execs, installed by the build itself"},
		{"/usr/lib/modules/0.0.0-test/modules.dep", "0644", "the guest modules, unpacked over the image"},
		{"/usr/bin/sudo", "04111", "a setuid bit, which survives no copy that is not root"},
		{"/etc/shadow", "0000", "a mode the invoking user cannot read, carried unchanged"},
	} {
		st := statInRootfs(t, rootfs, tc.path)
		if st == "" {
			t.Logf("%s is not in %s; skipped (%s)", tc.path, img, tc.why)
			continue
		}
		if !strings.Contains(st, "Mode:  "+tc.mode) {
			t.Errorf("%s: want mode %s (%s), got:\n%s", tc.path, tc.mode, tc.why, st)
		}
		if !strings.Contains(st, "User:     0   Group:     0") {
			t.Errorf("%s: want root-owned (%s), got:\n%s", tc.path, tc.why, st)
		}
	}

	// File capabilities. SPEC §12 names them as one of the two things a guest
	// still wants from §11.2's container bootstrap, and reading a
	// security.capability xattr as a namespaced root is not the operation the
	// export path performed — it read a tar, under fakeroot, and kept none of
	// them.
	capped := 0
	for _, path := range []string{"/usr/bin/arping", "/usr/bin/clockdiff", "/usr/bin/dumpcap"} {
		if statInRootfs(t, rootfs, path) == "" {
			continue
		}
		out, _ := exec.Command("debugfs", "-R", "ea_list "+path, rootfs).Output()
		if !strings.Contains(string(out), "security.capability") {
			t.Errorf("%s lost its file capabilities:\n%s", path, out)
			continue
		}
		capped++
	}
	if capped == 0 {
		t.Logf("no capability-carrying binary found in %s; that assertion did not run", img)
	}
}
