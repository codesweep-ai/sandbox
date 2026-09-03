package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codesweep-ai/sandbox/internal/run"
)

const testImage = "ghcr.io/codesweep-ai/sandbox-slim:v1"

// slotFor is the cache filename this image's rootfs is kept under, spelled out
// here rather than asked of fcdisk: a test that computes the path the same way
// the code does would pass however the keying changed.
func slotFor(t *testing.T, cache string) string {
	t.Helper()
	return filepath.Join(cache, "base-rootfs-ghcr.io-codesweep-ai-sandbox-slim.ext4")
}

// TestBaseRootfsCheckFailsWhenTheImageHasNoRootfs: the case that reached a user.
// The image is present and every other prerequisite is met, and create still
// cannot boot a member, so this has to count as an issue rather than a note.
func TestBaseRootfsCheckFailsWhenTheImageHasNoRootfs(t *testing.T) {
	cache := t.TempDir()
	status, msg := baseRootfsCheck(Deps{FCCache: cache, Image: testImage})
	if status != NO {
		t.Errorf("status = %v, want NO (create cannot run without it)", status)
	}
	for _, want := range []string{testImage, "cs-sandbox build"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q: %s", want, msg)
		}
	}
}

// TestBuildHintBuildsTheVariantThatIsMissing: the command has to repair the
// rootfs the line is about.
//
// A bare `cs-sandbox build` retargets to the shipped image, so against a missing
// slim rootfs it builds the other disk and changes nothing here. The hint says
// --slim for a slim image, and names the image, so it is right for a reference
// no flag would resolve to.
func TestBuildHintBuildsTheVariantThatIsMissing(t *testing.T) {
	for _, c := range []struct {
		image    string
		wantSlim bool
	}{
		{"ghcr.io/codesweep-ai/sandbox-slim:v1", true},
		{"localhost/sandbox-slim:ci", true},
		{"ghcr.io/codesweep-ai/sandbox:v1", false},
		{"ghcr.io/codesweep-ai/sandbox:slim-sounding-tag", false},
	} {
		hint := buildHint(c.image, "firecracker")
		if got := strings.Contains(hint, "--slim"); got != c.wantSlim {
			t.Errorf("%s: --slim = %v, want %v: %s", c.image, got, c.wantSlim, hint)
		}
		if !strings.Contains(hint, "CS_SANDBOX_IMAGE="+c.image) {
			t.Errorf("%s: hint does not pin the image: %s", c.image, hint)
		}
		if !strings.Contains(hint, "--engine firecracker") {
			t.Errorf("%s: hint does not ask for the engine: %s", c.image, hint)
		}
	}
}

// TestBuildHintNamesTheReportsEngine: the `cs-sandbox state` line is shared by
// both engines, so its hint cannot hardcode one. A podman report that told the
// reader to build firecracker artifacts would be advising work they did not ask
// for, and on a host with no KVM it would simply fail.
func TestBuildHintNamesTheReportsEngine(t *testing.T) {
	for _, engine := range []string{"podman", "firecracker"} {
		if hint := buildHint(testImage, engine); !strings.Contains(hint, "--engine "+engine) {
			t.Errorf("hint for %s names the wrong engine: %s", engine, hint)
		}
	}
}

// TestBaseRootfsCheckIsPerImage: a host that built one variant is not ready for
// another. One shared answer here is what let a host with the shipped rootfs be
// called ready for the slim image it was about to boot.
func TestBaseRootfsCheckIsPerImage(t *testing.T) {
	cache := t.TempDir()
	if err := os.WriteFile(slotFor(t, cache), []byte("ext4"), 0o600); err != nil {
		t.Fatal(err)
	}
	if status, msg := baseRootfsCheck(Deps{FCCache: cache, Image: testImage}); status != OK {
		t.Errorf("status = %v (%s), want OK — this image's rootfs is built", status, msg)
	}
	other := "ghcr.io/codesweep-ai/sandbox:v1"
	if status, _ := baseRootfsCheck(Deps{FCCache: cache, Image: other}); status != NO {
		t.Errorf("status = %v for %s, want NO — only the slim rootfs is built", status, other)
	}
}

// TestBaseRootfsCheckStandsDownWithoutInputs: a report that cannot resolve the
// cache or the image says so, rather than claiming a missing artifact.
func TestBaseRootfsCheckStandsDownWithoutInputs(t *testing.T) {
	for _, d := range []Deps{{Image: testImage}, {FCCache: t.TempDir()}} {
		if status, _ := baseRootfsCheck(d); status != HM {
			t.Errorf("status = %v for %+v, want HM", status, d)
		}
	}
}

// TestReflinkCostReadsTheKeyedRootfs: the size in the reflink warning comes from
// the file this image would actually be copied from.
//
// It used to come from an unkeyed base-rootfs.ext4, which per-image slots
// replaced — so it found nothing on every host and silently dropped the figure.
// Nothing caught that, because the only test wrote the legacy name too.
func TestReflinkCostReadsTheKeyedRootfs(t *testing.T) {
	cache, inst := t.TempDir(), t.TempDir()
	if err := os.WriteFile(slotFor(t, cache), []byte(strings.Repeat("a", 2<<20)), 0o600); err != nil {
		t.Fatal(err)
	}
	d := Deps{FCCache: cache, InstDir: inst, Image: testImage, Runner: run.NewFake().
		On("cp --reflink=always", run.Result{}, &run.ExitError{Argv: []string{"cp"}, ExitCode: 1})}

	_, msg := reflinkCheck(t.Context(), d)
	if !strings.Contains(msg, "GiB") {
		t.Errorf("the warning lost its size figure, so the keyed rootfs went unread: %s", msg)
	}
}

// TestEveryBuildHintComesFromBuildHint: no doctor line spells the build command
// out for itself.
//
// The one that did was wrong for a year's worth of the reasons buildHint now
// documents, and it read perfectly well: `build it with: cs-sandbox build` is
// what anybody would write. What makes it wrong is invisible at the call site —
// the retarget to the shipped image lives in the build command — so this is
// checked structurally rather than left to the next reader to notice.
func TestEveryBuildHintComesFromBuildHint(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "rootfs.go" { // rootfs.go builds it
			continue
		}
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(body), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue // prose may name the command
			}
			if strings.Contains(line, "cs-sandbox build") {
				t.Errorf("%s:%d writes the build command itself; call buildHint(image, engine):\n\t%s",
					f, i+1, strings.TrimSpace(line))
			}
		}
	}
}
