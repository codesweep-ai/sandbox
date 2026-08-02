// Package doctor checks host prerequisites, structured so each check is a typed
// result the CLI renders (and the package-name mapping is unit-testable).
package doctor

import (
	"context"
	"os"
	"os/exec"
	"strings"

	"github.com/codesweep-ai/sandbox/internal/run"
)

// Status is a check outcome.
type Status int

const (
	OK Status = iota // good
	NO               // a real problem (counts as an issue)
	HM               // informational / optional
)

// Check is one diagnostic line.
type Check struct {
	Status  Status
	Message string
}

// Group is a titled set of checks.
type Group struct {
	Title  string
	Checks []Check
}

// Report is the full diagnosis.
type Report struct {
	Engine string
	Groups []Group
	Issues int
}

// Deps are the inputs doctor needs.
type Deps struct {
	Runner  run.Runner
	User    string
	TierDir string
	Image   string
	Network string
	IsMacOS bool

	FCBinPath      string // cached firecracker binary
	FCVersionPin   string // release the build pins to (CS_SANDBOX_FC_VERSION / the default)
	FCVersionCache string // release actually cached (fc-version stamp); "" = unknown
}

// Diagnose runs the checks for the given engine ("podman" | "firecracker").
func Diagnose(ctx context.Context, engine string, d Deps) *Report {
	r := &Report{Engine: engine}
	pkg := "dnf install"
	apt := false
	switch {
	case d.IsMacOS:
		pkg = "brew install"
	case !have("dnf") && have("apt-get"):
		pkg = "apt install"
		apt = true
	}
	// `sudo` is how you install on Linux; Homebrew refuses to run under it.
	sudo := "sudo "
	if d.IsMacOS {
		sudo = ""
	}

	// podman + ssh + git — required on every host, both engines.
	pg := Group{Title: "podman + ssh + git (required on every host)"}
	if have("podman") {
		ver := run.Output(ctx, d.Runner, "podman", "--version")
		if ver != "" {
			ver = " (" + ver + ")"
		}
		pg.add(OK, "podman present"+ver)
	} else {
		hint := sudo + pkg + " podman"
		if d.IsMacOS {
			hint += " && podman machine init --cpus 4 --memory 8192 --disk-size 60 --now"
		}
		pg.add(NO, "podman not found — install:  "+hint)
	}
	// On macOS every container runs inside the podman machine VM, so a missing or
	// stopped machine breaks everything downstream — check it before the probes
	// that would otherwise misreport as "not built yet".
	machineUp := true
	if d.IsMacOS && have("podman") {
		var msg string
		machineUp, msg = machineState(ctx, d.Runner)
		if machineUp {
			pg.add(OK, msg)
		} else {
			pg.add(NO, msg)
		}
	}
	if have("ssh") && have("ssh-keygen") {
		pg.add(OK, "ssh client present")
	} else {
		sshPkg := "openssh-clients"
		if apt {
			sshPkg = "openssh-client"
		} else if d.IsMacOS {
			sshPkg = "openssh"
		}
		pg.add(NO, "ssh client not found (needed to reach sandboxes by name) — install:  "+sudo+pkg+" "+sshPkg)
	}
	if have("git") {
		pg.add(OK, "git present")
	} else {
		pg.add(NO, "git not found (needed to share repos with --repo) — install:  "+sudo+pkg+" git")
	}
	r.addGroup(pg)

	// rootless userns. On macOS the containers run inside the podman-machine VM,
	// which maps its own subuid/subgid ranges — there is nothing (and no
	// /etc/subuid, nor a usermod) on the Mac itself.
	ug := Group{Title: "rootless user namespaces (both engines)"}
	switch {
	case d.IsMacOS:
		ug.add(OK, "handled inside the podman machine VM (no host subuid/subgid needed on macOS)")
	case subidPresent(d.User):
		ug.add(OK, "subuid/subgid ranges for "+d.User+" present")
	default:
		ug.add(NO, "no subuid/subgid range for "+d.User+" — add:  sudo usermod --add-subuids 100000-165535 --add-subgids 100000-165535 \""+d.User+"\"")
	}
	r.addGroup(ug)

	// core cs-sandbox state. With the machine down every podman probe fails, so
	// say that rather than claiming nothing is built.
	cg := Group{Title: "cs-sandbox state"}
	if !machineUp {
		cg.add(HM, "image state unknown — podman machine not running (see above)")
	} else if _, err := d.Runner.Run(ctx, run.Opts{ReadOnly: true}, "podman", "image", "exists", d.Image); err == nil {
		cg.add(OK, "image present ("+d.Image+")")
	} else {
		cg.add(HM, "image not built yet — build it with:  cs-sandbox build")
	}
	if fileExists(d.TierDir + "/id_cs-sandbox_user") {
		cg.add(OK, "tier keys generated")
	} else {
		cg.add(HM, "tier keys not generated yet — created automatically on first 'cs-sandbox create'")
	}
	if !machineUp {
		cg.add(HM, "network state unknown — podman machine not running (see above)")
	} else if _, err := d.Runner.Run(ctx, run.Opts{ReadOnly: true}, "podman", "network", "exists", d.Network); err == nil {
		cg.add(OK, "network "+d.Network+" present")
	} else {
		cg.add(HM, "network "+d.Network+" not up yet — created automatically on first 'cs-sandbox create'")
	}
	r.addGroup(cg)

	if engine == "firecracker" && d.IsMacOS {
		// No KVM on a Mac, so none of the host-side microVM checks apply.
		fg := Group{Title: "firecracker microVM engine (Linux/KVM)"}
		fg.add(NO, "firecracker needs a Linux host with KVM — on macOS use:  cs-sandbox doctor --engine podman")
		r.addGroup(fg)
	} else if engine == "firecracker" {
		fg := Group{Title: "firecracker microVM engine (Linux/KVM)"}
		switch {
		case writable("/dev/kvm"):
			fg.add(OK, "/dev/kvm present and writable")
		case fileExists("/dev/kvm"):
			fg.add(NO, "/dev/kvm not writable — add yourself to the kvm group:  sudo usermod -aG kvm \""+d.User+"\"  (re-login)")
		default:
			fg.add(NO, "/dev/kvm missing — needs a Linux host with KVM (virtualization enabled)")
		}
		if miss := missingHostPackages(apt); len(miss) == 0 {
			fg.add(OK, "host packages present (passt, dnsmasq, fakeroot, e2fsprogs, socat, python3, newuidmap, iproute, curl)")
		} else {
			fg.add(NO, "missing host packages — install:  "+sudo+pkg+" "+strings.Join(miss, " "))
		}
		// Report what is actually on disk, not the pin — they diverge after a
		// version bump, and the refresh only happens on the next build.
		switch {
		case d.FCBinPath == "" || !isExecutable(d.FCBinPath):
			fg.add(HM, "firecracker binary not downloaded yet — fetched (SHA256-verified) on first 'create'")
		case d.FCVersionCache == "":
			fg.add(HM, "firecracker binary cached, version unrecorded (downloaded before it was tracked) — re-fetched and digest-verified by:  cs-sandbox build")
		case d.FCVersionCache != d.FCVersionPin:
			fg.add(HM, "firecracker binary cached ("+d.FCVersionCache+") but pinned to "+d.FCVersionPin+" — refreshed by:  cs-sandbox build")
		default:
			fg.add(OK, "firecracker binary cached ("+d.FCVersionCache+")")
		}
		r.addGroup(fg)
	}

	// agent tooling (optional).
	ag := Group{Title: "agent tooling (optional — host-side sign-in that instances inherit)"}
	if have("cs-claude") && have("cs-codex") && have("cs-opencode") {
		ag.add(OK, "agent tools on PATH (cs-claude, cs-codex, cs-opencode)")
	} else {
		ag.add(HM, "agent tools not on PATH — install them:  cs-sandbox install-agent-tools")
	}
	var agentMiss []string
	for _, b := range []string{"claude", "codex", "opencode"} {
		if !have(b) {
			agentMiss = append(agentMiss, b)
		}
	}
	if len(agentMiss) == 0 {
		ag.add(OK, "agent CLIs present (claude, codex, opencode)")
	} else {
		ag.add(HM, "agent CLI(s) not found: "+strings.Join(agentMiss, " ")+" — or sign in inside an instance: cs-sandbox agent-login claude <name>")
	}
	r.addGroup(ag)
	return r
}

