package fcdisk

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

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

// unwrapVmlinuxScript turns the packaged bzImage into the uncompressed ELF
// firecracker boots (R119), which is a scan for the compressed payload inside it
// followed by the matching decompressor.
//
// It is in this repository rather than fetched because the alternative was one
// unauthenticated request to raw.githubusercontent on every kernel build — for
// upstream's scripts/extract-vmlinux — and that host rate-limits shared CI
// egress with a 429. One such request stood between a green run and a red one,
// for a file whose whole content is the twelve lines below.
//
// This output IS the artifact, which is why the step is careful about what it
// accepts: a decompressor that succeeds on the wrong offset, or one that writes
// a diagnostic to stdout, would hand the guest a kernel with something else in
// front of it. Only an output `readelf` recognises as an ELF is taken, and every
// decompressor writes to a file rather than to the pipeline's stdout.
//
// Verified against the fetched script it replaces: both produce the same
// 75,346,184-byte vmlinux.elf, sha256 906957f7…, from the pinned F44 kernel.
//
// The formats are in the order a distro is likely to use, and zstd is first
// because Fedora's kernel is zstd-compressed. A format whose decompressor is not
// in the image is skipped rather than failed, so this stays correct on an image
// that carries fewer of them.
const unwrapVmlinuxScript = `unwrap_vmlinux() {
  _img=$1; _out=$2
  for _spec in '\050\265\057\375:zstd -dc' '\037\213\010:gzip -dc' '\375\067\172\130\132\000:xz -dc' \
               '\135\000\000\000:lzma -dc' '\102\132\150:bzip2 -dc' '\002\041\114\030:lz4 -dc'; do
    _magic=${_spec%%:*}; _dec=${_spec#*:}
    command -v ${_dec%% *} >/dev/null 2>&1 || continue
    for _pos in $(LC_ALL=C grep -abo "$(printf "$_magic")" "$_img" 2>/dev/null | cut -d: -f1); do
      tail -c "+$((_pos + 1))" "$_img" | $_dec > "$_out" 2>/dev/null || true
      if readelf -h "$_out" >/dev/null 2>&1; then return 0; fi
    done
  done
  echo "fc: no compressed kernel found inside $_img" >&2
  return 1
}
`

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
// the base rootfs built from image. It never builds anything; `cs-sandbox build`
// does that.
//
// The rootfs is asked for BY IMAGE, which is the check this could not make while
// one file served every image: a host that had built the slim rootfs passed this
// for a create naming the shipped one, and the microVM booted a filesystem
// nobody asked for. Now the miss is named, and it names the image.
func (c Cache) VerifyArtifacts(image string) error {
	rootfs := c.BaseRootfs(image)
	what := "base rootfs"
	if image != "" {
		what += " for " + image
	}
	for _, a := range []struct{ path, what string }{
		{c.FirecrackerBin(), "firecracker binary"},
		{c.Kernel(), "guest kernel"},
		{c.Initrd(), "guest initrd"},
		{rootfs, what},
	} {
		if !exists(a.path) {
			return fmt.Errorf("%s missing (%s) — run: cs-sandbox build", a.what, a.path)
		}
	}
	if !isExt4(rootfs) {
		return fmt.Errorf("base rootfs is not a filesystem (%s) — an interrupted build left a placeholder; run: cs-sandbox build", rootfs)
	}
	return nil
}

// EnsureArtifacts makes sure the cached firecracker artifacts exist, BUILDING any
// that are missing or stale (firecracker binary download, the fedora guest kernel
// + initrd + modules, and the base rootfs). Only what is missing/stale is rebuilt
// — a fully-populated cache is left untouched.
//
// The build path shells out to podman (a throwaway container for the kernel, a
// read-only image mount for the rootfs), mke2fs (rootfs image) and curl
// (firecracker download) through the Runner. It requires bc.Image and, for the
// rootfs, bc.InitPath. If the build inputs are unavailable and an artifact is
// missing, it returns an actionable error.
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
		if err := c.ensureBaseRootfs(ctx, r, bc); err != nil {
			return err
		}
		c.sayArtifactsReady(bc)
		return nil
	})
}

