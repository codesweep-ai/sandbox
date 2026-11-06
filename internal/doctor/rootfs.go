package doctor

import (
	"fmt"
	"os"
	"path/filepath"
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
// Keyed by image, like the file itself: a host keeps one slot per image version,
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

// baseRootfsSlots is a line per base rootfs the cache holds, then the command
// that drops the ones no sandbox here was made from.
//
// The cache keeps one per image version (R124), so a host where several
// cs-sandbox builds run collects one per version any of them asked for, each a
// sparse 32 GiB file with gigabytes written into it. Nothing else names them.
//
// "Made from" is read off the image id each sandbox's disk records beside it
// (fcdisk.RootfsImage). A sandbox's disk is a copy, so dropping a slot never
// breaks one; it costs whoever next creates from that image a rebuild, which is
// why the slot this report's own image boots from is never offered. A slot with
// no readable stamp says so and is never offered either: what it holds is
// unknown, and unknown is not unused.
func baseRootfsSlots(d Deps) []Check {
	if d.FCCache == "" {
		return nil
	}
	slots := fcdisk.Cache{Dir: d.FCCache}.BaseRootfsSlots()
	madeFrom := map[string]int{}
	if d.InstDir != "" {
		records, _ := filepath.Glob(filepath.Join(d.InstDir, "*", "*", fcdisk.RootfsImage))
		for _, rec := range records {
			if data, err := os.ReadFile(rec); err == nil {
				madeFrom[strings.TrimSpace(string(data))]++
			}
		}
	}
	own := ""
	if d.Image != "" {
		own = baseRootfsPath(d.FCCache, d.Image)
	}
	var checks []Check
	var drop []string
	var dropBytes int64
	for _, s := range slots {
		image := s.Image
		if image == "" && s.ImageID != "" {
			image = "image " + s.ImageID
		}
		users := "image unknown (no stamp), so not offered for removal"
		switch n := madeFrom[s.ImageID]; {
		case s.ImageID == "":
		case n > 0:
			users = image + ", " + sandboxes(n) + " made from it"
		default:
			users = image + ", no sandbox made from it"
			if s.Path != own {
				stamp := strings.TrimSuffix(s.Path, ".ext4") + ".stamp"
				drop = append(drop, s.Path, stamp)
				dropBytes += s.Bytes
			}
		}
		checks = append(checks, Check{Status: HM, Message: fmt.Sprintf("base rootfs %s: %s (%.1f GiB allocated)",
			filepath.Base(s.Path), users, float64(s.Bytes)/(1<<30))})
	}
	if len(drop) > 0 {
		checks = append(checks, Check{Status: HM, Message: fmt.Sprintf(
			"%s no sandbox here was made from hold %.1f GiB; the next create from one rebuilds it. Drop them with:  rm -f %s",
			plural(len(drop)/2, "base rootfs slot"), float64(dropBytes)/(1<<30), strings.Join(drop, " "))})
	}
	return checks
}

func sandboxes(n int) string {
	if n == 1 {
		return "1 sandbox"
	}
	return fmt.Sprintf("%d sandboxes", n)
}
