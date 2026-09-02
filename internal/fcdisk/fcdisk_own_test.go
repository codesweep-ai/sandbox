package fcdisk

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/codesweep-ai/sandbox/internal/run"
)

// requireDiskTools skips unless the host can build and read back an ext4 image.
// Every firecracker host has these (preflight refuses without them); a machine
// running only the unit tier may not.
func requireDiskTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"fakeroot", "mke2fs", "debugfs"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not on PATH", tool)
		}
	}
}

// sourceTree writes a small tree to build a disk from: a file, a subdirectory
// with a file of its own, and a symlink, so the assertions cover the three inode
// kinds a chown has to reach.
func sourceTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "top.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "inner.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("top.txt", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	return dir
}

// diskOwners maps each entry debugfs lists in dir of img to its "uid gid".
// Reading the image beats mounting it: a loop mount needs root, and the
// ownership the guest will see is what the inodes say either way.
//
// `debugfs -R "ls -l"` prints "inode mode (links) uid gid size date time name".
func diskOwners(t *testing.T, img, dir string) map[string]string {
	t.Helper()
	res, err := (&run.Exec{}).Run(context.Background(), run.Opts{ReadOnly: true},
		"debugfs", "-R", "ls -l "+dir, img)
	if err != nil {
		t.Fatalf("debugfs ls -l %s: %v", dir, err)
	}
	out := map[string]string{}
	for line := range strings.SplitSeq(res.Stdout, "\n") {
		f := strings.Fields(line)
		if len(f) < 9 {
			continue
		}
		out[f[len(f)-1]] = f[3] + " " + f[4]
	}
	if len(out) == 0 {
		t.Fatalf("debugfs listed nothing in %s of %s:\n%s", dir, img, res.Stdout)
	}
	return out
}

// TestBuildExt4DirOwnedByRecordsTheOwner: a --snapshot disk has to reach the
// guest owned by the sandbox user, whatever the source belongs to on the host,
// so that sharing a directory in reads the same under both engines (SPEC R163).
//
// Three inodes get there by three different routes, and each has been wrong on
// its own: the tree by fakeroot's chown, the filesystem root — what the user
// sees as ~/<name> — by mke2fs -E root_owner, and lost+found, which mke2fs
// writes as root whatever the tree says.
func TestBuildExt4DirOwnedByRecordsTheOwner(t *testing.T) {
	requireDiskTools(t)
	src := sourceTree(t)
	img := filepath.Join(t.TempDir(), "snap.ext4")

	const uid, gid = 4242, 77
	if err := BuildExt4DirOwnedBy(context.Background(), &run.Exec{}, src, img, 16, uid, gid); err != nil {
		t.Fatal(err)
	}

	const want = "4242 77"
	for dir, entries := range map[string][]string{
		"/":    {".", "top.txt", "sub", "link", "lost+found"},
		"/sub": {"inner.txt"},
	} {
		owners := diskOwners(t, img, dir)
		for _, name := range entries {
			got, ok := owners[name]
			if !ok {
				t.Errorf("%s is missing from %s of the disk: %v", name, dir, owners)
				continue
			}
			if got != want {
				t.Errorf("%s in %s is owned by %q, want %q", name, dir, got, want)
			}
		}
	}
}

// TestBuildExt4DirOwnedByLeavesTheSourceAlone: --snapshot is documented as a
// read-only copy, so building the disk must not rewrite the directory it was
// pointed at. fakeroot attempts the real chown before recording it and keeps
// whatever the caller was allowed to do, which for their own files is any group
// they belong to — so the ownership asked for here is deliberately one that
// would stick.
func TestBuildExt4DirOwnedByLeavesTheSourceAlone(t *testing.T) {
	requireDiskTools(t)
	uid, gid := ownerThatWouldStick(t)
	src := sourceTree(t)
	before := treeOwners(t, src)

	img := filepath.Join(t.TempDir(), "snap.ext4")
	if err := BuildExt4DirOwnedBy(context.Background(), &run.Exec{}, src, img, 16, uid, gid); err != nil {
		t.Fatal(err)
	}

	for path, was := range before {
		if now := lstatOwner(t, path); now != was {
			t.Errorf("%s went from %q to %q on the host", path, was, now)
		}
	}
}

// ownerThatWouldStick returns an owner the caller could really apply to their own
// files, so that an applied chown is distinguishable from a recorded one. Root
// may set any; everyone else can only move a file to another of their groups.
func ownerThatWouldStick(t *testing.T) (uid, gid int) {
	t.Helper()
	if os.Getuid() == 0 {
		return 4242, 77
	}
	groups, err := os.Getgroups()
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range groups {
		if g != os.Getgid() {
			return os.Getuid(), g
		}
	}
	t.Skip("the caller belongs to one group only, so no chown of theirs would stick")
	return 0, 0
}

// treeOwners maps every path under dir to its "uid:gid".
func treeOwners(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(dir, func(path string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		out[path] = lstatOwner(t, path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func lstatOwner(t *testing.T, path string) string {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("this platform does not report a file's owner")
	}
	return fmt.Sprintf("%d:%d", st.Uid, st.Gid)
}
