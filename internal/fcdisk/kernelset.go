package fcdisk

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// kernelSetsDir is the folder under the cache that holds the keyed kernel sets,
// one folder per set: kernels/<key>/ with the three files and four stamps below.
const kernelSetsDir = "kernels"

// kernelSetFiles are what a microVM boots (the kernel and its initramfs) and
// what a base rootfs takes its /lib/modules from.
var kernelSetFiles = []string{"vmlinux.elf", "initrd.img", "modules.tar"}

// kernelSetStamps say what built a set, in the order a build leaves them: kver
// comes out of the build itself, and kver-pin is written last, so a set whose
// stamps are all there was finished.
var kernelSetStamps = []string{"kver", "kernel-mode", "initramfs-src", "kver-pin"}

// KernelSet is one guest kernel, the initramfs that mounts root for it, and its
// module tree, with the stamps that say what built them. The unkeyed set is the
// one at the top of the cache, which every cs-sandbox build before keyed sets
// shared and older builds still read.
type KernelSet struct{ Dir string }

// Kernel is the set's vmlinux.elf.
func (k KernelSet) Kernel() string { return filepath.Join(k.Dir, "vmlinux.elf") }

// Initrd is the set's initrd.img.
func (k KernelSet) Initrd() string { return filepath.Join(k.Dir, "initrd.img") }

// Modules is the set's modules.tar, which a base rootfs unpacks into /lib/modules.
func (k KernelSet) Modules() string { return filepath.Join(k.Dir, "modules.tar") }

// KVer is the kernel release the set's build reported, or "" when it has no stamp.
func (k KernelSet) KVer() string { return k.stamp("kver") }

// Mode is the kernel mode the set was built in ("fedora"), or "".
func (k KernelSet) Mode() string { return k.stamp("kernel-mode") }

// Pin is the CS_SANDBOX_FC_KVER the set was built from, or "".
func (k KernelSet) Pin() string { return k.stamp("kver-pin") }

// Bytes is what the set's three files allocate on disk.
func (k KernelSet) Bytes() int64 {
	var n int64
	for _, f := range kernelSetFiles {
		n += allocatedBytes(filepath.Join(k.Dir, f))
	}
	return n
}

