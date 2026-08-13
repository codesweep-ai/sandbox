package fcdisk

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/codesweep-ai/sandbox/internal/lock"
	"github.com/codesweep-ai/sandbox/internal/run"
)

// DefaultKVerPin is the Fedora kernel-core NVR the guest kernel is pinned to when
// CS_SANDBOX_FC_KVER is unset. It is deliberately the F44 **GA** kernel from the
// frozen `fedora` repo, not a newer `updates` NVR: the `fedora` repo is immutable
// for the release's lifetime, so this pin stays resolvable, whereas `updates`
// churns the kernel release number every few weeks (200 → 202 → …) and drops the
// old one, which would break the build on a schedule. Bump this only on a Fedora
// base-image major bump (e.g. 44 → 45), to that release's GA kernel-core NVR.
const DefaultKVerPin = "6.19.10-300.fc44"

// DefaultFCVersion is the firecracker release tag the host VMM binary is pinned
// to when CS_SANDBOX_FC_VERSION is unset. The cached binary carries an
// `fc-version` stamp, so bumping this pin re-downloads it on the next build —
// bump fcDigests in the same commit.
const DefaultFCVersion = "v1.16.0"

// fcDigests pins the SHA256 of the DefaultFCVersion release tarballs, keyed by
// the firecracker arch name. Verifying against a digest committed *here* — not
// only against the `.sha256.txt` served next to the tarball — is what makes the
// download tamper-evident: a checksum fetched from the same origin as the
// artifact it describes proves nothing if that origin is compromised. An
// overridden CS_SANDBOX_FC_VERSION has no digest here and falls back to the
// published checksum, which catches corruption but is not a trust anchor.
var fcDigests = map[string]string{
	"x86_64":  "bd04e26952d4e158085778c6230a0b383d2619c319182e27eaa9d61a212e92d6",
	"aarch64": "531c713cdbc37d4b8bc2533d851aabc0267096afa1768086a37672abb668efd7",
}

// fcArch maps GOARCH to the arch name firecracker uses in its release assets.
func fcArch() (string, error) {
	switch runtime.GOARCH {
	case "amd64":
		return "x86_64", nil
	case "arm64":
		return "aarch64", nil
	default:
		return "", fmt.Errorf("fc: unsupported architecture %s", runtime.GOARCH)
	}
}

// BuildConfig carries the inputs the artifact BUILD path needs; the caller
// resolves them from the environment. A zero BuildConfig is valid (its
// Defaulted() fills the pins).
type BuildConfig struct {
	Image    string // podman image the kernel/rootfs are built from (required to build)
	InitPath string // host path of the guest init (image/guest/init), baked in as /fc-init
	// InitramfsSrc is the host path of image/guest/initramfs-init.c, compiled
	// static and packed as initrd.img. Required to build the boot artifacts.
	InitramfsSrc string
	Kernel       string // "fedora" (default) or "host"
	KVerPin      string // pinned fedora kernel-core NVR (CS_SANDBOX_FC_KVER); "" = latest
	RootfsGB     int    // base rootfs size in GiB (default 32)
	FCVersion    string // firecracker release tag (default v1.16.0)
}

// Defaulted returns a copy with empty fields filled from the defaults.
func (b BuildConfig) Defaulted() BuildConfig {
	if b.Kernel == "" {
		b.Kernel = "fedora"
	}
	if b.KVerPin == "" && b.Kernel == "fedora" {
		b.KVerPin = DefaultKVerPin
	}
	// 32 GiB, not the guest's working set: the disk is sparse and reflink-shared
	// with every instance, so the number is a ceiling the host is never billed
	// for — only written blocks cost anything. 14 GiB was too tight for real work
	// inside a sandbox (building the sandbox image itself needs ~20).
	if b.RootfsGB == 0 {
		b.RootfsGB = 32
	}
	if b.FCVersion == "" {
		b.FCVersion = DefaultFCVersion
	}
	return b
}

// stampPath returns the path of a small marker file under the cache.
func (c Cache) stampPath(name string) string { return filepath.Join(c.Dir, name) }

