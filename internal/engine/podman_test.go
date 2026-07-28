package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/codesweep-ai/sandbox/internal/hostenv"
	"github.com/codesweep-ai/sandbox/internal/run"
)

func baseParams() runParams {
	return runParams{
		Name: "feature", Type: "agent", Port: 2201, SSHBind: "127.0.0.1", IntPort: 22,
		DNSPrimary: "10.89.0.53", DNSGateway: "10.89.0.1",
		User: "dev", UID: 1000, GID: 1000, Home: "/home/dev", TZ: "America/Los_Angeles",
		HomeVol: "cs-sandbox-home-feature", ContVol: "cs-sandbox-containers-feature",
		SeedDir: "/inst/feature/seed", Image: "localhost/cs-sandbox:44",
	}
}

func TestBuildRunArgsScaledDownCaps(t *testing.T) {
	got := strings.Join(buildRunArgs(baseParams()), " ")
	for _, want := range []string{
		"podman run -d",
		"--name feature --hostname feature",
		"--network cs-sandbox-net",
		"-p 127.0.0.1:2201:22",
		"--dns 10.89.0.53 --dns 10.89.0.1",
		"--cap-add=SYS_ADMIN", "--cap-add=NET_ADMIN", "--cap-add=MKNOD", "--cap-add=SYS_PTRACE",
		"--device /dev/net/tun",
		"--security-opt unmask=/proc/sys",
		"--security-opt label=disable",
		"--sysctl net.ipv4.ping_group_range=1000 1000",
		"--userns=keep-id",
		"--user 0:0",
		"-e CS_SANDBOX_TYPE=agent",
		"-e CS_SANDBOX_UID=1000",
		"-e CS_SANDBOX_HOME=/home/dev",
		"--label cs-sandbox.managed=1",
		"--label cs-sandbox.ssh_port=2201",
		"--label cs-sandbox.name=feature",
		"-v cs-sandbox-home-feature:/home/dev",
		"-v /inst/feature/seed:/run/cs-sandbox-seed:ro",
		"localhost/cs-sandbox:44 sleep infinity",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("argv missing %q\nfull: %s", want, got)
		}
	}
	if strings.Contains(got, "--privileged") {
		t.Error("scaled-down path must NOT use --privileged")
	}
}

// The host group NAME is passed on macOS only; a Linux host leaves the guest's
// existing name for that gid alone.
func TestBuildRunArgsGroupIsMacOSOnly(t *testing.T) {
	if got := strings.Join(buildRunArgs(baseParams()), " "); strings.Contains(got, "CS_SANDBOX_GROUP") {
		t.Errorf("no group name -> no CS_SANDBOX_GROUP:\n%s", got)
	}
	p := baseParams()
	p.Group = "staff"
	if got := strings.Join(buildRunArgs(p), " "); !strings.Contains(got, "-e CS_SANDBOX_GROUP=staff") {
		t.Errorf("group name should be passed through:\n%s", got)
	}
	if got := macOSGroup(hostenv.Host{Group: "staff", IsMacOS: true}); got != "staff" {
		t.Errorf("macOS host should pass its group name, got %q", got)
	}
	if got := macOSGroup(hostenv.Host{Group: "dev"}); got != "" {
		t.Errorf("Linux host should pass no group name, got %q", got)
	}
}

func TestBuildRunArgsPrivileged(t *testing.T) {
	p := baseParams()
	p.Privileged = true
	got := strings.Join(buildRunArgs(p), " ")
	if !strings.Contains(got, "--privileged") {
		t.Error("privileged path must use --privileged")
	}
	if strings.Contains(got, "--cap-add=SYS_ADMIN") {
		t.Error("privileged path must not also add scaled-down caps")
	}
}

func TestBuildRunArgsYoloSoloLabels(t *testing.T) {
	p := baseParams()
	p.Yolo, p.Solo = true, true
	got := strings.Join(buildRunArgs(p), " ")
	if !strings.Contains(got, "--label cs-sandbox.yolo=1") {
		t.Error("yolo label missing")
	}
	if !strings.Contains(got, "--label cs-sandbox.solo=1") {
		t.Error("solo label missing")
	}
	if !strings.Contains(got, "-e CS_SANDBOX_YOLO=1") {
		t.Error("CS_SANDBOX_YOLO=1 env missing")
	}
}

func TestBuildRunArgsEnvStoresMounts(t *testing.T) {
	p := baseParams()
	p.EnvFile = "/inst/box/seed/inject-env"
	p.StoreVols = []string{"cs-sandbox-shared-base:/var/lib/shared/base:ro"}
	p.Stores = []string{"/var/lib/shared/base"}
	p.Mounts = []string{"/host/api:/run/cs-sandbox-repos/api:ro"}
	got := strings.Join(buildRunArgs(p), " ")
	for _, want := range []string{
		"--env-file /inst/box/seed/inject-env",
		"-v cs-sandbox-shared-base:/var/lib/shared/base:ro",
		"-e CS_SANDBOX_IMAGE_STORES=/var/lib/shared/base",
		"-v /host/api:/run/cs-sandbox-repos/api:ro",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("argv missing %q", want)
		}
	}
}