func (k KernelSet) stamp(name string) string {
	data, err := os.ReadFile(filepath.Join(k.Dir, name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func (k KernelSet) writeStamp(name, val string) error {
	return os.WriteFile(filepath.Join(k.Dir, name), []byte(val+"\n"), 0o644)
}

// complete reports whether every file and stamp of the set is there. A set
// missing any of them is an interrupted build or adoption, which is safe to
// redo; a complete one is somebody's finished set.
func (k KernelSet) complete() bool {
	for _, f := range kernelSetFiles {
		if !exists(filepath.Join(k.Dir, f)) {
			return false
		}
	}
	for _, s := range kernelSetStamps {
		if k.stamp(s) == "" {
			return false
		}
	}
	return true
}

// clear removes the set's stamps and then its files, so a set caught halfway
// reads as incomplete and never as finished.
func (k KernelSet) clear() {
	for _, s := range slices.Backward(kernelSetStamps) {
		_ = os.Remove(filepath.Join(k.Dir, s))
	}
	for _, f := range kernelSetFiles {
		_ = os.Remove(filepath.Join(k.Dir, f))
	}
}

// SameFiles reports whether two sets name the same kernel and initramfs on disk,
// as the unkeyed set's names do once they are links to a keyed set's files.
func (k KernelSet) SameFiles(o KernelSet) bool {
	for _, p := range [][2]string{{k.Kernel(), o.Kernel()}, {k.Initrd(), o.Initrd()}} {
		a, err := os.Stat(p[0])
		if err != nil {
			return false
		}
		b, err := os.Stat(p[1])
		if err != nil || !os.SameFile(a, b) {
			return false
		}
	}
	return true
}

// rebuildReason reports why the set cannot serve bc, or "" when it can. It only
// governs fedora mode; host mode is handled in ensureKernel. The pinned-NVR check
// is what forces a rebuild after CS_SANDBOX_FC_KVER or DefaultKVerPin changes.
func (k KernelSet) rebuildReason(bc BuildConfig) string {
	switch {
	case k.stamp("kernel-mode") != "fedora":
		return "kernel mode is not fedora"
	case !exists(k.Kernel()):
		return "vmlinux.elf missing"
	case !exists(k.Initrd()):
		return "initrd.img missing"
	case !exists(k.Modules()):
		return "modules.tar missing"
	case !exists(filepath.Join(k.Dir, "kver")):
		return "kver stamp missing"
	case k.stamp("kver-pin") != bc.KVerPin:
		return "pinned kernel NVR changed"
	case k.stamp("initramfs-src") != initramfsStamp(bc):
		return "initramfs builder changed"
	}
	return ""
}

// UnkeyedKernelSet is the set at the top of the cache: vmlinux.elf, initrd.img
// and modules.tar beside the cache's other files. Older cs-sandbox builds read
// it, and a host-kernel build boots it.
func (c Cache) UnkeyedKernelSet() KernelSet { return KernelSet{Dir: c.Dir} }

// KernelSetFor is the kernel set a build with bc boots and builds its base
// rootfs against.
//
// Keyed by the build's inputs, so two cs-sandbox builds pinned to different
// kernels each keep their own set. One shared set meant a build pinned to
// another kernel replaced it for every build on the host, and each one's next
// create rebuilt its rootfs for a kernel it never asked for (SBX-120). The key
// reads as the kernel release and mode, then a short hash of the pin and the
// initramfs source, so a changed initramfs gets a set of its own rather than
// rewriting one another build boots. Host mode is never built here, and keeps
// the unkeyed set.
func (c Cache) KernelSetFor(bc BuildConfig) KernelSet {
	bc = bc.Defaulted()
	if bc.Kernel == "host" {
		return c.UnkeyedKernelSet()
	}
	return KernelSet{Dir: filepath.Join(c.Dir, kernelSetsDir, kernelSetKey(bc))}
}

// kernelSetKey is the folder name of bc's set. The release part is the one a
// Fedora kernel package installs its modules under, NVR.arch, which the set's
// own kver stamp then confirms.
func kernelSetKey(bc BuildConfig) string {
	nvr := parseKernelPin(bc.KVerPin).NVR
	if nvr == "" {
		nvr = "latest"
	}
	arch, err := fcArch()
	if err != nil {
		arch = runtime.GOARCH
	}
	sum := sha256.Sum256([]byte(bc.KVerPin + "|" + initramfsStamp(bc)))
	h := hex.EncodeToString(sum[:])[:8]
	return slug(bc.KVerPin+"|"+h, 200, nvr+"."+arch, bc.Kernel, h)
}

// KernelSets lists every keyed kernel set the cache holds.
func (c Cache) KernelSets() []KernelSet {
	entries, _ := os.ReadDir(filepath.Join(c.Dir, kernelSetsDir))
	sets := make([]KernelSet, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			sets = append(sets, KernelSet{Dir: filepath.Join(c.Dir, kernelSetsDir, e.Name())})
		}
	}
	return sets
}

// KeyedSetOf is the keyed set whose files a run.json naming the unkeyed kernel
// and initrd actually boots, where those names are links to one. A sandbox made
// before keyed sets names the unkeyed paths, and pointing it at the keyed set
// on its next start keeps it on the kernel it booted, whatever an older build
// later writes at the top of the cache.
func (c Cache) KeyedSetOf(kernel, initrd string) (KernelSet, bool) {
	top := c.UnkeyedKernelSet()
	if kernel != top.Kernel() || initrd != top.Initrd() {
		return KernelSet{}, false
	}
	for _, k := range c.KernelSets() {
		if top.SameFiles(k) {
			return k, true
		}
	}
	return KernelSet{}, false
}

// adoptKernelSet takes the unkeyed set into bc's keyed set where its stamps say
// it is exactly what bc would build, and reports whether it did. Its files are
// linked rather than moved, so the unkeyed names stay what they were for the
// older builds still reading them, and the link costs no space. A set built
// from anything else is left alone.
func (c Cache) adoptKernelSet(set KernelSet, bc BuildConfig) bool {
	top := c.UnkeyedKernelSet()
	if set.Dir == top.Dir || top.rebuildReason(bc) != "" {
		return false
	}
	if err := os.MkdirAll(set.Dir, 0o755); err != nil {
		return false
	}
	set.clear()
	for _, f := range kernelSetFiles {
		if err := placeFile(filepath.Join(top.Dir, f), filepath.Join(set.Dir, f)); err != nil {
			return false
		}
	}
	for _, s := range kernelSetStamps {
		if err := set.writeStamp(s, top.stamp(s)); err != nil {
			return false
		}
	}
	c.say("adopted the cached guest kernel %s", set.KVer())
	return true
}

// offerUnkeyed puts set's files at the unkeyed names where nothing is there,
// so an older cs-sandbox build on the host finds a kernel where it always has.
// Anything already there is left alone: it is what an older build, or the
// sandboxes it made, boot.
//
// The files are links and the stamps are copies, which is what keeps the keyed
// set safe from an older build. One that rebuilds removes the three file names
// before writing new files, so the keyed set keeps its own, but it writes its
// stamps in place, through any link.
func (c Cache) offerUnkeyed(set KernelSet) {
	top := c.UnkeyedKernelSet()
	if set.Dir == top.Dir {
		return
	}
	for _, f := range kernelSetFiles {
		if _, err := os.Lstat(filepath.Join(top.Dir, f)); err == nil {
			return
		}
	}
	for _, f := range kernelSetFiles {
		if err := placeFile(filepath.Join(set.Dir, f), filepath.Join(top.Dir, f)); err != nil {
			return
		}
	}
	for _, s := range kernelSetStamps {
		_ = os.Remove(filepath.Join(top.Dir, s))
		if err := top.writeStamp(s, set.stamp(s)); err != nil {
			return
		}
	}
}

// placeFile gives dst the content of src as a hard link, or as a copy on a
// filesystem that cannot link. A symlink at src is followed, so dst never
// depends on a name somebody else may replace.
func placeFile(src, dst string) error {
	if real, err := filepath.EvalSymlinks(src); err == nil {
		src = real
	}
	_ = os.Remove(dst)
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// ErrKernelSetOwned is what a create gets instead of rebuilding, in place, a
// complete kernel set whose stamps name other inputs. errors.Is matches it.
var ErrKernelSetOwned = errors.New("guest kernel set built from other inputs")

type ownedKernelError struct{ dir, have, want, pin string }

func (e *ownedKernelError) Error() string {
	return fmt.Sprintf("fc: %s holds the guest kernel set of %s, not of %s, and create will not rebuild "+
		"a kernel set other sandboxes may boot in place; to replace it, run:  CS_SANDBOX_FC_KVER=%s cs-sandbox build",
		e.dir, e.have, e.want, e.pin)
}

func (e *ownedKernelError) Is(target error) bool { return target == ErrKernelSetOwned }

// kernelInputs names what a set was built from, the way the refusal quotes it.
func kernelInputs(mode, pin, initramfs string) string {
	return fmt.Sprintf("%s kernel %s with initramfs %s", mode, pin, initramfs)
}