// sayArtifactsReady closes the artifact step by naming what the cache now holds.
//
// Each step above is silent when it has nothing to do, which is the right shape
// while a build is working and the wrong one when it finishes: a second `build`
// on a host where everything is fresh printed the "setting up…" line and then
// nothing at all, leaving the one question it was asked — is the thing there and
// current? — answered only by the absence of an error.
//
// The figures come from the cache rather than from the steps, so the line reads
// the same whether a step built its artifact just now or found it already built.
// Anything missing is left out rather than reported as empty; the only way to
// reach this line with a gap is a kernel mode that records no version.
func (c Cache) sayArtifactsReady(bc BuildConfig) {
	parts := make([]string, 0, 3)
	if v := c.readStamp("fc-version"); v != "" {
		parts = append(parts, "firecracker "+v)
	}
	if k := c.readStamp("kver"); k != "" {
		parts = append(parts, "guest kernel "+k)
	}
	if b := c.BaseRootfsBytes(bc.Image); b > 0 {
		parts = append(parts, "base filesystem "+gib(b))
	}
	if len(parts) == 0 {
		return
	}
	c.say("ready: %s", strings.Join(parts, ", "))
}

// gib renders a size the way every line here quotes one.
func gib(b int64) string { return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30)) }

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

// Download is a firecracker VMM download running in the background, and the
// handle whoever started it renders and waits on.
//
// It exists because the VMM is ~7 MB of network that used to queue behind a
// 2.14 GB image pull for no reason: it needs neither the image nor the kernel.
// Started alongside the pull it is usually finished before the pull is, and on a
// link slow enough that it is not, the caller draws the bar for the remainder.
//
// The bar belongs to the caller rather than to this download for one practical
// reason: `podman pull` runs attached to the terminal and draws bars of its own,
// so anything written from here while it runs lands in the middle of them. This
// download stays silent, and the caller — which knows when the pull is over —
// decides when the line is free.
//
// Every method is nil-safe, so a caller with no firecracker to fetch holds a nil
// *Download and treats it like any other.
type Download struct {
	label string
	path  string       // the file curl is writing, which is how far it has got
	total atomic.Int64 // Content-Length, set once the HEAD lands; 0 = unknown
	done  chan error
}

// Label names what is being fetched, or "" when nothing is.
func (d *Download) Label() string {
	if d == nil {
		return ""
	}
	return d.label
}

// Total is the size the server promised, or 0 before the HEAD lands and where it
// did not say.
func (d *Download) Total() int64 {
	if d == nil {
		return 0
	}
	return d.total.Load()
}