// TestEnvFilePath: injected env resolves to the 0600 seed file; nothing to
// inject means no --env-file at all.
func TestEnvFilePath(t *testing.T) {
	if got := envFilePath("/inst/box/seed", "FOO=bar\n"); got != "/inst/box/seed/inject-env" {
		t.Errorf("envFilePath = %q", got)
	}
	if got := envFilePath("/inst/box/seed", "  \n"); got != "" {
		t.Errorf("envFilePath(blank) = %q, want empty", got)
	}
	p := baseParams()
	p.EnvFile = ""
	if strings.Contains(strings.Join(buildRunArgs(p), " "), "--env-file") {
		t.Error("no injected env should mean no --env-file")
	}
}

// TestBuildRunArgsNoSecretsInArgv: injected values must never reach argv, which
// is world-readable through /proc/<pid>/cmdline while podman runs.
func TestBuildRunArgsNoSecretsInArgv(t *testing.T) {
	p := baseParams()
	p.EnvFile = "/inst/box/seed/inject-env"
	if got := strings.Join(buildRunArgs(p), " "); strings.Contains(got, "sekret") {
		t.Errorf("argv leaked an injected value: %s", got)
	}
}

func TestDNSMasqIP(t *testing.T) {
	if got := dnsmasqIP("10.89.0.1"); got != "10.89.0.53" {
		t.Errorf("dnsmasqIP = %s, want 10.89.0.53", got)
	}
}

func TestHostHostsLine(t *testing.T) {
	if got := hostHostsLine([]string{"box", "box.lan"}); got != "169.254.1.2 box box.lan" {
		t.Errorf("hostHostsLine = %q, want %q", got, "169.254.1.2 box box.lan")
	}
	if got := hostHostsLine(nil); got != "" {
		t.Errorf("hostHostsLine(nil) = %q, want empty", got)
	}
}

func TestWaitReady(t *testing.T) {
	t.Run("ready", func(t *testing.T) {
		d := Deps{Runner: run.NewFake(), StartTimeout: 1}
		if err := d.waitReady(context.Background(), "box"); err != nil {
			t.Fatalf("waitReady: %v", err)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		r := run.NewFake().On("podman exec", run.Result{ExitCode: 1}, errors.New("not ready"))
		d := Deps{Runner: r}
		err := d.waitReady(context.Background(), "box")
		if err == nil || !strings.Contains(err.Error(), "did not become ready") {
			t.Fatalf("waitReady error = %v, want timeout", err)
		}
	})

	t.Run("canceled", func(t *testing.T) {
		r := run.NewFake().On("podman exec", run.Result{ExitCode: 1}, errors.New("not ready"))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		d := Deps{Runner: r, StartTimeout: 60}
		err := d.waitReady(ctx, "box")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waitReady error = %v, want context.Canceled", err)
		}
	})
}

// --snapshot copies with the host cp's spelling of copy-on-write. GNU's
// --reflink=auto is not portable: BSD/macOS cp rejects it ("illegal option --
// -"), which broke --snapshot outright on a Mac.
func TestCopyTreeArgv(t *testing.T) {
	linux := strings.Join(copyTreeArgv(false, "/src", "/dst"), " ")
	if linux != "cp -a --reflink=auto /src /dst" {
		t.Errorf("linux argv = %q", linux)
	}
	mac := strings.Join(copyTreeArgv(true, "/src", "/dst"), " ")
	if mac != "cp -a -c /src /dst" {
		t.Errorf("macOS argv = %q", mac)
	}
	if strings.Contains(mac, "--reflink") {
		t.Errorf("macOS cp has no --reflink: %q", mac)
	}
}

// TestPodmanExecRunsAsDevUser: exec must land as the dev user in their home —
// the container's main process is uid 0, so without --user/--workdir every
// exec'd command would run as root with HOME=/root (a different agent profile,
// root-owned files, and behaviour the firecracker engine doesn't share).
func TestPodmanExecRunsAsDevUser(t *testing.T) {
	f := run.NewFake()
	p := NewPodman(Deps{Runner: f, Host: hostenv.Host{User: "dev"}})

	if err := p.Exec(context.Background(), "box", ExecIO{Argv: []string{"id", "-un"}}); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(f.Calls[0], " ")
	for _, want := range []string{"--user dev", "--workdir /home/dev", "box id -un"} {
		if !strings.Contains(got, want) {
			t.Errorf("exec argv missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, "-it") {
		t.Errorf("a one-shot command should not allocate a TTY: %s", got)
	}

	// An interactive shell adds -it and defaults to a login shell.
	f2 := run.NewFake()
	p2 := NewPodman(Deps{Runner: f2, Host: hostenv.Host{User: "dev"}})
	if err := p2.Exec(context.Background(), "box", ExecIO{Interactive: true}); err != nil {
		t.Fatal(err)
	}
	got2 := strings.Join(f2.Calls[0], " ")
	for _, want := range []string{"-it", "--user dev", "bash -l"} {
		if !strings.Contains(got2, want) {
			t.Errorf("interactive exec argv missing %q: %s", want, got2)
		}
	}
}
