package doctor

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codesweep-ai/sandbox/internal/run"
)

// stubLookPath makes only the named binaries "present".
func stubLookPath(t *testing.T, present ...string) {
	t.Helper()
	set := map[string]bool{}
	for _, p := range present {
		set[p] = true
	}
	orig := lookPath
	lookPath = func(bin string) (string, error) {
		if set[bin] {
			return "/usr/bin/" + bin, nil
		}
		return "", exec.ErrNotFound
	}
	t.Cleanup(func() { lookPath = orig })
}

func TestMissingHostPackagesFedora(t *testing.T) {
	// Only socat + git present; the rest are missing.
	stubLookPath(t, "socat", "git")
	miss := missingHostPackages(false) // fedora names
	joined := strings.Join(miss, " ")
	for _, want := range []string{"passt", "dnsmasq", "fakeroot", "e2fsprogs", "python3", "shadow-utils", "curl"} {
		if !strings.Contains(joined, want) {
			t.Errorf("fedora missing-list should contain %q: %v", want, miss)
		}
	}
	// e2fsprogs appears once even though two binaries (mke2fs, e2fsck) map to it.
	if strings.Count(joined, "e2fsprogs") != 1 {
		t.Errorf("e2fsprogs should be deduped: %v", miss)
	}
	if strings.Contains(joined, "socat") || strings.Contains(joined, "git") {
		t.Errorf("present binaries should not appear: %v", miss)
	}
}

func TestMissingHostPackagesDebian(t *testing.T) {
	stubLookPath(t)                                      // nothing present
	miss := strings.Join(missingHostPackages(true), " ") // debian names
	if !strings.Contains(miss, "dnsmasq-base") {
		t.Errorf("debian should map dnsmasq -> dnsmasq-base: %s", miss)
	}
	if !strings.Contains(miss, "uidmap") {
		t.Errorf("debian should map newuidmap -> uidmap: %s", miss)
	}
	if strings.Contains(miss, "shadow-utils") {
		t.Errorf("debian must not use the fedora name shadow-utils: %s", miss)
	}
}

// On macOS there is no /etc/subuid and no usermod — the podman machine VM owns
// the subuid mapping — so doctor must not report a host-side userns problem or
// hand out a Linux-only remedy.
func TestDiagnoseMacOSNoSubuidAdvice(t *testing.T) {
	stubLookPath(t, "podman", "ssh", "ssh-keygen", "git")
	rep := Diagnose(context.Background(), "podman", Deps{Runner: runningMachine(), User: "jsdelfino", IsMacOS: true})
	all := reportText(rep)
	for _, bad := range []string{"usermod", "subuid/subgid ranges for", "sudo dnf", "sudo apt"} {
		if strings.Contains(all, bad) {
			t.Errorf("macOS report should not mention %q:\n%s", bad, all)
		}
	}
	if rep.Issues != 0 {
		t.Errorf("fully-provisioned macOS host should report 0 issues, got %d:\n%s", rep.Issues, all)
	}
}

// The Firecracker engine is Linux/KVM-only; asking for it on a Mac should say so
// once, not emit /dev/kvm and Linux-package advice.
func TestDiagnoseMacOSFirecracker(t *testing.T) {
	stubLookPath(t, "podman", "ssh", "ssh-keygen", "git")
	rep := Diagnose(context.Background(), "firecracker", Deps{Runner: runningMachine(), User: "jsdelfino", IsMacOS: true})
	all := reportText(rep)
	if !strings.Contains(all, "--engine podman") {
		t.Errorf("macOS firecracker report should point at --engine podman:\n%s", all)
	}
	if strings.Contains(all, "/dev/kvm") || strings.Contains(all, "passt") {
		t.Errorf("macOS firecracker report should skip Linux host checks:\n%s", all)
	}
}