func (c Cache) readStamp(name string) string {
	data, err := os.ReadFile(c.stampPath(name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func (c Cache) writeStamp(name, val string) error {
	return os.WriteFile(c.stampPath(name), []byte(val+"\n"), 0o644)
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// isExt4 reports whether p carries an ext4 superblock — magic 0xEF53, stored
// little-endian at offset 0x438. "The file exists" is too weak a test for the
// base rootfs: an interrupted build leaves the truncate placeholder in
// place, which is a hole, not a filesystem, and a microVM booted from it fails
// in ways that point nowhere near the real cause.
func isExt4(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	var b [2]byte
	if _, err := f.ReadAt(b[:], 0x438); err != nil {
		return false
	}
	return b[0] == 0x53 && b[1] == 0xEF
}

// VerifyArtifacts returns an actionable error if any cached artifact a microVM
// boots from is missing — the firecracker binary, the guest kernel + initrd, and
// the base rootfs. It never builds anything; `cs-sandbox build` does that.
func (c Cache) VerifyArtifacts() error {
	for _, a := range []struct{ path, what string }{
		{c.FirecrackerBin(), "firecracker binary"},
		{c.Kernel(), "guest kernel"},
		{c.Initrd(), "guest initrd"},
		{c.BaseRootfs(), "base rootfs"},
	} {
		if !exists(a.path) {
			return fmt.Errorf("%s missing (%s) — run: cs-sandbox build", a.what, a.path)
		}
	}
	if !isExt4(c.BaseRootfs()) {
		return fmt.Errorf("base rootfs is not a filesystem (%s) — an interrupted build left a placeholder; run: cs-sandbox build", c.BaseRootfs())
	}
	return nil
}

// EnsureArtifacts makes sure the cached firecracker artifacts exist, BUILDING any
// that are missing or stale (firecracker binary download, the fedora guest kernel
// + initrd + modules, and the base rootfs). Only what is missing/stale is rebuilt
// — a fully-populated cache is left untouched.
//
// The build path shells out to podman (kernel + rootfs export), fakeroot + mke2fs
// (rootfs image) and curl (firecracker download) through the Runner. It requires
// bc.Image and, for the rootfs, bc.InitPath. If the build inputs are unavailable
// and an artifact is missing, it returns an actionable error.
// It holds the artifact lock throughout, so a `build` and a `create` (or two
// creates) cannot interleave here — see withArtifactLock.
func (c Cache) EnsureArtifacts(ctx context.Context, r run.Runner, bc BuildConfig) error {
	bc = bc.Defaulted()
	return c.withArtifactLock(func() error {
		if err := c.ensureFirecrackerBin(ctx, r, bc); err != nil {
			return err
		}
		if err := c.ensureKernel(ctx, r, bc); err != nil {
			return err
		}
		return c.ensureBaseRootfs(ctx, r, bc)
	})
}

// artifactLock is the lock file serializing access to one artifact cache.
const artifactLock = ".artifacts.lock"

// withArtifactLock runs fn holding an exclusive lock on the cache directory.
//
// `cs-sandbox build` and `cs-sandbox create` run the same artifact path over one
// shared cache, and interleaving them corrupts it: a create that finishes its
// base rootfs and stamps it, followed by a build that deletes that rootfs and
// truncates a fresh one, leaves a stamp claiming "fresh" over an empty disk —
// which VerifyArtifacts accepts and the next microVM boots as garbage. The two
// processes also share one rootfs.tar export path, so either one's cleanup pulls
// the file out from under the other mid-build.
//
// The lock covers reads of the base rootfs too (ReflinkRootfs): copying it while
// another process rewrites it in place would hand the new instance a torn disk.
// A blocked caller is told why, since holding it across a rootfs build means
// minutes of waiting. flock releases on close, so a crashed build cannot wedge
// the cache. Locks are per *Lock, not per process — never nest these.
func (c Cache) withArtifactLock(fn func() error) error {
	if err := os.MkdirAll(c.Dir, 0o755); err != nil {
		return err
	}
	l := lock.NewAt(filepath.Join(c.Dir, artifactLock))
	ok, err := l.TryAcquire()
	if err != nil {
		return err
	}
	if !ok {
		c.say("waiting for another cs-sandbox process to finish with the artifact cache…")
		if err := l.Acquire(); err != nil {
			return err
		}
	}
	defer l.Release()
	return fn()
}

// fcRefreshReason reports why the cached firecracker binary must be
// (re)downloaded for bc, or "" when the cache holds the wanted release. The
// stamp check is what makes a CS_SANDBOX_FC_VERSION / DefaultFCVersion bump take
// effect: without it a cache populated once would pin itself to whatever release
// happened to be current then, forever. A binary with no stamp predates version
// tracking, so its release is unknown and it is refetched once — which also
// re-verifies it against the digest pinned in this repo.
func (c Cache) fcRefreshReason(bc BuildConfig) string {
	switch {
	case !exists(c.FirecrackerBin()):
		return "firecracker binary missing"
	case c.readStamp("fc-version") == "":
		return "cached firecracker binary has no recorded version"
	case c.readStamp("fc-version") != bc.FCVersion:
		return "pinned firecracker version changed"
	}
	return ""
}

// ensureFirecrackerBin downloads + checksum-verifies the firecracker binary when
// the cache is missing it or holds a different release than bc pins.
func (c Cache) ensureFirecrackerBin(ctx context.Context, r run.Runner, bc BuildConfig) error {
	if c.fcRefreshReason(bc) == "" {
		return nil
	}
	fc := c.FirecrackerBin()
	arch, err := fcArch()
	if err != nil {
		return err
	}
	c.say("downloading firecracker %s…", bc.FCVersion)
	if err := os.MkdirAll(filepath.Join(c.Dir, "bin"), 0o755); err != nil {
		return err
	}
	base := "https://github.com/firecracker-microvm/firecracker/releases/download/" + bc.FCVersion
	tgz := fmt.Sprintf("firecracker-%s-%s.tgz", bc.FCVersion, arch)
	dl := filepath.Join(c.Dir, tgz)
	if _, err := r.Run(ctx, run.Opts{}, "curl", "-fsSL", "-o", dl, base+"/"+tgz); err != nil {
		return fmt.Errorf("fc: failed to download %s: %w", tgz, err)
	}
	want, pinned, err := c.fcWantDigest(ctx, r, bc, arch, base, tgz)
	if err != nil {
		_ = os.Remove(dl)
		return err
	}
	data, err := os.ReadFile(dl)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	if want != got {
		_ = os.Remove(dl)
		src := "checksum published with the release"
		if pinned {
			src = "digest pinned in fcDigests"
		}
		return fmt.Errorf("fc: firecracker %s (%s) does not match the %s (want=%s got=%s)",
			bc.FCVersion, arch, src, want, got)
	}
	if _, err := r.Run(ctx, run.Opts{}, "tar", "-xzf", dl, "-C", c.Dir); err != nil {
		return err
	}
	// cp release-*/firecracker-*-<arch> -> bin/firecracker.
	matches, _ := filepath.Glob(filepath.Join(c.Dir, "release-*", "firecracker-*-"+arch))
	if len(matches) == 0 {
		return fmt.Errorf("fc: firecracker binary not found in release tarball")
	}
	// Drop the stamp before replacing the binary so a failure mid-install leaves
	// the cache "unknown version" (refetched next time) rather than a stamp that
	// claims a release the binary on disk is not.
	_ = os.Remove(c.stampPath("fc-version"))
	if err := installBin(matches[0], fc); err != nil {
		return err
	}
	if rel, _ := filepath.Glob(filepath.Join(c.Dir, "release-*")); len(rel) > 0 {
		for _, d := range rel {
			_ = os.RemoveAll(d)
		}
	}
	_ = os.Remove(dl)
	return c.writeStamp("fc-version", bc.FCVersion)
}

// installBin puts src at dst by writing a temp file beside it and renaming it
// into place. The rename only swaps the directory entry, so it succeeds while a
// running microVM is still executing the old binary — copying onto dst directly
// fails there with ETXTBSY ("text file busy"), and would leave a half-written
// VMM behind if it were interrupted. Live VMs keep the inode they booted with;
// the next one gets the new release.
func installBin(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	tmp := dst + ".new"
	if err := os.WriteFile(tmp, data, 0o755); err != nil {
		return err
	}
	// WriteFile only applies the mode when it creates the file, and umask masks
	// it — set the exec bits explicitly.
	if err := os.Chmod(tmp, 0o755); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// fcWantDigest returns the SHA256 the downloaded tarball must match, and whether
// it came from the in-repo pin. The pin only covers DefaultFCVersion; any other
// release falls back to the checksum published alongside it, which is
// corruption-only protection — so that path warns.
func (c Cache) fcWantDigest(ctx context.Context, r run.Runner, bc BuildConfig, arch, base, tgz string) (digest string, pinned bool, err error) {
	why := fmt.Sprintf("%s is not the pinned release (%s)", bc.FCVersion, DefaultFCVersion)
	if bc.FCVersion == DefaultFCVersion {
		if d := fcDigests[arch]; d != "" {
			return d, true, nil
		}
		why = fmt.Sprintf("no digest is pinned for %s", arch)
	}
	c.say("warning: %s — verifying against the checksum published with it, which is not a trust anchor", why)
	res, err := r.Run(ctx, run.Opts{ReadOnly: true}, "curl", "-fsSL", base+"/"+tgz+".sha256.txt")
	if err != nil {
		return "", false, fmt.Errorf("fc: failed to fetch the published checksum for %s: %w", tgz, err)
	}
	// "<sha256>  <file>.tgz"
	for _, line := range strings.Split(res.Stdout, "\n") {
		if f := strings.Fields(line); len(f) > 0 {
			return f[0], false, nil
		}
	}
	return "", false, fmt.Errorf("fc: no checksum found in %s.sha256.txt", tgz)
}

// kernelRebuildReason reports why the cached fedora guest kernel must be
// (re)built for bc, or "" when the cache is fresh and can be reused. It only
// governs fedora mode (bc.Kernel != "host"); host mode is handled in
// ensureKernel. The pinned-NVR check is what forces a rebuild after
// CS_SANDBOX_FC_KVER / DefaultKVerPin changes.
func (c Cache) kernelRebuildReason(bc BuildConfig) string {
	switch {
	case c.readStamp("kernel-mode") != "fedora":
		return "kernel mode is not fedora"
	case !exists(c.Kernel()):
		return "vmlinux.elf missing"
	case !exists(c.Initrd()):
		return "initrd.img missing"
	case !exists(filepath.Join(c.Dir, "modules.tar")):
		return "modules.tar missing"
	case !exists(c.stampPath("kver")):
		return "kver stamp missing"
	case c.readStamp("kver-pin") != bc.KVerPin:
		return "pinned kernel NVR changed"
	case c.readStamp("initramfs-src") != initramfsStamp(bc):
		return "initramfs builder changed"
	}
	return ""
}

// initramfsBuilder is bumped whenever the initramfs *assembly* changes in a way
// the source hash alone would not capture (module list, packing, compiler
// flags). Together with the source hash it keys the initrd.img cache.
const initramfsBuilder = "v1-static-init"

// initramfsStamp identifies the initramfs that bc would produce. An unreadable
// source stamps as empty, which never matches a built artifact, so the rebuild
// then fails loudly with the read error rather than silently reusing a stale
// initrd.
func initramfsStamp(bc BuildConfig) string {
	data, err := os.ReadFile(bc.InitramfsSrc)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return initramfsBuilder + "-" + hex.EncodeToString(sum[:])[:12]
}

// ensureKernel builds/refreshes the boot artifacts (vmlinux.elf + initrd.img,
// plus modules.tar in fedora mode). Only the fedora mode BUILD is supported (the
// default); host mode falls back to an actionable error if the artifacts are
// missing, since it would need the host's own /boot.
func (c Cache) ensureKernel(ctx context.Context, r run.Runner, bc BuildConfig) error {
	if bc.Kernel == "host" {
		// Host-kernel mode is never built here — it would have to lift vmlinuz out
		// of the host's /boot. Reuse a cached artifact set if present; otherwise
		// point the caller back at the default kernel.
		if exists(c.Kernel()) && exists(c.Initrd()) {
			return nil
		}
		return fmt.Errorf("fc: missing host-kernel artifacts and CS_SANDBOX_FC_KERNEL=host build is unsupported; use the default fedora kernel")
	}
	// Fedora mode: rebuild when the mode flipped, any artifact is missing, the
	// pinned kernel NVR changed, or the initramfs source changed.
	if c.kernelRebuildReason(bc) == "" {
		return nil
	}
	if bc.Image == "" {
		return fmt.Errorf("fc: guest kernel artifacts missing/stale and no image available to build them")
	}
	if bc.InitramfsSrc == "" {
		return fmt.Errorf("fc: guest kernel artifacts missing/stale and no initramfs source available to build them")
	}
	c.say("building the guest kernel (this can take a few minutes)…")
	for _, f := range []string{"vmlinux.elf", "initrd.img", "modules.tar", "kver"} {
		_ = os.Remove(c.stampPath(f))
	}
	if err := c.buildFedoraBootArtifacts(ctx, r, bc); err != nil {
		return err
	}
	if err := c.writeStamp("kernel-mode", "fedora"); err != nil {
		return err
	}
	if err := c.writeStamp("initramfs-src", initramfsStamp(bc)); err != nil {
		return err
	}
	return c.writeStamp("kver-pin", bc.KVerPin)
}

// initramfsBuildScript assembles initrd.img: a static init compiled from
// image/guest/initramfs-init.c plus the one module the boot path needs.
//
// An initramfs is unavoidable — Fedora builds CONFIG_VIRTIO_MMIO=m, so no block
// device exists until it is loaded and the kernel cannot mount root=/dev/vda on
// its own. It only has to be *small*: the ~38 MB dracut initrd this replaces
// spent ~2.4 s of every boot probing for storage stacks and networks a microVM
// cannot have.
//
// virtio_blk and ext4 are built into the Fedora kernel, so virtio_mmio is the
// only module needed here; everything else loads from the real root afterwards.
// The numeric prefix makes load order explicit to the init.
const initramfsBuildScript = `IR=/tmp/initramfs
mkdir -p "$IR/modules" "$IR/newroot" "$IR/proc" "$IR/sys" "$IR/dev"
printf '%s' "$FC_INITRAMFS_C" > /tmp/initramfs-init.c
gcc -static -Os -Wall -Wextra -o "$IR/init" /tmp/initramfs-init.c
strip "$IR/init"
for m in virtio_mmio; do
  src=$(find "/lib/modules/$KVER" -name "$m.ko*" | head -1)
  [ -n "$src" ] || { echo "fc: $m.ko not found in guest kernel $KVER" >&2; exit 1; }
  case "$src" in
    *.xz)  xz -dc    "$src" > "$IR/modules/10-$m.ko" ;;
    *.zst) zstd -dc  "$src" > "$IR/modules/10-$m.ko" ;;
    *.gz)  gzip -dc  "$src" > "$IR/modules/10-$m.ko" ;;
    *)     cp        "$src"   "$IR/modules/10-$m.ko" ;;
  esac
done
( cd "$IR" && find . -print0 | cpio --null -o -H newc --quiet ) | gzip -9 > /artifacts/initrd.img`

// buildFedoraBootArtifacts builds everything a microVM boots from, short of the
// rootfs: the Fedora guest kernel (vmlinux.elf), the initramfs that mounts root
// (initrd.img), and the guest's module tree (modules.tar + kver). All in a
// throwaway container from the image.
func (c Cache) buildFedoraBootArtifacts(ctx context.Context, r run.Runner, bc BuildConfig) error {
	kpkg := "kernel-core"
	if bc.KVerPin != "" {
		kpkg = "kernel-core-" + bc.KVerPin
	}
	_, _ = r.Run(ctx, run.Opts{}, "podman", "rm", "-f", "fckbuild")
	// --user 0:0 --entrypoint /bin/bash bypasses the image entrypoint so dnf runs as root.
	// The package spec is arch-qualified ("$FC_KPKG.$(uname -m)"): dnf5 resolves a
	// bare name-version-release inconsistently (an updates-repo NVR fails to match
	// without the arch), so the .arch NEVRA form is the reliable spec.
	initramfsC, err := os.ReadFile(bc.InitramfsSrc)
	if err != nil {
		return fmt.Errorf("fc: reading initramfs source %s: %w", bc.InitramfsSrc, err)
	}
	script := `set -e
FC_SPEC="$FC_KPKG.$(uname -m)"
dnf install -y --setopt=install_weak_deps=False "$FC_SPEC" gcc glibc-static cpio zstd xz gzip binutils file >/dev/null \
  || { echo "fc: dnf could not install $FC_SPEC (pinned kernel no longer in the Fedora repos? bump CS_SANDBOX_FC_KVER)" >&2; exit 1; }
KVER=$(ls -1 /lib/modules | head -1)
VMZ=/lib/modules/$KVER/vmlinuz; [ -f "$VMZ" ] || VMZ=/boot/vmlinuz-$KVER
curl -fsSL https://raw.githubusercontent.com/torvalds/linux/master/scripts/extract-vmlinux -o /tmp/ev; chmod +x /tmp/ev
mkdir -p /artifacts
/tmp/ev "$VMZ" > /artifacts/vmlinux.elf
` + initramfsBuildScript + `
tar -C /lib/modules -cf /artifacts/modules.tar "$KVER"
echo "$KVER" > /artifacts/kver`

	if _, err := r.Run(ctx, run.Opts{Env: []string{"FC_KPKG=" + kpkg}}, "podman", "run",
		"--name", "fckbuild", "--user", "0:0", "-e", "FC_KPKG="+kpkg,
		"-e", "FC_INITRAMFS_C="+string(initramfsC),
		"--entrypoint", "/bin/bash", bc.Image, "-c", script); err != nil {
		_, _ = r.Run(ctx, run.Opts{}, "podman", "rm", "-f", "fckbuild")
		return fmt.Errorf("fc: Fedora kernel build failed: %w", err)
	}
	_, cpErr := r.Run(ctx, run.Opts{}, "podman", "cp", "fckbuild:/artifacts/.", c.Dir+"/")
	_, _ = r.Run(ctx, run.Opts{}, "podman", "rm", "-f", "fckbuild")
	if cpErr != nil {
		return fmt.Errorf("fc: copying kernel artifacts: %w", cpErr)
	}
	for _, f := range []string{"vmlinux.elf", "initrd.img", "modules.tar", "kver"} {
		if fi, err := os.Stat(c.stampPath(f)); err != nil || fi.Size() == 0 {
			return fmt.Errorf("fc: Fedora kernel build produced no %s", f)
		}
	}
	return nil
}

// baseRootfsStamp is what the cached base rootfs is judged fresh against: the
// image it was exported from, the kernel it was built for, and its size.
//
// The size matters as much as the rest. Without it, raising RootfsGB — the
// default here, or CS_SANDBOX_FC_ROOTFS_GB — leaves an existing base at its old
// size forever, so every new sandbox comes up silently smaller than asked for
// with nothing anywhere to point at the cause.
func baseRootfsStamp(imgid, kver, kernelMode, inithash string, gb int) string {
	return fmt.Sprintf("%s|%s|%s|%s|%dG", imgid, kver, kernelMode, inithash, gb)
}

// ensureBaseRootfs builds/refreshes the base rootfs ext4 when the stamp (see
// baseRootfsStamp) changed or the disk is missing.
func (c Cache) ensureBaseRootfs(ctx context.Context, r run.Runner, bc BuildConfig) error {
	kver := c.readStamp("kver")
	if bc.Kernel == "host" && kver == "" {
		kver = run.Output(ctx, r, "uname", "-r")
	}
	imgid := ""
	if bc.Image != "" {
		imgid = run.Output(ctx, r, "podman", "image", "inspect", bc.Image, "--format", "{{.Id}}")
	}
	inithash := ""
	if bc.InitPath != "" {
		if data, err := os.ReadFile(bc.InitPath); err == nil {
			sum := sha256.Sum256(data)
			inithash = hex.EncodeToString(sum[:])[:12]
		}
	}
	cur := baseRootfsStamp(imgid, kver, bc.Kernel, inithash, bc.RootfsGB)
	// The stamp alone is not enough: it can vouch for a placeholder left by an
	// interrupted build, so require the disk to actually be a filesystem.
	if exists(c.BaseRootfs()) && isExt4(c.BaseRootfs()) && c.readStamp("base-rootfs.stamp") == cur {
		return nil
	}
	if bc.Image == "" || bc.InitPath == "" {
		return fmt.Errorf("fc: base rootfs missing/stale and cannot build (need image + init path)")
	}
	c.say("building the base sandbox filesystem…")
	// Drop the stamp before deleting what it describes, so an interrupted build
	// leaves "no stamp" (rebuild next time) rather than a stamp vouching for the
	// empty truncate placeholder below — the state VerifyArtifacts would accept
	// and a microVM would boot as garbage.
	_ = os.Remove(c.stampPath("base-rootfs.stamp"))
	_ = os.Remove(c.BaseRootfs())
	tmp := filepath.Join(c.Dir, "build")
	tarPath := filepath.Join(c.Dir, "rootfs.tar")
	_, _ = r.Run(ctx, run.Opts{}, "podman", "rm", "-f", "fcbuild")
	if _, err := r.Run(ctx, run.Opts{}, "podman", "create", "--name", "fcbuild", bc.Image, "sleep", "infinity"); err != nil {
		return fmt.Errorf("fc: base rootfs: podman create: %w", err)
	}
	if _, err := r.Run(ctx, run.Opts{}, "podman", "export", "fcbuild", "-o", tarPath); err != nil {
		_, _ = r.Run(ctx, run.Opts{}, "podman", "rm", "-f", "fcbuild")
		return fmt.Errorf("fc: base rootfs: podman export: %w", err)
	}
	_, _ = r.Run(ctx, run.Opts{}, "podman", "rm", "-f", "fcbuild")
	_ = os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return err
	}
	if _, err := r.Run(ctx, run.Opts{}, "truncate", "-s", strconv.Itoa(bc.RootfsGB)+"G", c.BaseRootfs()); err != nil {
		return err
	}
	// The fedora path pulls guest /lib/modules from modules.tar; host mode copies
	// the host's /lib/modules/<kver>. Assemble + pack under one fakeroot so the
	// logical ownership survives into the ext4 image.
	modTar := ""
	if bc.Kernel != "host" {
		modTar = filepath.Join(c.Dir, "modules.tar")
	}
	script := `set -e
tar -C "$FC_TMP" -xpf "$FC_ROOTFS_TAR"
mkdir -p "$FC_TMP/lib/modules"
if [ -n "$FC_MOD_TAR" ]; then tar -C "$FC_TMP/lib/modules" -xf "$FC_MOD_TAR"
else cp -a "/lib/modules/$FC_KVER" "$FC_TMP/lib/modules/"; fi
install -m0755 "$FC_INIT" "$FC_TMP/fc-init"
find "$FC_TMP" ! -readable -exec chmod u+rX {} + 2>/dev/null || true
mke2fs -F -q -t ext4 -d "$FC_TMP" "$FC_ROOTFS_IMG"`
	env := []string{
		"FC_TMP=" + tmp,
		"FC_ROOTFS_TAR=" + tarPath,
		"FC_ROOTFS_IMG=" + c.BaseRootfs(),
		"FC_MOD_TAR=" + modTar,
		"FC_KVER=" + kver,
		"FC_INIT=" + bc.InitPath,
	}
	if _, err := r.Run(ctx, run.Opts{Env: env}, "fakeroot", "--", "bash", "-c", script); err != nil {
		_ = os.RemoveAll(tmp)
		_ = os.Remove(tarPath)
		return fmt.Errorf("fc: base rootfs: build: %w", err)
	}
	_ = os.RemoveAll(tmp)
	_ = os.Remove(tarPath)
	return c.writeStamp("base-rootfs.stamp", cur)
}