// hostPackage maps a required binary to (fedora-pkg, debian-pkg).
type hostPackage struct{ bin, fedora, debian string }

var hostPackages = []hostPackage{
	{"pasta", "passt", "passt"},
	{"dnsmasq", "dnsmasq", "dnsmasq-base"},
	{"fakeroot", "fakeroot", "fakeroot"},
	{"mke2fs", "e2fsprogs", "e2fsprogs"},
	{"e2fsck", "e2fsprogs", "e2fsprogs"},
	{"socat", "socat", "socat"},
	{"python3", "python3", "python3"},
	{"newuidmap", "shadow-utils", "uidmap"},
	{"ip", "iproute", "iproute2"},
	{"curl", "curl", "curl"},
}

// missingHostPackages returns the deduped package names to install for any
// missing required binary.
func missingHostPackages(apt bool) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range hostPackages {
		if have(p.bin) {
			continue
		}
		name := p.fedora
		if apt {
			name = p.debian
		}
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

func (g *Group) add(s Status, msg string) {
	g.Checks = append(g.Checks, Check{Status: s, Message: msg})
}

func (r *Report) addGroup(g Group) {
	for _, c := range g.Checks {
		if c.Status == NO {
			r.Issues++
		}
	}
	r.Groups = append(r.Groups, g)
}

// lookPath is a package var so tests can stub binary presence.
var lookPath = exec.LookPath

func have(bin string) bool { _, err := lookPath(bin); return err == nil }

// machineInit is the suggested machine size — matches INSTALL.md, which sizes
// the VM for several sandboxes at once.
const machineInit = "podman machine init --cpus 4 --memory 8192 --disk-size 60 --now"

// machineState reports whether the default podman machine is running, plus the
// line to show. `podman machine inspect` exits non-zero when no machine exists,
// which is the "never initialized" case rather than "stopped".
func machineState(ctx context.Context, r run.Runner) (up bool, msg string) {
	res, err := r.Run(ctx, run.Opts{ReadOnly: true}, "podman", "machine", "inspect", "--format", "{{.Name}} {{.State}}")
	out := strings.TrimSpace(res.Stdout)
	if err != nil || out == "" {
		return false, "no podman machine — every sandbox runs inside it; create one:  " + machineInit
	}
	// One line per machine; the default is the first (and usually only) one.
	name, state, _ := strings.Cut(firstLine(out), " ")
	if strings.EqualFold(strings.TrimSpace(state), "running") {
		return true, "podman machine running (" + name + ")"
	}
	return false, "podman machine " + name + " is " + strings.TrimSpace(state) +
		" — start it:  podman machine start " + name
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

func subidPresent(user string) bool {
	return grepUserPrefix("/etc/subuid", user) && grepUserPrefix("/etc/subgid", user)
}

func grepUserPrefix(path, user string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	prefix := user + ":"
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func isExecutable(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0
}

func writable(p string) bool {
	f, err := os.OpenFile(p, os.O_WRONLY, 0)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}
