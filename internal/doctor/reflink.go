package doctor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/codesweep-ai/sandbox/internal/run"
)

// reflinkCheck is the doctor line for extent sharing between the artifact cache
// and the instances tree. Informational, never an issue: without reflink every
// sandbox still works exactly the same, it just costs disk and a copy — so the
// message leads with what it costs rather than with the missing capability.
func reflinkCheck(ctx context.Context, d Deps) (Status, string) {
	if d.FCCache == "" || d.InstDir == "" {
		return HM, "extent sharing unchecked"
	}
	if reflinkWorks(ctx, d.Runner, d.FCCache, d.InstDir) {
		return OK, "instance disks share extents with the base (reflink)"
	}
	size := ""
	if n := baseRootfsRealBytes(d.FCCache); n > 0 {
		size = fmt.Sprintf(" (~%.1f GiB)", float64(n)/(1<<30))
	}
	return HM, "no reflink to the instances dir — each microVM copies the base" + size
}

// probeBytes is the size of the file the reflink probe copies. Comfortably over
// a filesystem block, because a file small enough to live inline in metadata
// (btrfs stores those under max_inline) has no extents to share and would fail
// the clone for a reason that says nothing about the filesystem's capability.
const probeBytes = 64 << 10

// reflinkWorks reports whether a reflink (CoW) copy from srcDir to dstDir
// succeeds — the exact operation the Firecracker engine performs to give each
// sandbox its rootfs (cp --reflink=auto <cache>/base-rootfs.ext4 <inst>/rootfs.ext4).
//
// It probes the real pair of directories rather than reading a filesystem type,
// for two reasons. Capability is not implied by the name: XFS has it only when
// made with reflink=1, ZFS only with block cloning, ext4 not at all. And these
// are two different trees under two different XDG roots — either can be moved
// (CS_SANDBOX_INSTANCES_DIR, XDG_CACHE_HOME) — so a host can be btrfs on both
// sides and still share nothing, because a reflink cannot cross a mount point.
//
// Directories that do not exist yet are created for the probe and removed after,
// so the answer describes where the disks will actually live rather than an
// ancestor that may sit on a different filesystem.
func reflinkWorks(ctx context.Context, r run.Runner, srcDir, dstDir string) bool {
	for _, d := range []string{srcDir, dstDir} {
		undo, err := ensureDir(d)
		if err != nil {
			return false
		}
		// Deferred to function exit on purpose, and the loop runs twice: both
		// directories have to stay until the probe below has used them.
		//nolint:gocritic // deferInLoop: bounded at two, and that is the point
		defer undo()
	}
	src, err := os.CreateTemp(srcDir, ".cs-reflink-probe-*")
	if err != nil {
		return false
	}
	defer os.Remove(src.Name())
	if _, err := src.Write([]byte(strings.Repeat("a", probeBytes))); err != nil {
		src.Close()
		return false
	}
	if err := src.Close(); err != nil {
		return false
	}
	dst := filepath.Join(dstDir, filepath.Base(src.Name())+".copy")
	defer os.Remove(dst)
	// --reflink=always, not auto: auto is what the engine uses because it wants
	// the copy either way, but it succeeds silently on a full copy and so can
	// answer nothing here.
	_, err = r.Run(ctx, run.Opts{}, "cp", "--reflink=always", src.Name(), dst)
	return err == nil
}

// ensureDir creates dir if absent and returns a function that removes exactly
// the components it created, deepest first. Pre-existing directories are left
// alone, and a component that gained other content in the meantime survives:
// os.Remove refuses a non-empty directory, and that refusal is the check.
func ensureDir(dir string) (undo func(), err error) {
	if _, err := os.Stat(dir); err == nil {
		return func() {}, nil
	}
	// Deepest missing ancestor first: everything from there down is ours.
	var made []string
	for p := filepath.Clean(dir); ; p = filepath.Dir(p) {
		if _, err := os.Stat(p); err == nil {
			break
		}
		made = append(made, p)
		if parent := filepath.Dir(p); parent == p {
			break // reached the root without finding anything that exists
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return func() {}, err
	}
	return func() {
		for _, p := range made { // already deepest-first
			_ = os.Remove(p)
		}
	}, nil
}

// baseRootfsRealBytes is the disk a non-reflink host pays per sandbox: the base
// rootfs's *allocated* size, not its apparent one. The fallback copy preserves
// holes (GNU cp defaults to --sparse=auto), so a 32 GiB disk holding 6 GiB costs
// 6, and quoting the apparent size would overstate it fivefold. Zero when the
// base has not been built yet, in which case the caller omits the figure.
func baseRootfsRealBytes(fcCache string) int64 {
	fi, err := os.Stat(filepath.Join(fcCache, "base-rootfs.ext4"))
	if err != nil {
		return 0
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Blocks > 0 {
		return st.Blocks * 512 // st_blocks is always 512-byte units
	}
	return fi.Size()
}