// Bytes is how much of it is on disk.
func (d *Download) Bytes() int64 {
	if d == nil {
		return 0
	}
	fi, err := os.Stat(d.path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// Wait blocks until the download and its verification finish, and reports what
// happened. Calling it more than once is a programming error; there is one
// result to hand out.
func (d *Download) Wait() error {
	if d == nil || d.done == nil {
		return nil
	}
	return <-d.done
}

// StartFirecrackerBin begins the VMM download in the background, or returns a
// handle with nothing to do when the cache already holds the pinned release.
// The caller MUST Wait before anything else touches the artifact cache: this
// holds the artifact lock while it runs.
func (c Cache) StartFirecrackerBin(ctx context.Context, r run.Runner, bc BuildConfig) *Download {
	bc = bc.Defaulted()
	if c.fcRefreshReason(bc) == "" {
		return nil
	}
	arch, err := fcArch()
	if err != nil {
		d := &Download{done: make(chan error, 1)}
		d.done <- err
		return d
	}
	tgz := fmt.Sprintf("firecracker-%s-%s.tgz", bc.FCVersion, arch)
	d := &Download{
		label: "  firecracker " + bc.FCVersion,
		path:  filepath.Join(c.Dir, tgz),
		done:  make(chan error, 1),
	}
	// Silent while it runs: see the type comment. The lock is taken here rather
	// than left to EnsureArtifacts because this runs before it, and flock is per
	// *Lock rather than per process — the two must not overlap.
	quiet := c
	quiet.Progress, quiet.Bars = nil, nil
	go func() {
		d.total.Store(contentLength(ctx, r, fcReleaseURL(bc.FCVersion)+"/"+tgz))
		d.done <- quiet.withArtifactLock(func() error {
			return quiet.ensureFirecrackerBin(ctx, r, bc)
		})
	}()
	return d
}

// fcReleaseURL is the release directory one firecracker version's assets live in.
func fcReleaseURL(version string) string {
	return "https://github.com/firecracker-microvm/firecracker/releases/download/" + version
}

// contentLength asks how big a download will be, for a progress bar's total.
//
// -I is a HEAD: one round trip that moves no payload. The LAST content-length
// wins, because -L follows GitHub's redirect to the host actually serving the
// asset and every hop carries a header of its own. Zero on any failure, which
// draws a bar without a proportion — a progress bar is not worth failing a
// download over, and the download itself reports its own errors.
func contentLength(ctx context.Context, r run.Runner, url string) int64 {
	res, err := r.Run(ctx, run.Opts{ReadOnly: true}, "curl", "-fsSLI", url)
	if err != nil {
		return 0
	}
	var n int64
	for line := range strings.SplitSeq(res.Stdout, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(k), "content-length") {
			continue
		}
		if parsed, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
			n = parsed
		}
	}
	return n
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
	base := fcReleaseURL(bc.FCVersion)
	tgz := fmt.Sprintf("firecracker-%s-%s.tgz", bc.FCVersion, arch)
	dl := filepath.Join(c.Dir, tgz)
	// A bar for the download, on the path that has one to draw. `build` fetches
	// this alongside the image pull and renders the handle itself (see Download),
	// so there it arrives here with no reporter and this adds nothing. A `create`
	// preparing a host for itself has the terminal to itself, and this is the
	// only place that download can say how far along it is.
	stop := func() {}
	if c.Bars != nil {
		total := contentLength(ctx, r, base+"/"+tgz)
		stop = c.Bars.Watch("  firecracker "+bc.FCVersion, func() (int64, int64) {
			fi, err := os.Stat(dl)
			if err != nil {
				return 0, total
			}
			return fi.Size(), total
		})
	}
	_, err = r.Run(ctx, run.Opts{}, "curl", "-fsSL", "-o", dl, base+"/"+tgz)
	stop() // the line is wanted back before anything below reports on it
	if err != nil {
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
		return errors.New("fc: firecracker binary not found in release tarball")
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
		why = "no digest is pinned for " + arch
	}
	c.say("warning: %s — verifying against the checksum published with it, which is not a trust anchor", why)
	res, err := r.Run(ctx, run.Opts{ReadOnly: true}, "curl", "-fsSL", base+"/"+tgz+".sha256.txt")
	if err != nil {
		return "", false, fmt.Errorf("fc: failed to fetch the published checksum for %s: %w", tgz, err)
	}
	// "<sha256>  <file>.tgz"
	for line := range strings.SplitSeq(res.Stdout, "\n") {
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
		return errors.New("fc: missing host-kernel artifacts and CS_SANDBOX_FC_KERNEL=host build is unsupported; use the default fedora kernel")
	}
	// Fedora mode: rebuild when the mode flipped, any artifact is missing, the
	// pinned kernel NVR changed, or the initramfs source changed.
	if c.kernelRebuildReason(bc) == "" {
		return nil
	}
	if bc.Image == "" {
		return errors.New("fc: guest kernel artifacts missing/stale and no image available to build them")
	}
	if bc.InitramfsSrc == "" {
		return errors.New("fc: guest kernel artifacts missing/stale and no initramfs source available to build them")
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
` + unwrapVmlinuxScript + `
FC_SPEC="$FC_KPKG.$(uname -m)"
dnf install -y --setopt=install_weak_deps=False "$FC_SPEC" gcc glibc-static cpio zstd xz gzip binutils file >/dev/null \
  || { echo "fc: dnf could not install $FC_SPEC (pinned kernel no longer in the Fedora repos? bump CS_SANDBOX_FC_KVER)" >&2; exit 1; }
KVER=$(ls -1 /lib/modules | head -1)
VMZ=/lib/modules/$KVER/vmlinuz; [ -f "$VMZ" ] || VMZ=/boot/vmlinuz-$KVER
mkdir -p /artifacts
unwrap_vmlinux "$VMZ" /artifacts/vmlinux.elf
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

// legacyBaseRootfsStamp vouched for the single unkeyed rootfs. Read only where
// that file is being adopted; nothing writes it any more.
const legacyBaseRootfsStamp = "base-rootfs.stamp"

// baseRootfsStampName is the stamp beside one image's rootfs — the same key the
// disk carries, so the two are removed and written as a pair and neither can end
// up vouching for the other's image.
func baseRootfsStampName(image string) string {
	if image == "" {
		return legacyBaseRootfsStamp
	}
	return "base-rootfs-" + imageSlot(image) + ".stamp"
}

// ensureBaseRootfs builds/refreshes the base rootfs ext4 when the stamp (see
// baseRootfsStamp) changed or the disk is missing. The filesystem is written
// from the image's own mounted tree (baseRootfsMountedScript), falling back to
// the export path on a host that cannot make the merged mount.
func (c Cache) ensureBaseRootfs(ctx context.Context, r run.Runner, bc BuildConfig) error {
	kver := c.readStamp("kver")
	if bc.Kernel == "host" && kver == "" {
		kver = run.Output(ctx, r, "uname", "-r")
	}
	imgid, imgsize := "", int64(0)
	if bc.Image != "" {
		// Id and Size in one inspect. The size is the bar's total below: the
		// filesystem is written from this image's own tree, so what the image
		// occupies is what the ext4 is about to. It is an estimate — ext4 rounds
		// every one of 145k files up to a block, so the real disk runs a few
		// percent over — and the bar treats it as one.
		fields := strings.Fields(run.Output(ctx, r, "podman", "image", "inspect", bc.Image, "--format", "{{.Id}} {{.Size}}"))
		if len(fields) > 0 {
			imgid = fields[0]
		}
		if len(fields) > 1 {
			imgsize, _ = strconv.ParseInt(fields[1], 10, 64)
		}
	}
	inithash := ""
	if bc.InitPath != "" {
		if data, err := os.ReadFile(bc.InitPath); err == nil {
			sum := sha256.Sum256(data)
			inithash = hex.EncodeToString(sum[:])[:12]
		}
	}
	cur := baseRootfsStamp(imgid, kver, bc.Kernel, inithash, bc.RootfsGB)
	rootfs, stamp := c.BaseRootfs(bc.Image), baseRootfsStampName(bc.Image)
	// The stamp alone is not enough: it can vouch for a placeholder left by an
	// interrupted build, so require the disk to actually be a filesystem.
	if exists(rootfs) && isExt4(rootfs) && c.readStamp(stamp) == cur {
		return nil
	}
	// A cache filled before the rootfs was kept per image holds one unkeyed file.
	// Where its stamp says it came from THIS image, take it: a rename costs
	// nothing, and the rebuild it saves costs minutes and several gigabytes. One
	// built from some other image is left where it is — this cannot say which, and
	// deleting somebody's cache to tidy up is not this function's business. The
	// line below says it is there so the space can be reclaimed deliberately.
	if legacy := filepath.Join(c.Dir, legacyBaseRootfs); rootfs != legacy && exists(legacy) {
		switch {
		case isExt4(legacy) && c.readStamp(legacyBaseRootfsStamp) == cur:
			if err := os.Rename(legacy, rootfs); err == nil {
				if err := c.writeStamp(stamp, cur); err == nil {
					_ = os.Remove(c.stampPath(legacyBaseRootfsStamp))
					c.say("adopted the cached base filesystem for %s", bc.Image)
					return nil
				}
				// Stamped nothing over a moved file: the next run rebuilds, which is
				// the harmless direction. Fall through and build now instead.
			}
		default:
			c.say("note: %s was built for another image and is no longer read; remove it to reclaim the space", legacy)
		}
	}
	if bc.Image == "" || bc.InitPath == "" {
		return errors.New("fc: base rootfs missing/stale and cannot build (need image + init path)")
	}
	// Say how long this is about to be quiet for. What follows is one mke2fs pass
	// over every file in the image, and mke2fs has nothing to say while it runs —
	// without -q it prints its stage lines and then sits on "Copying files into
	// the device:" for the whole of it, because the stages that carry a progress
	// meter are the fast ones. So the wait is the thing to announce, the way the
	// guest kernel step above announces its own.
	c.say("building the base sandbox filesystem (this can take a minute)…")
	start := time.Now()
	// Drop the stamp before deleting what it describes, so an interrupted build
	// leaves "no stamp" (rebuild next time) rather than a stamp vouching for the
	// empty truncate placeholder below — the state VerifyArtifacts would accept
	// and a microVM would boot as garbage.
	_ = os.Remove(c.stampPath(stamp))
	_ = os.Remove(rootfs)
	tmp := filepath.Join(c.Dir, "build")
	_ = os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return err
	}
	if _, err := r.Run(ctx, run.Opts{}, "truncate", "-s", strconv.Itoa(bc.RootfsGB)+"G", rootfs); err != nil {
		return err
	}
	// The fedora path pulls guest /lib/modules from modules.tar; host mode copies
	// the host's /lib/modules/<kver>.
	modTar := ""
	if bc.Kernel != "host" {
		modTar = filepath.Join(c.Dir, "modules.tar")
	}
	// The bar watches the filesystem being written grow towards the image it is
	// being written from, because mke2fs reports nothing while it copies. Its
	// total includes the guest modules, which are unpacked into the tree the same
	// pass reads.
	total := imgsize
	if fi, err := os.Stat(modTar); err == nil {
		total += fi.Size()
	}
	sample := func() (int64, int64) { return c.BaseRootfsBytes(bc.Image), total }
	stop := c.Bars.Watch("  base filesystem", sample)
	err := c.buildBaseRootfsMounted(ctx, r, bc, rootfs, tmp, modTar, kver)
	if errors.Is(err, errNoOverlayMount) {
		stop()
		c.say("%v — building it the slower way, by exporting the image", err)
		stop = c.Bars.Watch("  base filesystem", sample)
		err = c.buildBaseRootfsExported(ctx, r, bc, rootfs, tmp, modTar, kver)
	}
	stop()
	_ = os.RemoveAll(tmp)
	if err != nil {
		return err
	}
	// And say what came out. After a minute of silence "done" is worth little on
	// its own; the size is the cheap proof that a filesystem was written rather
	// than a hole, since the file is a 32 GiB sparse one either way.
	if b := c.BaseRootfsBytes(bc.Image); b > 0 {
		c.say("built the base sandbox filesystem: %s in %s", gib(b), time.Since(start).Round(time.Second))
	} else {
		c.say("built the base sandbox filesystem in %s", time.Since(start).Round(time.Second))
	}
	return c.writeStamp(stamp, cur)
}

// errNoOverlayMount reports the one failure of the mounted build that belongs to
// the host's configuration rather than to the build: no unprivileged overlay
// mount. It is what selects the export fallback below.
var errNoOverlayMount = errors.New("fc: this host cannot make an unprivileged overlay mount")

// overlayUnavailable is the exit status baseRootfsMountedScript leaves when the
// merged mount fails. That failure gets a status of its own because it is the
// only one worth retrying differently: it happens before any expensive step, and
// everything after the mount is a real build error that a second, slower attempt
// would only reproduce. The value has only to be one podman, tar, install and
// mke2fs never return; 75 is EX_TEMPFAIL.
const overlayUnavailable = 75

// baseRootfsMountedScript writes the ext4 straight from the image's own tree.
//
// `podman image mount` hands back the merged image with no copy at all, and an
// overlay whose upper carries the two things the image lacks — /fc-init and
// /lib/modules — supplies them without writing into the image, which is mounted
// read-only. One `mke2fs -d` over the merged mount then makes the filesystem in
// a single pass.
//
// What that replaces was three passes over the same 5.4 GB: `podman export`
// wrote the image out as a tar, `tar -x` wrote it again as files, and `mke2fs -d`
// read those back to write it a third time. Two of the three existed only to
// hand bytes to the next step, and those bytes already sat unpacked in podman's
// store. Measured on the shipped image, 2m01s became 50s and ~11 GB of transient
// writes became none.
//
// All of it MUST run in one `podman unshare`: the image mount lives in that
// process's mount namespace and goes when it exits. Being namespaced root there
// is also what retires `fakeroot` from this path — the image's files already
// carry their in-image ownership, so nothing has to fake it, and the `chmod u+rX`
// pre-pass that existed only because the caller could not read the image's
// mode-0000 files goes with it.
//
// It is also the more faithful of the two. Compared entry by entry against an
// export build of the same image, all 145,614 of them agree except where this
// one is right: the export path dropped every `security.capability` xattr the
// image carried (`arping`, `clockdiff`, `dumpcap`), and its `chmod u+rX` left
// /etc/shadow and /etc/gshadow at 0400 in the guest where the image has 0000.
//
// The extras are written THROUGH the merged mount rather than into the upper
// directly, so /lib being a symlink to usr/lib resolves exactly as it did when
// the tree was extracted, and so the mount is attempted before the 105 MB of
// modules are unpacked rather than after.
//
// The trap runs on every exit path. The overlay and the temp dirs would go with
// the namespace anyway, but podman records the image mount in its own store, so
// a skipped `image umount` leaks a refcount that `podman image umount --all`
// then has to clear.
var baseRootfsMountedScript = `set -e
m=$(podman image mount "$FC_IMAGE")
trap 'umount "$FC_MERGED" 2>/dev/null; podman image umount "$FC_IMAGE" >/dev/null 2>&1; rm -rf "$FC_UPPER" "$FC_WORK"' EXIT
mkdir -p "$FC_UPPER" "$FC_WORK" "$FC_MERGED"
mount -t overlay overlay -o lowerdir="$m",upperdir="$FC_UPPER",workdir="$FC_WORK" "$FC_MERGED" || exit ` + strconv.Itoa(overlayUnavailable) + `
mkdir -p "$FC_MERGED/lib/modules"
if [ -n "$FC_MOD_TAR" ]; then tar -C "$FC_MERGED/lib/modules" -xf "$FC_MOD_TAR"
else cp -a "/lib/modules/$FC_KVER" "$FC_MERGED/lib/modules/"; fi
install -m0755 "$FC_INIT" "$FC_MERGED/fc-init"
mke2fs -F -q -t ext4 -d "$FC_MERGED" "$FC_ROOTFS_IMG"`

// buildBaseRootfsMounted builds the base rootfs from the mounted image. It
// returns errNoOverlayMount, and nothing else, when the host cannot make the
// merged mount.
func (c Cache) buildBaseRootfsMounted(ctx context.Context, r run.Runner, bc BuildConfig, rootfs, tmp, modTar, kver string) error {
	env := []string{
		"FC_IMAGE=" + bc.Image,
		"FC_UPPER=" + filepath.Join(tmp, "upper"),
		"FC_WORK=" + filepath.Join(tmp, "work"),
		"FC_MERGED=" + filepath.Join(tmp, "merged"),
		"FC_ROOTFS_IMG=" + rootfs,
		"FC_MOD_TAR=" + modTar,
		"FC_KVER=" + kver,
		"FC_INIT=" + bc.InitPath,
	}
	_, err := r.Run(ctx, run.Opts{Env: env}, "podman", "unshare", "bash", "-c", baseRootfsMountedScript)
	var exit *run.ExitError
	if errors.As(err, &exit) && exit.ExitCode == overlayUnavailable {
		// Carry the kernel's own words: "cannot make an overlay mount" says nothing
		// about which of the several reasons for that this host has.
		if detail := strings.TrimSpace(exit.Stderr); detail != "" {
			return fmt.Errorf("%w (%s)", errNoOverlayMount, detail)
		}
		return errNoOverlayMount
	}
	if err != nil {
		return fmt.Errorf("fc: base rootfs: build: %w", err)
	}
	return nil
}

// baseRootfsExportScript is the fallback assembly: unpack the exported image,
// add the guest modules and /fc-init, and pack the tree into the ext4 — all
// under one fakeroot, so the ownership the image records survives into the
// filesystem rather than becoming the invoking user's. The `chmod u+rX` pre-pass
// is there because that user cannot otherwise read the image's mode-0000 files.
const baseRootfsExportScript = `set -e
tar -C "$FC_TMP" -xpf "$FC_ROOTFS_TAR"
mkdir -p "$FC_TMP/lib/modules"
if [ -n "$FC_MOD_TAR" ]; then tar -C "$FC_TMP/lib/modules" -xf "$FC_MOD_TAR"
else cp -a "/lib/modules/$FC_KVER" "$FC_TMP/lib/modules/"; fi
install -m0755 "$FC_INIT" "$FC_TMP/fc-init"
find "$FC_TMP" ! -readable -exec chmod u+rX {} + 2>/dev/null || true
mke2fs -F -q -t ext4 -d "$FC_TMP" "$FC_ROOTFS_IMG"`

// buildBaseRootfsExported is the fallback for a host that cannot make the merged
// mount — a kernel older than 5.11, a graph driver or a policy that refuses one.
// It writes the image out as a 5.4 GB tar, unpacks it into a second full copy,
// and reads that back into the filesystem, which is ~11 GB written and then
// deleted to move bytes podman already holds unpacked. Nothing here is better
// than the path above: it is only more portable, and it loses the file
// capabilities and the unreadable files' modes that the mounted build keeps.
func (c Cache) buildBaseRootfsExported(ctx context.Context, r run.Runner, bc BuildConfig, rootfs, tmp, modTar, kver string) error {
	// Start from an empty tree: the mounted attempt this follows leaves its
	// (empty) merge point behind, and untarring the image around it would put a
	// stray directory in the guest's root.
	_ = os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return err
	}
	tarPath := filepath.Join(c.Dir, "rootfs.tar")
	defer func() { _ = os.Remove(tarPath) }()
	_, _ = r.Run(ctx, run.Opts{}, "podman", "rm", "-f", "fcbuild")
	if _, err := r.Run(ctx, run.Opts{}, "podman", "create", "--name", "fcbuild", bc.Image, "sleep", "infinity"); err != nil {
		return fmt.Errorf("fc: base rootfs: podman create: %w", err)
	}
	if _, err := r.Run(ctx, run.Opts{}, "podman", "export", "fcbuild", "-o", tarPath); err != nil {
		_, _ = r.Run(ctx, run.Opts{}, "podman", "rm", "-f", "fcbuild")
		return fmt.Errorf("fc: base rootfs: podman export: %w", err)
	}
	_, _ = r.Run(ctx, run.Opts{}, "podman", "rm", "-f", "fcbuild")
	env := []string{
		"FC_TMP=" + tmp,
		"FC_ROOTFS_TAR=" + tarPath,
		"FC_ROOTFS_IMG=" + rootfs,
		"FC_MOD_TAR=" + modTar,
		"FC_KVER=" + kver,
		"FC_INIT=" + bc.InitPath,
	}
	if _, err := r.Run(ctx, run.Opts{Env: env}, "fakeroot", "--", "bash", "-c", baseRootfsExportScript); err != nil {
		return fmt.Errorf("fc: base rootfs: build: %w", err)
	}
	return nil
}
