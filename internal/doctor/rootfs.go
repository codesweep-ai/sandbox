package doctor

import (
	"strings"

	"github.com/codesweep-ai/sandbox/internal/fcdisk"
)

// baseRootfsCheck is the doctor line for the disk every microVM is copied from.
//
// A note rather than an issue, which is the distinction the lines above draw:
// what create repairs on its own is informational, and what only a build repairs
// is a real problem. This used to be the second kind and is now the first, since
// create builds the artifacts it is missing (R162). What the line carries now is
// the cost rather than a command: a first create that has to make this one spends
// a minute on it before the sandbox appears.
//
// Reported here because until it was, nothing reported it at all — `cs-sandbox
// state` above asks whether the IMAGE is present, the rootfs is a separate
// artifact made FROM that image, and a host that had pulled the one without
// building the other was told it was ready and then failed mid-create.
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
	return HM, "no base rootfs for " + d.Image + " yet — the first 'cs-sandbox create' builds it (about a minute), or now with:  " + d.buildHint("firecracker")
}

// buildHint is the command that builds this image, and the artifacts one engine
// needs from it, exactly — and nothing the reader's own host already implies.
//
// Each part is carried only when a bare `cs-sandbox build` would do something
// else. A shorter hint that repairs the wrong thing is worse than a longer one,
// and a longer one that repeats what the default already does is a command
// nobody reads.
//
// CS_SANDBOX_IMAGE is what pins a build to a name the flags cannot reach: CI
// builds localhost/sandbox-slim:ci, and no combination of them gets there. It is
// carried only where the name was overridden, because otherwise it says what the
// binary would have worked out for itself.
//
// The engine is carried where it is not the one this host picks on its own,
// which is a property of the machine reading the message rather than of what it
// is missing. --slim is carried by the name, because a bare build retargets to
// the SHIPPED image: run against a missing slim artifact it would build the
// other variant and leave this one as absent as it found it.
func (d Deps) buildHint(engine string) string {
	cmd := "cs-sandbox build"
	if !d.ImageIsDefault {
		cmd = "CS_SANDBOX_IMAGE=" + d.Image + " " + cmd
	}
	if engine != d.DefaultEngine {
		cmd += " --engine " + engine
	}
	if isSlim(d.Image) {
		cmd += " --slim"
	}
	return cmd
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

// baseRootfsRealBytes is the disk a non-reflink host pays per sandbox: what the
// base rootfs allocates, which fcdisk measures. The fallback copy preserves holes
// (GNU cp defaults to --sparse=auto), so a 32 GiB disk holding 6 GiB costs 6, and
// quoting the apparent size would overstate it fivefold. Zero when the base has
// not been built yet, in which case the caller omits the figure.
//
// It takes the image because the cache is keyed by one. It used to stat an
// unkeyed base-rootfs.ext4, which per-image slots (SPEC R124) replaced — so on
// every host since, it found nothing, returned zero, and quietly dropped the
// figure the warning exists to carry.
func baseRootfsRealBytes(fcCache, image string) int64 {
	return fcdisk.Cache{Dir: fcCache}.BaseRootfsBytes(image)
}
