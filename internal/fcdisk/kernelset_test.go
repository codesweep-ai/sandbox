package fcdisk

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codesweep-ai/sandbox/internal/run"
)

// kernelBuild is a Runner whose `podman cp` out of the kernel build container
// puts down the four files a real build copies out, each naming kver, so a test
// can tell one build's set from another's by content.
type kernelBuild struct {
	*run.Fake
	kver string
}

func (k *kernelBuild) Run(ctx context.Context, opts run.Opts, argv ...string) (run.Result, error) {
	if len(argv) == 4 && argv[0] == "podman" && argv[1] == "cp" && argv[2] == "fckbuild:/artifacts/." {
		for _, f := range []string{"vmlinux.elf", "initrd.img", "modules.tar"} {
			if err := os.WriteFile(filepath.Join(argv[3], f), []byte(f+" of "+k.kver), 0o644); err != nil {
				return run.Result{}, err
			}
		}
		if err := os.WriteFile(filepath.Join(argv[3], "kver"), []byte(k.kver+"\n"), 0o644); err != nil {
			return run.Result{}, err
		}
	}
	return k.Fake.Run(ctx, opts, argv...)
}

// kernelBuildConfig is a fedora build pinned to pin, with an initramfs source
// to hash.
func kernelBuildConfig(t *testing.T, pin string) BuildConfig {
	t.Helper()
	src := filepath.Join(t.TempDir(), "initramfs-init.c")
	if err := os.WriteFile(src, []byte("int main(void){return 0;}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return BuildConfig{Kernel: "fedora", KVerPin: pin, Image: "img", InitramfsSrc: src}
}

// stampUnkeyed lays down an unkeyed set the way a cs-sandbox build before keyed
// sets left it: regular files at the top of the cache, stamped for bc.
func stampUnkeyed(t *testing.T, c Cache, bc BuildConfig, kver string) {
	t.Helper()
	for _, f := range kernelSetFiles {
		if err := os.WriteFile(filepath.Join(c.Dir, f), []byte(f+" of "+kver), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for k, v := range map[string]string{
		"kver":          kver,
		"kernel-mode":   "fedora",
		"kver-pin":      bc.KVerPin,
		"initramfs-src": initramfsStamp(bc),
	} {
		if err := c.writeStamp(k, v); err != nil {
			t.Fatal(err)
		}
	}
}

// snapshot is every kernel file and stamp in dir with its content, and the
// inode of each file, so a test can say a set was left exactly as it was.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, f := range append(append([]string{}, kernelSetFiles...), kernelSetStamps...) {
		p := filepath.Join(dir, f)
		data, err := os.ReadFile(p)
		if err != nil {
			out[f] = "absent"
			continue
		}
		fi, _ := os.Lstat(p)
		out[f] = string(data) + "|" + fi.Mode().String()
	}
	return out
}

func sameSnapshot(t *testing.T, what string, before, after map[string]string) {
	t.Helper()
	for k, v := range before {
		if after[k] != v {
			t.Errorf("%s: %s went from %q to %q", what, k, v, after[k])
		}
	}
}

// TestTwoKernelPinsKeepTheirOwnSets is SBX-120. The guest kernel, initrd and
// modules were one set per cache, so a build pinned to another kernel replaced
// them for every build on the host: on 2026-11-05 kernel 6.19.10 replaced 7.2.5
// for every session. Each pin now builds a set of its own, the second's ensure
// leaves the first's files exactly as they were, and each base rootfs stamp
// names the kernel of its own build.
func TestTwoKernelPinsKeepTheirOwnSets(t *testing.T) {
	c := Cache{Dir: t.TempDir()}
	ctx := context.Background()
	first := kernelBuildConfig(t, "koji:7.2.5-200.fc44")
	second := kernelBuildConfig(t, "6.19.10-300.fc44")

	if err := c.ensureKernel(ctx, &kernelBuild{run.NewFake(), "7.2.5-200.fc44.x86_64"}, first); err != nil {
		t.Fatalf("first ensure = %v, want nil", err)
	}
	one := c.KernelSetFor(first)
	before := snapshot(t, one.Dir)

	if err := c.ensureKernel(ctx, &kernelBuild{run.NewFake(), "6.19.10-300.fc44.x86_64"}, second); err != nil {
		t.Fatalf("second ensure = %v, want nil", err)
	}
	two := c.KernelSetFor(second)

	if one.Dir == two.Dir {
		t.Fatalf("two kernel pins share the set %s", one.Dir)
	}
	sameSnapshot(t, "the first build's set after the second's ensure", before, snapshot(t, one.Dir))
	if got := one.KVer(); got != "7.2.5-200.fc44.x86_64" {
		t.Errorf("first set kver = %q, want its own kernel", got)
	}
	if got := two.KVer(); got != "6.19.10-300.fc44.x86_64" {
		t.Errorf("second set kver = %q, want its own kernel", got)
	}
	data, _ := os.ReadFile(two.Kernel())
	if string(data) != "vmlinux.elf of 6.19.10-300.fc44.x86_64" {
		t.Errorf("second set boots %q, want its own kernel", data)
	}
	if !strings.HasPrefix(filepath.Base(one.Dir), "7.2.5-200.fc44.") {
		t.Errorf("set folder %s does not start with the kernel release it holds", filepath.Base(one.Dir))
	}

	// A base rootfs is built for the kernel its own build boots. Under one set,
	// the second build's kernel was the only kver there was, so the first's
	// next create rebuilt its rootfs for a kernel it never asked for.
	bc, _ := slotBuild(t, "img:a", "sha256:aaaa")
	bc.KVerPin, bc.InitramfsSrc = first.KVerPin, first.InitramfsSrc
	if err := c.ensureBaseRootfs(ctx, run.NewFake().OnStdout("image inspect", "sha256:aaaa"), bc); err != nil {
		t.Fatalf("first build's rootfs = %v, want nil", err)
	}
	if got := strings.Split(c.readStamp(baseRootfsStampName("img:a")), "|")[1]; got != "7.2.5-200.fc44.x86_64" {
		t.Errorf("first build's rootfs stamp names kernel %q, want its own", got)
	}
}

// TestEnsureKernelAdoptsTheUnkeyedSet: a cache from before keyed sets holds one
// set at the top. Where its stamps say it is exactly what this build would
// make, ensure takes it into the keyed set without building anything, and the
// unkeyed names stay regular files with the same bytes and stamps, which is
// what older builds on the host still read.
func TestEnsureKernelAdoptsTheUnkeyedSet(t *testing.T) {
	c := Cache{Dir: t.TempDir()}
	bc := kernelBuildConfig(t, DefaultKVerPin)
	stampUnkeyed(t, c, bc, "7.2.5-200.fc44.x86_64")
	before := snapshot(t, c.Dir)

	r := run.NewFake()
	if err := c.ensureKernel(context.Background(), r, bc); err != nil {
		t.Fatalf("ensure = %v, want nil", err)
	}
	if len(r.Calls) != 0 {
		t.Errorf("adoption ran %d commands, want none: %s", len(r.Calls), r)
	}
	set := c.KernelSetFor(bc)
	if !c.UnkeyedKernelSet().SameFiles(set) {
		t.Error("the keyed set does not carry the unkeyed set's files")
	}
	if set.rebuildReason(bc) != "" || !set.complete() {
		t.Errorf("adopted set is not fresh: %q", set.rebuildReason(bc))
	}
	sameSnapshot(t, "the unkeyed set after adoption", before, snapshot(t, c.Dir))

	// What adoption is for: an older build pinned to another kernel rewrites the
	// unkeyed set the way it always has, removing each name and copying in its
	// own files, and the adopted set still holds the kernel this build boots.
	for _, f := range kernelSetFiles {
		if err := os.Remove(filepath.Join(c.Dir, f)); err != nil {
			t.Fatal(err)
		}
	}
	stampUnkeyed(t, c, kernelBuildConfig(t, "6.19.10-300.fc44"), "6.19.10-300.fc44.x86_64")
	data, _ := os.ReadFile(set.Kernel())
	if string(data) != "vmlinux.elf of 7.2.5-200.fc44.x86_64" || set.KVer() != "7.2.5-200.fc44.x86_64" {
		t.Errorf("an older build's rewrite of the unkeyed set reached the adopted set: %q, kver %q", data, set.KVer())
	}
}

// TestEnsureKernelLeavesAnotherVersionsUnkeyedSet: an unkeyed set built for
// another kernel is what an older build, and the sandboxes it made, boot. A
// build pinned elsewhere makes its own set and does not touch that one.
func TestEnsureKernelLeavesAnotherVersionsUnkeyedSet(t *testing.T) {
	c := Cache{Dir: t.TempDir()}
	older := kernelBuildConfig(t, "6.19.10-300.fc44")
	stampUnkeyed(t, c, older, "6.19.10-300.fc44.x86_64")
	before := snapshot(t, c.Dir)

	bc := kernelBuildConfig(t, DefaultKVerPin)
	if err := c.ensureKernel(context.Background(), &kernelBuild{run.NewFake(), "7.2.5-200.fc44.x86_64"}, bc); err != nil {
		t.Fatalf("ensure = %v, want nil", err)
	}
	sameSnapshot(t, "another version's unkeyed set", before, snapshot(t, c.Dir))
	if got := c.KernelSetFor(bc).KVer(); got != "7.2.5-200.fc44.x86_64" {
		t.Errorf("own set kver = %q, want the kernel it pinned", got)
	}
}

// TestEnsureKernelOffersAFreshCacheItsSet: on a cache with no unkeyed set, the
// set a build makes is also copied to the unkeyed names, so an older build on
// the host finds a kernel where it always has. The stamps there are copies of
// their own too, since an older build writes its stamps in place.
func TestEnsureKernelOffersAFreshCacheItsSet(t *testing.T) {
	c := Cache{Dir: t.TempDir()}
	bc := kernelBuildConfig(t, DefaultKVerPin)
	if err := c.ensureKernel(context.Background(), &kernelBuild{run.NewFake(), "7.2.5-200.fc44.x86_64"}, bc); err != nil {
		t.Fatalf("ensure = %v, want nil", err)
	}
	set, top := c.KernelSetFor(bc), c.UnkeyedKernelSet()
	if !top.SameFiles(set) {
		t.Fatal("the unkeyed names do not carry the new set's files")
	}
	if r := top.rebuildReason(bc); r != "" {
		t.Errorf("an older build of the same pin would rebuild the unkeyed set: %s", r)
	}
	for _, s := range kernelSetStamps {
		a, _ := os.Stat(filepath.Join(top.Dir, s))
		b, _ := os.Stat(filepath.Join(set.Dir, s))
		if a == nil || b == nil || os.SameFile(a, b) {
			t.Errorf("stamp %s at the top is not a copy of its own", s)
		}
	}
}

// TestEnsureKernelRefusesAFinishedSetOfOtherInputs: a finished set under this
// build's key that names other inputs is somebody's, and rebuilding it in place
// changes what their sandboxes boot. A create refuses in one sentence naming
// both and the build that replaces it; that build does.
func TestEnsureKernelRefusesAFinishedSetOfOtherInputs(t *testing.T) {
	c := Cache{Dir: t.TempDir()}
	bc := kernelBuildConfig(t, DefaultKVerPin)
	set := c.KernelSetFor(bc)
	if err := os.MkdirAll(set.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	stampUnkeyed(t, Cache{Dir: set.Dir}, bc, "7.2.5-200.fc44.x86_64")
	if err := set.writeStamp("initramfs-src", "v0-other"); err != nil {
		t.Fatal(err)
	}

	r := &kernelBuild{run.NewFake(), "7.2.5-200.fc44.x86_64"}
	err := c.ensureKernel(context.Background(), r, bc)
	if !errors.Is(err, ErrKernelSetOwned) {
		t.Fatalf("create over another build's set = %v, want ErrKernelSetOwned", err)
	}
	want := "fc: " + set.Dir + " holds the guest kernel set of fedora kernel " + DefaultKVerPin +
		" with initramfs v0-other, not of fedora kernel " + DefaultKVerPin + " with initramfs " + initramfsStamp(bc) +
		", and create will not rebuild a kernel set other sandboxes may boot in place; to replace it, run:  CS_SANDBOX_FC_KVER=" +
		DefaultKVerPin + " cs-sandbox build"
	if err.Error() != want {
		t.Errorf("refusal =\n%s\nwant\n%s", err, want)
	}
	if r.Contains("podman run") {
		t.Error("the refused create built a kernel anyway")
	}

	bc.Replace = true
	if err := c.ensureKernel(context.Background(), r, bc); err != nil {
		t.Fatalf("explicit build = %v, want nil", err)
	}
	if got := set.rebuildReason(bc); got != "" {
		t.Errorf("after the explicit build the set is still stale: %s", got)
	}
}

// TestKeyedSetOf: a run.json naming the unkeyed paths boots whichever keyed set
// holds the same bytes, and nothing else is a keyed set's.
func TestKeyedSetOf(t *testing.T) {
	c := Cache{Dir: t.TempDir()}
	bc := kernelBuildConfig(t, DefaultKVerPin)
	stampUnkeyed(t, c, bc, "7.2.5-200.fc44.x86_64")
	if _, ok := c.KeyedSetOf(c.Kernel(), c.Initrd()); ok {
		t.Fatal("unkeyed files that are no keyed set's resolved to one")
	}
	if err := c.ensureKernel(context.Background(), run.NewFake(), bc); err != nil {
		t.Fatal(err)
	}
	k, ok := c.KeyedSetOf(c.Kernel(), c.Initrd())
	if !ok || k.Dir != c.KernelSetFor(bc).Dir {
		t.Errorf("KeyedSetOf(unkeyed) = %v %v, want the adopted set", k, ok)
	}
	if _, ok := c.KeyedSetOf(k.Kernel(), k.Initrd()); ok {
		t.Error("a run.json already naming a keyed set was resolved again")
	}
}

// TestPlaceFileGivesAnIndependentCopy is SBX-121. A hard link let a write in
// place through one name change the other's bytes, so one build's kernel could
// change under another's. Each name owns its bytes, whether the filesystem
// clones the file or it is copied in full, and the copy keeps content and mode.
func TestPlaceFileGivesAnIndependentCopy(t *testing.T) {
	for _, tc := range []struct {
		name  string
		clone func(dst, src *os.File) error
	}{
		{"clone where the filesystem can", cloneFile},
		{"full copy where it cannot", func(*os.File, *os.File) error { return errors.ErrUnsupported }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func(old func(dst, src *os.File) error) { cloneFile = old }(cloneFile)
			cloneFile = tc.clone

			dir := t.TempDir()
			src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
			if err := os.WriteFile(src, []byte("vmlinux.elf of 7.2.5"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(dst, []byte("what was there"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := placeFile(src, dst); err != nil {
				t.Fatalf("placeFile = %v", err)
			}
			a, _ := os.Stat(src)
			b, err := os.Stat(dst)
			if err != nil {
				t.Fatal(err)
			}
			if os.SameFile(a, b) {
				t.Fatal("placeFile left src and dst one file")
			}
			if data, _ := os.ReadFile(dst); string(data) != "vmlinux.elf of 7.2.5" {
				t.Errorf("dst holds %q, want src's bytes", data)
			}
			if b.Mode() != a.Mode() {
				t.Errorf("dst mode = %v, want %v", b.Mode(), a.Mode())
			}

			// A binary that writes in place, O_TRUNC on the existing path,
			// changes only the name it wrote.
			for _, w := range [][2]string{{dst, src}, {src, dst}} {
				text := "rewritten in place through " + filepath.Base(w[0])
				if err := os.WriteFile(w[0], []byte(text), 0o644); err != nil {
					t.Fatal(err)
				}
				if data, _ := os.ReadFile(w[1]); string(data) == text {
					t.Errorf("a write in place to %s reached %s", filepath.Base(w[0]), filepath.Base(w[1]))
				}
			}
			if _, err := os.Stat(dst + ".tmp"); err == nil {
				t.Error("placeFile left its temporary file behind")
			}
		})
	}
}

// TestEnsureKernelGivesEachNameItsOwnBytes: the keyed set and the unkeyed
// names hold copies, whether the unkeyed set was adopted or offered, so a
// write in place at the top of the cache never reaches the set this build
// boots, and the other way round.
func TestEnsureKernelGivesEachNameItsOwnBytes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, c Cache, bc BuildConfig) run.Runner
	}{
		{"adopted", func(t *testing.T, c Cache, bc BuildConfig) run.Runner {
			stampUnkeyed(t, c, bc, "7.2.5-200.fc44.x86_64")
			return run.NewFake()
		}},
		{"offered", func(t *testing.T, c Cache, bc BuildConfig) run.Runner {
			return &kernelBuild{run.NewFake(), "7.2.5-200.fc44.x86_64"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Cache{Dir: t.TempDir()}
			bc := kernelBuildConfig(t, DefaultKVerPin)
			if err := c.ensureKernel(context.Background(), tc.setup(t, c, bc), bc); err != nil {
				t.Fatalf("ensure = %v, want nil", err)
			}
			set, top := c.KernelSetFor(bc), c.UnkeyedKernelSet()
			for _, f := range kernelSetFiles {
				a, _ := os.Stat(filepath.Join(top.Dir, f))
				b, _ := os.Stat(filepath.Join(set.Dir, f))
				if a == nil || b == nil || os.SameFile(a, b) {
					t.Errorf("%s at the top and in the keyed set are one file", f)
				}
			}
			want, _ := os.ReadFile(set.Kernel())
			if err := os.WriteFile(top.Kernel(), []byte("an older build's kernel"), 0o644); err != nil {
				t.Fatal(err)
			}
			if got, _ := os.ReadFile(set.Kernel()); string(got) != string(want) {
				t.Errorf("a write in place at the top changed the keyed set's kernel to %q", got)
			}
		})
	}
}

// TestEnsureKernelUnsharesALinkedPair: a cache an earlier build left with the
// unkeyed names hard-linked to the keyed set's files gets a copy of its own at
// each unkeyed name the next time this build ensures its set. The keyed files
// keep the inodes their sandboxes booted, and a run.json naming the unkeyed
// paths still resolves to the keyed set.
func TestEnsureKernelUnsharesALinkedPair(t *testing.T) {
	c := Cache{Dir: t.TempDir()}
	bc := kernelBuildConfig(t, DefaultKVerPin)
	if err := c.ensureKernel(context.Background(), &kernelBuild{run.NewFake(), "7.2.5-200.fc44.x86_64"}, bc); err != nil {
		t.Fatal(err)
	}
	set, top := c.KernelSetFor(bc), c.UnkeyedKernelSet()
	inodes := map[string]os.FileInfo{}
	for _, f := range kernelSetFiles {
		_ = os.Remove(filepath.Join(top.Dir, f))
		if err := os.Link(filepath.Join(set.Dir, f), filepath.Join(top.Dir, f)); err != nil {
			t.Fatal(err)
		}
		inodes[f], _ = os.Stat(filepath.Join(set.Dir, f))
	}

	r := run.NewFake()
	if err := c.ensureKernel(context.Background(), r, bc); err != nil {
		t.Fatalf("ensure over a linked pair = %v, want nil", err)
	}
	if len(r.Calls) != 0 {
		t.Errorf("the ensure ran %d commands, want none: %s", len(r.Calls), r)
	}
	for _, f := range kernelSetFiles {
		a, _ := os.Stat(filepath.Join(top.Dir, f))
		b, _ := os.Stat(filepath.Join(set.Dir, f))
		if a == nil || b == nil || os.SameFile(a, b) {
			t.Errorf("%s is still one file at both names", f)
			continue
		}
		if !os.SameFile(b, inodes[f]) {
			t.Errorf("the keyed set's %s was replaced, want it kept as its sandboxes booted it", f)
		}
		x, _ := os.ReadFile(filepath.Join(top.Dir, f))
		y, _ := os.ReadFile(filepath.Join(set.Dir, f))
		if string(x) != string(y) {
			t.Errorf("the unkeyed %s holds %q, want the keyed set's bytes %q", f, x, y)
		}
	}
	if k, ok := c.KeyedSetOf(c.Kernel(), c.Initrd()); !ok || k.Dir != set.Dir {
		t.Errorf("KeyedSetOf(unkeyed) = %v %v after unsharing, want the keyed set", k, ok)
	}
}

// TestSameFilesComparesBytes: two sets hold the same kernel where their files
// are one file or carry the same bytes, and not where the bytes differ at the
// same length.
func TestSameFilesComparesBytes(t *testing.T) {
	put := func(dir, kernel, initrd string) KernelSet {
		t.Helper()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for f, v := range map[string]string{"vmlinux.elf": kernel, "initrd.img": initrd} {
			if err := os.WriteFile(filepath.Join(dir, f), []byte(v), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return KernelSet{Dir: dir}
	}
	root := t.TempDir()
	a := put(filepath.Join(root, "a"), "kernel 7.2.5", "initrd A")
	if !a.SameFiles(put(filepath.Join(root, "b"), "kernel 7.2.5", "initrd A")) {
		t.Error("two copies of one kernel and initrd do not read as the same set")
	}
	if a.SameFiles(put(filepath.Join(root, "c"), "kernel 6.1.9", "initrd A")) {
		t.Error("kernels of the same length and other bytes read as the same")
	}
	if a.SameFiles(put(filepath.Join(root, "d"), "kernel 7.2.5", "initrd B")) {
		t.Error("initrds of the same length and other bytes read as the same")
	}
}
