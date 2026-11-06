package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codesweep-ai/sandbox/internal/fcconfig"
	"github.com/codesweep-ai/sandbox/internal/run"
)

const testImage = "ghcr.io/codesweep-ai/sandbox-slim:v1"

// slotFor is the cache filename this image's rootfs is kept under, spelled out
// here rather than asked of fcdisk: a test that computes the path the same way
// the code does would pass however the keying changed.
func slotFor(t *testing.T, cache string) string {
	t.Helper()
	return filepath.Join(cache, "base-rootfs-ghcr.io-codesweep-ai-sandbox-slim-v1.ext4")
}

// TestBaseRootfsCheckReportsAMissingRootfs: the case that reached a user, whose
// answer has changed. The image being present said nothing about the rootfs made
// from it, and a host with one and not the other was called ready and then
// failed mid-create. It is still reported, and it is now a note rather than an
// issue, because create builds what it is missing (R162) — so the line carries
// what that will cost instead of a command the user must run first.
func TestBaseRootfsCheckReportsAMissingRootfs(t *testing.T) {
	cache := t.TempDir()
	status, msg := baseRootfsCheck(Deps{FCCache: cache, Image: testImage})
	if status != HM {
		t.Errorf("status = %v, want HM (create builds it, so it is not an issue)", status)
	}
	for _, want := range []string{testImage, "cs-sandbox create", "cs-sandbox build"} {
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
		hint := Deps{Image: c.image, DefaultEngine: "podman"}.buildHint("firecracker")
		if got := strings.Contains(hint, "--slim"); got != c.wantSlim {
			t.Errorf("%s: --slim = %v, want %v: %s", c.image, got, c.wantSlim, hint)
		}
		if !strings.Contains(hint, "CS_SANDBOX_IMAGE="+c.image) {
			t.Errorf("%s: hint does not pin an overridden image: %s", c.image, hint)
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
		// The other engine is this host's default, so the report's own has to be
		// asked for by name.
		other := "podman"
		if engine == "podman" {
			other = "firecracker"
		}
		d := Deps{Image: testImage, DefaultEngine: other}
		if hint := d.buildHint(engine); !strings.Contains(hint, "--engine "+engine) {
			t.Errorf("hint for %s names the wrong engine: %s", engine, hint)
		}
	}
}

// TestBuildHintLeavesOutWhatTheHostImplies: the hint is read by somebody who
// has just been told something is missing, and every part of it they would have
// got for free is one more thing to read past. A default image on a host whose
// own engine is the one being reported is a bare `cs-sandbox build`.
func TestBuildHintLeavesOutWhatTheHostImplies(t *testing.T) {
	d := Deps{Image: testImage, ImageIsDefault: true, DefaultEngine: "firecracker"}
	if got := d.buildHint("firecracker"); got != "cs-sandbox build --slim" {
		t.Errorf("hint = %q, want a bare build (--slim carries the variant)", got)
	}
	shipped := Deps{Image: "ghcr.io/codesweep-ai/sandbox:v1", ImageIsDefault: true, DefaultEngine: "firecracker"}
	if got := shipped.buildHint("firecracker"); got != "cs-sandbox build" {
		t.Errorf("hint = %q, want nothing but the command", got)
	}
	// An overridden name is the one thing flags cannot reach, so it stays.
	pinned := Deps{Image: "localhost/sandbox-slim:ci", DefaultEngine: "firecracker"}
	if got := pinned.buildHint("firecracker"); !strings.HasPrefix(got, "CS_SANDBOX_IMAGE=localhost/sandbox-slim:ci ") {
		t.Errorf("hint = %q, want the pinned name spelled out", got)
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
	// A note rather than an issue since create builds what it is missing, but
	// still a different answer for a different image, which is the whole point.
	other := "ghcr.io/codesweep-ai/sandbox:v1"
	status, msg := baseRootfsCheck(Deps{FCCache: cache, Image: other})
	if status != HM {
		t.Errorf("status = %v for %s, want HM — only the slim rootfs is built", status, other)
	}
	if !strings.Contains(msg, other) {
		t.Errorf("message = %q, want it to name %s", msg, other)
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

// TestBaseRootfsSlotsListsEverySlot: a host running several cs-sandbox builds
// keeps a base rootfs per image version, gigabytes each, and this is the only
// place that names them. Each slot says whose it is and whether a sandbox here
// was made from it, and the drop command offers exactly the ones nothing was:
// never this report's own image, never one a sandbox came from, and never one
// whose image is unknown.
func TestBaseRootfsSlotsListsEverySlot(t *testing.T) {
	cache, inst := t.TempDir(), t.TempDir()
	write := func(p, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// This report's image, with no sandbox made from it yet.
	write(slotFor(t, cache), "ext4")
	write(filepath.Join(cache, "base-rootfs-ghcr.io-codesweep-ai-sandbox-slim-v1.stamp"), "sha256:own|k|fedora|i|32G|"+testImage+"\n")
	// Another version, which one sandbox was made from.
	used := filepath.Join(cache, "base-rootfs-ghcr.io-codesweep-ai-sandbox-v2.ext4")
	write(used, "ext4")
	write(filepath.Join(cache, "base-rootfs-ghcr.io-codesweep-ai-sandbox-v2.stamp"), "sha256:used|k|fedora|i|32G|ghcr.io/codesweep-ai/sandbox:v2\n")
	write(filepath.Join(inst, "default", "box", "rootfs.image"), "sha256:used\n")
	// The one-per-repository file an older build wrote, which nothing was made from.
	stale := filepath.Join(cache, "base-rootfs-ghcr.io-codesweep-ai-sandbox.ext4")
	write(stale, "ext4")
	write(filepath.Join(cache, "base-rootfs-ghcr.io-codesweep-ai-sandbox.stamp"), "sha256:stale|k|fedora|i|32G\n")
	// No stamp at all.
	unknown := filepath.Join(cache, "base-rootfs.ext4")
	write(unknown, "ext4")

	checks := baseRootfsSlots(Deps{FCCache: cache, InstDir: inst, Image: testImage})
	var all []string
	for _, c := range checks {
		if c.Status != HM {
			t.Errorf("%q is %v, want HM: a slot is never a problem in itself", c.Message, c.Status)
		}
		all = append(all, c.Message)
	}
	text := strings.Join(all, "\n")
	for _, want := range []string{
		"base-rootfs-ghcr.io-codesweep-ai-sandbox-slim-v1.ext4: " + testImage + ", no sandbox made from it",
		"base-rootfs-ghcr.io-codesweep-ai-sandbox-v2.ext4: ghcr.io/codesweep-ai/sandbox:v2, 1 sandbox made from it",
		"base-rootfs-ghcr.io-codesweep-ai-sandbox.ext4: image sha256:stale, no sandbox made from it",
		"base-rootfs.ext4: image unknown",
		"GiB allocated",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("listing lacks %q:\n%s", want, text)
		}
	}
	if len(checks) != 5 {
		t.Fatalf("got %d lines, want one per slot and one drop command:\n%s", len(checks), text)
	}
	drop := checks[len(checks)-1].Message
	_, cmd, ok := strings.Cut(drop, "rm -f ")
	if !ok {
		t.Fatalf("last line carries no drop command: %s", drop)
	}
	if want := stale + " " + strings.TrimSuffix(stale, ".ext4") + ".stamp"; cmd != want {
		t.Errorf("drop command removes %q, want only the unused slot and its stamp: %q", cmd, want)
	}
}

// TestKernelSetsListsEverySet: a host running several cs-sandbox builds keeps a
// guest kernel set per kernel, and this is the only place that names them. Each
// set says its kernel, its mode and how many sandboxes boot it. A run.json on
// the unkeyed paths counts for the set those names link to. The drop command
// offers exactly the sets nothing boots: never this report's own, never the one
// the unkeyed names link to, and never one with no stamp.
func TestKernelSetsListsEverySet(t *testing.T) {
	cache, inst := t.TempDir(), t.TempDir()
	write := func(p, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	set := func(name, kver, pin string) string {
		dir := filepath.Join(cache, "kernels", name)
		for _, f := range []string{"vmlinux.elf", "initrd.img", "modules.tar"} {
			write(filepath.Join(dir, f), f+" of "+name)
		}
		if kver != "" {
			write(filepath.Join(dir, "kver"), kver+"\n")
			write(filepath.Join(dir, "kernel-mode"), "fedora\n")
			write(filepath.Join(dir, "kver-pin"), pin+"\n")
		}
		return dir
	}
	bootsFrom := func(name, dir string) {
		cfg := fcconfig.Build(fcconfig.Spec{KernelPath: filepath.Join(dir, "vmlinux.elf"), InitrdPath: filepath.Join(dir, "initrd.img")})
		if err := os.MkdirAll(filepath.Join(inst, "default", name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := cfg.WriteFile(filepath.Join(inst, "default", name, "run.json")); err != nil {
			t.Fatal(err)
		}
	}
	own := set("7.2.5-200.fc44.x86_64-fedora-aaaaaaaa", "7.2.5-200.fc44.x86_64", "koji:7.2.5-200.fc44")
	used := set("7.2.6-200.fc44.x86_64-fedora-bbbbbbbb", "7.2.6-200.fc44.x86_64", "koji:7.2.6-200.fc44")
	linked := set("6.19.10-300.fc44.x86_64-fedora-cccccccc", "6.19.10-300.fc44.x86_64", "6.19.10-300.fc44")
	unused := set("6.18.1-200.fc44.x86_64-fedora-dddddddd", "6.18.1-200.fc44.x86_64", "6.18.1-200.fc44")
	set("unknown", "", "")
	// The unkeyed names link to one set, and a sandbox made before keyed sets names them.
	for _, f := range []string{"vmlinux.elf", "initrd.img"} {
		if err := os.Link(filepath.Join(linked, f), filepath.Join(cache, f)); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(cache, "kver"), "6.19.10-300.fc44.x86_64\n")
	bootsFrom("old", cache)
	bootsFrom("box", used)

	checks := kernelSets(Deps{FCCache: cache, InstDir: inst, KernelSet: own})
	var all []string
	for _, c := range checks {
		if c.Status != HM {
			t.Errorf("%q is %v, want HM: a kernel set is never a problem in itself", c.Message, c.Status)
		}
		all = append(all, c.Message)
	}
	text := strings.Join(all, "\n")
	for _, want := range []string{
		"guest kernel set 7.2.5-200.fc44.x86_64-fedora-aaaaaaaa: kernel 7.2.5-200.fc44.x86_64 (fedora, pin koji:7.2.5-200.fc44), no sandbox boots it",
		"guest kernel set 7.2.6-200.fc44.x86_64-fedora-bbbbbbbb: kernel 7.2.6-200.fc44.x86_64 (fedora, pin koji:7.2.6-200.fc44), 1 sandbox boots it",
		"guest kernel set 6.19.10-300.fc44.x86_64-fedora-cccccccc: kernel 6.19.10-300.fc44.x86_64 (fedora, pin 6.19.10-300.fc44), 1 sandbox boots it",
		"guest kernel set unknown: kernel unknown (no stamp)",
		"guest kernel set at the top of the cache: kernel 6.19.10-300.fc44.x86_64, the files of 6.19.10-300.fc44.x86_64-fedora-cccccccc, named by 1 sandbox",
		"MiB)",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("listing lacks %q:\n%s", want, text)
		}
	}
	if len(checks) != 7 {
		t.Fatalf("got %d lines, want one per set, one for the unkeyed set and one drop command:\n%s", len(checks), text)
	}
	drop := checks[len(checks)-1].Message
	if _, cmd, ok := strings.Cut(drop, "rm -rf "); !ok || cmd != unused {
		t.Errorf("drop line = %q, want it to remove only %s", drop, unused)
	}
}