// Linux keeps the usermod remedy when the ranges are absent.
func TestDiagnoseLinuxKeepsSubuidAdvice(t *testing.T) {
	stubLookPath(t, "podman", "ssh", "ssh-keygen", "git")
	rep := Diagnose(context.Background(), "podman", Deps{Runner: run.NewFake(), User: "nosuchuser"})
	if all := reportText(rep); !strings.Contains(all, "usermod --add-subuids") {
		t.Errorf("Linux host without subuid ranges should advise usermod:\n%s", all)
	}
}

// The firecracker line must describe the binary actually on disk, not the pin:
// a cache left at an older release (or one downloaded before the version was
// tracked) has to say so, or doctor reports a version the host does not have.
func TestDiagnoseFirecrackerVersionReporting(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "firecracker")
	if err := os.WriteFile(bin, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, cached, want string
		wantStatus         Status
	}{
		{"matches the pin", "v1.16.0", "firecracker binary cached (v1.16.0)", OK},
		{"stale cache", "v1.15.0", "cached (v1.15.0) but pinned to v1.16.0", HM},
		{"unstamped cache", "", "version unrecorded", HM},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubLookPath(t, "podman", "ssh", "ssh-keygen", "git")
			rep := Diagnose(context.Background(), "firecracker", Deps{
				Runner: run.NewFake(), User: "jsdelfino",
				FCBinPath: bin, FCVersionPin: "v1.16.0", FCVersionCache: tc.cached,
			})
			all := reportText(rep)
			if !strings.Contains(all, tc.want) {
				t.Errorf("report should contain %q:\n%s", tc.want, all)
			}
			if got := fcCheckStatus(t, rep); got != tc.wantStatus {
				t.Errorf("firecracker binary check status = %v, want %v", got, tc.wantStatus)
			}
		})
	}

	// Nothing cached at all — the pin must not be presented as if it were.
	stubLookPath(t, "podman", "ssh", "ssh-keygen", "git")
	all := reportText(Diagnose(context.Background(), "firecracker", Deps{
		Runner: run.NewFake(), User: "jsdelfino",
		FCBinPath: filepath.Join(t.TempDir(), "absent"), FCVersionPin: "v1.16.0",
	}))
	if !strings.Contains(all, "not downloaded yet") {
		t.Errorf("empty cache should report the binary as missing:\n%s", all)
	}
	if strings.Contains(all, "cached (v1.16.0)") {
		t.Errorf("empty cache must not claim a cached version:\n%s", all)
	}
}

// The agent-tooling group covers all three agents: every wrapper and every CLI has
// to be present before it reports OK, and a missing one is named in the advice.
func TestDiagnoseAgentTooling(t *testing.T) {
	base := []string{"podman", "ssh", "ssh-keygen", "git"}
	wrappers := []string{"cs-claude", "cs-codex", "cs-opencode"}
	clis := []string{"claude", "codex", "opencode"}
	diagnose := func(t *testing.T, present ...string) string {
		t.Helper()
		stubLookPath(t, append(append([]string{}, base...), present...)...)
		return reportText(Diagnose(context.Background(), "podman", Deps{Runner: run.NewFake(), User: "jsdelfino"}))
	}

	all := diagnose(t, append(append([]string{}, wrappers...), clis...)...)
	if !strings.Contains(all, "agent tools on PATH (cs-claude, cs-codex, cs-opencode)") {
		t.Errorf("all wrappers present should report them all:\n%s", all)
	}
	if !strings.Contains(all, "agent CLIs present (claude, codex, opencode)") {
		t.Errorf("all CLIs present should report them all:\n%s", all)
	}

	// Dropping any single wrapper or CLI has to show up — otherwise a half-installed
	// toolset reads as healthy.
	for _, missing := range wrappers {
		var have []string
		for _, w := range wrappers {
			if w != missing {
				have = append(have, w)
			}
		}
		out := diagnose(t, append(have, clis...)...)
		if !strings.Contains(out, "agent tools not on PATH") {
			t.Errorf("missing %s should report the tools as not installed:\n%s", missing, out)
		}
	}
	for _, missing := range clis {
		var have []string
		for _, c := range clis {
			if c != missing {
				have = append(have, c)
			}
		}
		out := diagnose(t, append(have, wrappers...)...)
		if !strings.Contains(out, "agent CLI(s) not found: "+missing) {
			t.Errorf("missing %s should be named in the advice:\n%s", missing, out)
		}
	}
}

