package doctor

import (
	"os"
	"strings"
	"syscall"

	"github.com/codesweep-ai/sandbox/internal/fcdisk"
)

// baseRootfsCheck is the doctor line for the disk every microVM is copied from.
//
// It is the one firecracker prerequisite `create` does not supply for itself.
// The binary above is fetched on first use and the tier keys are made on first
// create; the base rootfs is built by `cs-sandbox build` and by nothing else, so
// a host without it boots nothing — it dies on the first member, after the group
// and the network are already up.
//
// An issue rather than a note, and that is the distinction the lines above draw:
// what create repairs on its own is informational, and what only a build repairs
// is a real problem. Reported here because until it was, nothing reported it at
// all — `cs-sandbox state` above asks whether the IMAGE is present, the rootfs is
// a separate artifact made FROM that image, and a host that had pulled the one
// without building the other was told it was ready and then failed mid-create.
//
// Keyed by image, like the file itself: a host keeps one slot per image variant,
// so the shipped image being ready says nothing about the slim one. That is not
// hypothetical — it is how this was found, on a host that had built the shipped
// rootfs months ago and had never built the slim one it was about to boot.
func baseRootfsCheck(d Deps) (Status, string) {
	if d.FCCache == "" || d.Image == "" {
		return HM, "base rootfs unchecked — no artifact cache or image resolved"
	}
	path := baseRootfsPath(d.FCCache, d.Image)
	if fileExists(path) {
		return OK, "base rootfs built for " + d.Image
	}
	return NO, "no base rootfs for " + d.Image + " (" + path + ") — build it with:  " + buildHint(d.Image, "firecracker")
}

// buildHint is the command that builds one image, and the artifacts one engine
// needs from it, exactly.
//
// Every part of it is load-bearing, and a shorter form repairs the wrong thing.
// A bare `cs-sandbox build` retargets to the SHIPPED image, so run against a
// missing slim artifact it builds the other variant and leaves this one as
// absent as it found it — with the doctor still reporting the same line. So the
// variant is passed when the name says slim, because --slim is what selects the
// ci-slim.sh Containerfiles.
//
// CS_SANDBOX_IMAGE is named rather than left to the default. It is redundant
// when the image IS the default for its variant and required when it is not —
// CI pins a build to localhost/sandbox-slim:ci, and no combination of flags
// reaches that one. Spelling it out is right in both cases, and a hint that is
// right only sometimes is worse than a longer one.
//
// The engine is the one the report is about. Left off, a build makes firecracker
// artifacts only where the host's automatic engine is firecracker, which is a
// property of the machine reading the message rather than of what it is missing.
func buildHint(image, engine string) string {
	flags := " --engine " + engine
	if isSlim(image) {
		flags += " --slim"
	}
	return "CS_SANDBOX_IMAGE=" + image + " cs-sandbox build" + flags
}

// isSlim reports whether a reference names a slim image, by its repository
// rather than its tag: the variant is what the published package name carries,
// and a tag may say anything at all.
func isSlim(image string) bool {
	repo := image
	if at := strings.IndexByte(repo, '@'); at >= 0 {
		repo = repo[:at]
	}
	if colon := strings.LastIndexByte(repo, ':'); colon > strings.LastIndexByte(repo, '/') {
		repo = repo[:colon]
	}
	return strings.Contains(repo, "slim")
}

// baseRootfsPath is where the cache keeps one image's base rootfs. Asked of
// fcdisk rather than spelled out, because the keying is its rule to change: a
// second copy of it here is how a caller ends up checking a file nothing writes.
func baseRootfsPath(fcCache, image string) string {
	return fcdisk.Cache{Dir: fcCache}.BaseRootfs(image)
}

// baseRootfsRealBytes is the disk a non-reflink host pays per sandbox: the base
// rootfs's *allocated* size, not its apparent one. The fallback copy preserves
// holes (GNU cp defaults to --sparse=auto), so a 32 GiB disk holding 6 GiB costs
// 6, and quoting the apparent size would overstate it fivefold. Zero when the
// base has not been built yet, in which case the caller omits the figure.
//
// It takes the image because the cache is keyed by one. It used to stat an
// unkeyed base-rootfs.ext4, which per-image slots (SPEC R124) replaced — so on
// every host since, it found nothing, returned zero, and quietly dropped the
// figure the warning exists to carry.
func baseRootfsRealBytes(fcCache, image string) int64 {
	fi, err := os.Stat(baseRootfsPath(fcCache, image))
	if err != nil {
		return 0
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Blocks > 0 {
		return st.Blocks * 512 // st_blocks is always 512-byte units
	}
	return fi.Size()
}