// fcCheckStatus returns the status of the "firecracker binary" check line.
func fcCheckStatus(t *testing.T, r *Report) Status {
	t.Helper()
	for _, g := range r.Groups {
		for _, c := range g.Checks {
			if strings.HasPrefix(c.Message, "firecracker binary") {
				return c.Status
			}
		}
	}
	t.Fatal("no firecracker binary check in the report")
	return OK
}

// runningMachine is a Fake whose podman machine inspect reports a running VM.
func runningMachine() *run.Fake {
	return run.NewFake().OnStdout("machine inspect", "podman-machine-default running\n")
}

// A stopped machine is a real, actionable problem — and the podman probes that
// depend on it must not be reported as "not built yet".
func TestDiagnoseMacOSMachineStopped(t *testing.T) {
	stubLookPath(t, "podman", "ssh", "ssh-keygen", "git")
	f := run.NewFake().OnStdout("machine inspect", "podman-machine-default stopped\n")
	rep := Diagnose(context.Background(), "podman", Deps{Runner: f, User: "jsdelfino", IsMacOS: true})
	all := reportText(rep)
	if !strings.Contains(all, "podman machine start podman-machine-default") {
		t.Errorf("stopped machine should advise starting it:\n%s", all)
	}
	if rep.Issues != 1 {
		t.Errorf("stopped machine should be exactly 1 issue, got %d:\n%s", rep.Issues, all)
	}
	if strings.Contains(all, "image not built yet") || strings.Contains(all, "not up yet") {
		t.Errorf("state probes should not be trusted with the machine down:\n%s", all)
	}
	if f.Contains("podman image exists") || f.Contains("podman network exists") {
		t.Errorf("state probes should be skipped with the machine down: %s", f)
	}
}

// No machine at all: inspect exits non-zero, so advise init rather than start.
func TestDiagnoseMacOSNoMachine(t *testing.T) {
	stubLookPath(t, "podman", "ssh", "ssh-keygen", "git")
	f := run.NewFake().On("machine inspect", run.Result{ExitCode: 125}, errors.New("VM does not exist"))
	all := reportText(Diagnose(context.Background(), "podman", Deps{Runner: f, User: "jsdelfino", IsMacOS: true}))
	if !strings.Contains(all, "podman machine init") {
		t.Errorf("absent machine should advise init:\n%s", all)
	}
	if strings.Contains(all, "machine start") {
		t.Errorf("absent machine should not advise start:\n%s", all)
	}
}

// Linux has no podman machine — it must not be probed or reported there.
func TestDiagnoseLinuxSkipsMachineCheck(t *testing.T) {
	stubLookPath(t, "podman", "ssh", "ssh-keygen", "git")
	f := run.NewFake()
	all := reportText(Diagnose(context.Background(), "podman", Deps{Runner: f, User: "jsdelfino"}))
	if strings.Contains(all, "podman machine") {
		t.Errorf("Linux report should not mention the podman machine:\n%s", all)
	}
	if f.Contains("machine inspect") {
		t.Errorf("Linux should not probe the podman machine: %s", f)
	}
}

func reportText(r *Report) string {
	var b strings.Builder
	for _, g := range r.Groups {
		b.WriteString(g.Title + "\n")
		for _, c := range g.Checks {
			b.WriteString(c.Message + "\n")
		}
	}
	return b.String()
}

func TestNormalizeAllPresent(t *testing.T) {
	all := []string{}
	for _, p := range hostPackages {
		all = append(all, p.bin)
	}
	stubLookPath(t, all...)
	if miss := missingHostPackages(false); len(miss) != 0 {
		t.Errorf("all present -> no missing, got %v", miss)
	}
}
