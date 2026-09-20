package cli

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/codesweep-ai/sandbox/internal/engine"
	"github.com/codesweep-ai/sandbox/internal/lend"
	"github.com/codesweep-ai/sandbox/internal/run"
)

// The lender's container, and the reason it is one.
//
// A lender used to be a host process on a fixed port, and that is what made two
// people on one machine collide: 2500 is one port, bound wide because a sandbox
// reaches a host process at the host's ordinary address (SPEC R152). The second
// caller's `create` found something answering there, adopted it, and reported a
// loan the other user's lender would never honour — a 401 on the first model
// call, from a lender that was never theirs.
//
// It runs on the group's own network now. Sandboxes reach it by name over that
// network, nothing is bound on the host, and two groups on one machine cannot
// reach each other's lender because the container has no interface on any
// network but its own. The boundary is a namespace rather than a rule about who
// may call, which is what makes it hold for two runners under ONE account as
// well as for two accounts.
//
// The trust model is unchanged. The credential still lives in the host's
// filesystem and is read per request (R150); the mount is a view of the same
// file, not a copy of it. The binary is the image's own unless one is handed in
// from the host — see lenderBinary.
//
// Lives here rather than in internal/lend because that package is held to
// importing nothing else in the repository: everything it needs arrives as a
// parameter, and a podman command line is not that.

// lenderBox is one group's lender container.
type lenderBox struct {
	Runner run.Runner
	Spec   lenderBoxSpec
}

// lenderBoxSpec is what the container is built from. The paths are HOST paths,
// mounted at the same location inside, so nothing is translated on the way in
// and a log line means the same thing on both sides.
type lenderBoxSpec struct {
	Network string // the group's podman network
	Image   string // the sandbox image, which every group already has
	Home    string // the host home holding the ~/.cs-<agent> profiles and .cs-keys
	InstDir string // the instances root, where the loans are
	// Bin is a Linux cs-sandbox to run instead of the image's own. It is not
	// optional in every case: the slim CI image drops the whole codesweep-tools
	// stanza, so there is no cs-sandbox in it at all.
	// LogDir is where the container's log is kept when the container is removed.
	// Empty keeps nothing.
	LogDir string
	Bin    string
	// Stage is where Bin is copied before the container starts, and what the
	// container then runs.
	//
	// A copy rather than a mount of the live file, and the reason is mundane but
	// real: a bind-mounted executable is mapped in the container, so writing to
	// it fails with ETXTBSY. `go build -o bin/cs-sandbox` would then fail for as
	// long as any lent sandbox was up, which is exactly when somebody is
	// iterating on the lender.
	//
	// It sits under InstDir, which is already mounted read-only at the same
	// path, so the copy needs no mount of its own and the container's view of it
	// is the host's path.
	Stage string
}

// name is this box's container name.
func (b lenderBox) name() string { return lend.BoxName(b.Spec.Network) }

// guestBase is the base URL a sandbox on this network reaches the lender at.
func (b lenderBox) guestBase() string { return lend.GuestURL(lend.GuestName, lend.DefaultBind) }

// lenderBoxArgv is the `podman run` command, pure so a golden test can pin it.
//
// --entrypoint skips the image's own, which exists to bring up a SANDBOX: a dev
// user, sshd, the nested engine. None of that belongs in a process that answers
// one HTTP port, and skipping it is most of why this container is serving in
// well under a second.
//
// --security-opt label=disable for the same reason the group gateway uses it:
// the mounts are host directories owned by the invoking user, and under SELinux
// the container's read is denied without either this or a relabel — and
// relabelling somebody's home directory is not something a tool may do.
func lenderBoxArgv(name string, s lenderBoxSpec) []string {
	a := []string{"podman", "run", "-d",
		"--name", name,
		"--hostname", lend.GuestName,
		"--network", s.Network,
		"--network-alias", lend.GuestName,
		"--restart=always",
		"--label", "cs-sandbox.managed=1",
		"--label", "cs-sandbox.lender=1",
		"--security-opt", "label=disable",
		// The lender resolves its own paths from these, so the container reads
		// exactly the files the host process would have read.
		"-e", "HOME=" + s.Home,
		"-e", "CS_SANDBOX_AGENT_HOME=" + s.Home,
		"-e", "CS_SANDBOX_INSTANCES_DIR=" + s.InstDir,
		// Read-only, both. The lender writes nothing: it reads a credential per
		// request and a loan table that create and destroy own (R150, R151).
		"-v", s.Home + ":" + s.Home + ":ro",
		"-v", s.InstDir + ":" + s.InstDir + ":ro",
	}
	entry := "cs-sandbox"
	if s.Bin != "" {
		entry = s.Stage
	}
	return append(a, "--entrypoint", entry, s.Image,
		"lender", "--addr", lend.DefaultBind, "--callers", string(lend.CallersNetwork))
}

// canRead reports whether the running lender can read a path, asked from inside
// the container rather than from here.
//
// The frame of reference is the whole point. The lender bind-mounts the agent
// home and the instances root and nothing else, so a path the host resolves
// perfectly well can be absent over there — a symlink pointing out of the
// mounted tree is the case that costs the most, because it dangles inside the
// container while every host-side check passes.
//
// `test -r` rather than a read: this asks whether the file is there and
// readable, and must never move the credential itself (R149, R150).
func (b lenderBox) canRead(ctx context.Context, path string) error {
	if _, err := b.Runner.Run(ctx, run.Opts{ReadOnly: true},
		"podman", "exec", b.name(), "test", "-r", path); err != nil {
		return classifyRead(path, err)
	}
	return nil
}

// classifyRead tells `test` answering no from podman failing to ask.
//
// Exit 1 is `test` itself, and it is the only answer that says anything about
// the path. Everything else is podman unable to put the question — no such
// container, the container removed mid-exec, no podman at all — and reporting
// those as an unreadable credential sends the reader to inspect a file that was
// never the problem. Measured: a lender removed beside a running create reported
// itself as a symlink pointing out of the mounted tree, in an agent home that
// held no symlink.
func classifyRead(path string, err error) error {
	var exit *run.ExitError
	if errors.As(err, &exit) && exit.ExitCode == 1 {
		return errUnreadable
	}
	return fmt.Errorf("asking the lender to read %s: %w", path, err)
}

// errUnreadable is `test -r` answering no: the lender is there and answered, and
// the path is the thing at fault.
var errUnreadable = errors.New("the lender container cannot read it")

// ensure brings the lender up for this network and returns the base URL a
// sandbox reaches it at.
//
// Idempotent, and it adopts rather than churns: a running box is used as it is,
// a stopped one is started, and only an absent one is created. That matters
// because every create in a group calls this.
func (b lenderBox) ensure(ctx context.Context) (string, error) {
	if b.running(ctx) {
		return b.serving(ctx), nil
	}
	if b.exists(ctx) {
		if _, err := b.Runner.Run(ctx, run.Opts{}, "podman", "start", b.name()); err == nil {
			if err := b.waitReady(ctx); err == nil {
				return b.serving(ctx), nil
			}
		}
		// A box that will not start again is worse than no box: it holds the
		// name and the alias while answering nothing.
		_, _ = b.Runner.Run(ctx, run.Opts{}, "podman", "rm", "-f", b.name())
	}
	if err := b.stage(); err != nil {
		return "", err
	}
	res, err := b.Runner.Run(ctx, run.Opts{}, lenderBoxArgv(b.name(), b.Spec)...)
	if err != nil {
		// A concurrent create may have won the race between the checks above
		// and this command — several cells of a matrix start together, and two
		// people's `create` can land in the same second. Podman refuses the
		// second one on the name, which is the right answer to the wrong
		// question: what the caller wants is a lender on this network, and
		// there is now one. Adopt it if it serves.
		if b.exists(ctx) {
			if werr := b.waitReady(ctx); werr == nil {
				return b.serving(ctx), nil
			}
		}
		d := strings.TrimSpace(res.Stderr)
		if d != "" {
			d = "\n  " + d
		}
		hint := ""
		if b.Spec.Bin == "" {
			hint = "\n  the image supplies the lender's binary when CS_SANDBOX_LENDER_BIN names none, " +
				"and a slimmed image carries no cs-sandbox: point that variable at a Linux build of it"
		}
		return "", fmt.Errorf("start the credential lender on %s: %w%s%s", b.Spec.Network, err, d, hint)
	}
	if err := b.waitReady(ctx); err != nil {
		return "", err
	}
	return b.serving(ctx), nil
}

// serving is the address callers get once the box is up, and the one place the
// interface it answers on is checked.
func (b lenderBox) serving(ctx context.Context) string {
	b.alignMTU(ctx)
	return b.guestBase()
}

// alignMTU makes the lender's interface match the bridge it is plugged into.
//
// The value is inferred at attach, and on a rootless host the inference can be
// wrong: the first container onto a bridge takes pasta's 65520 uplink MTU while
// the bridge itself comes up at 1500. Nothing reports the mismatch. Every small
// packet still passes, so DNS resolves, TCP connects and plain HTTP answers 200
// — and the first packet over 1500 bytes is dropped in silence. A TLS
// ClientHello is the first thing that big, so the lender accepts a loan, swaps
// in the real credential, and then cannot finish a handshake with the provider
// it fronts. What reaches a person is an agent retrying "API error" against a
// credential that was never wrong.
//
// networkCreateArgv pins the MTU, so a network this version created cannot
// produce the mismatch. This is for the ones earlier versions created: they
// carry no MTU option, they are still in use, and the bridge cannot be repaired
// without recreating the network — which is not something starting a lender may
// do to a running group. Setting it on the container needs no restart and is a
// no-op when the value is already right.
//
// Best effort, and deliberately not an error. A host where this cannot run is
// not one where create should fail: the lender is often fine, and a lender that
// is not says so on its first request.
func (b lenderBox) alignMTU(ctx context.Context) {
	res, err := b.Runner.Run(ctx, run.Opts{ReadOnly: true},
		"podman", "inspect", b.name(), "--format", "{{.State.Pid}}")
	if err != nil {
		return
	}
	pid := strings.TrimSpace(res.Stdout)
	if pid == "" || pid == "0" {
		return
	}
	_, _ = b.Runner.Run(ctx, run.Opts{}, "podman", "unshare",
		"nsenter", "-t", pid, "-n", "ip", "link", "set", "dev", "eth0", "mtu", engine.BridgeMTU)
}

// stage copies the binary the container will run into place.
//
// Written beside its destination and renamed onto it, which is what makes this
// safe while an older lender is still running: rename replaces the name without
// touching the inode the running container has mapped. Truncating in place
// would fail with ETXTBSY, which is the whole failure this exists to avoid.
//
// Each call writes under a temporary name of its own. Every create in a group
// stages while the lender is down, and a matrix starts several at once. With one
// shared name, one create's rename took the file another was still writing or
// about to rename (SBX-040). Now each renames only the complete copy it wrote,
// and the last rename wins with the same bytes.
func (b lenderBox) stage() error {
	if b.Spec.Bin == "" {
		return nil
	}
	src, err := os.ReadFile(b.Spec.Bin)
	if err != nil {
		return fmt.Errorf("read the lender binary %s: %w", b.Spec.Bin, err)
	}
	dir := filepath.Dir(b.Spec.Stage)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(b.Spec.Stage)+".*.new")
	if err != nil {
		return err
	}
	_, err = tmp.Write(src)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), 0o755)
	}
	if err == nil {
		err = os.Rename(tmp.Name(), b.Spec.Stage)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return nil
}

func (b lenderBox) running(ctx context.Context) bool {
	out := run.Output(ctx, b.Runner, "podman", "container", "inspect", b.name(),
		"--format", "{{.State.Running}}")
	return strings.TrimSpace(out) == "true"
}

func (b lenderBox) exists(ctx context.Context) bool {
	_, err := b.Runner.Run(ctx, run.Opts{ReadOnly: true}, "podman", "container", "exists", b.name())
	return err == nil
}

// waitReady blocks until the lender answers its own health endpoint.
//
// Asked from INSIDE the container, with the curl the image is guaranteed to
// carry, because there is no address left that the host could dial. That is the
// design rather than a limitation of the probe: a lender nothing off the
// network can reach is a lender nothing off the network can reach.
func (b lenderBox) waitReady(ctx context.Context) error {
	deadline := time.Now().Add(lenderBoxReady)
	var last string
	for time.Now().Before(deadline) {
		if why := b.probe(ctx, "http://"+lend.ProbeAddr(lend.DefaultBind)+"/healthz"); why == "" {
			return nil
		} else {
			last = why
		}
		if !b.running(ctx) {
			return fmt.Errorf("the credential lender container stopped as it started:\n%s",
				strings.TrimSpace(run.Output(ctx, b.Runner, "podman", "logs", "--tail", "20", b.name())))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return fmt.Errorf("the credential lender on %s never answered: %s", b.Spec.Network, last)
}

// probe asks the lender's container to fetch a URL, and reports why it could
// not, or "" when it could. It is for the lender's own health endpoint, where
// anything but a 200 is a failure. An upstream is asked with reach.
func (b lenderBox) probe(ctx context.Context, url string) string {
	res, err := b.Runner.Run(ctx, run.Opts{ReadOnly: true}, "podman", "exec", b.name(),
		"curl", "-fsS", "-o", "/dev/null", "-m", "5", url)
	if err == nil {
		return ""
	}
	if d := strings.TrimSpace(res.Stderr); d != "" {
		return d
	}
	return err.Error()
}

// reach reports why the lender cannot connect to an upstream, or "" when it
// can.
//
// Asked from the lender's container, because the lender is what dials an
// upstream, so the only question worth asking is whether the lender can reach
// it. A probe from the host answers a different question.
//
// It opens a connection and sends nothing. Whether the lender can reach the
// address is settled once the connection opens, and whatever status a request
// would draw says nothing more about that: a provider's base address answers a
// bare GET with a 401 or a 404 when it is perfectly healthy. A request is also
// not free. An upstream that records or replays traffic counts every one it
// receives, and a probe it never recorded fails a replay that was passing.
func (b lenderBox) reach(ctx context.Context, upstream string) string {
	u, err := url.Parse(upstream)
	if err != nil || u.Hostname() == "" {
		return "it is not a URL with a host"
	}
	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	// The host and port travel as arguments, never inside the script.
	res, err := b.Runner.Run(ctx, run.Opts{ReadOnly: true}, "podman", "exec", b.name(),
		"timeout", "5", "bash", "-c", `exec 3<>"/dev/tcp/$1/$2"`, "reach", u.Hostname(), port)
	if err == nil {
		return ""
	}
	if res.ExitCode == 124 {
		return "no connection within 5s"
	}
	// The first line carries the reason, after bash's own prefix. A name that does
	// not resolve is followed by a second line that says only "Invalid argument".
	if why, _, _ := strings.Cut(strings.TrimSpace(res.Stderr), "\n"); why != "" {
		if i := strings.LastIndex(why, ": "); i >= 0 {
			why = why[i+2:]
		}
		return strings.ToLower(why)
	}
	return err.Error()
}

// stop removes the box. Removed rather than stopped: it holds a network alias,
// and a stopped container that still owns the name is one more state the next
// ensure would have to reason about.
func (b lenderBox) stop(ctx context.Context) error {
	if !b.exists(ctx) {
		return nil
	}
	// Stopped first, then read: the lender prints what each slot's upstream
	// answered as it exits, and that summary is the first thing a reader of the
	// kept log wants.
	_, _ = b.Runner.Run(ctx, run.Opts{}, "podman", "stop", "-t", "5", b.name())
	b.keepLog(ctx)
	_, err := b.Runner.Run(ctx, run.Opts{}, "podman", "rm", "-f", b.name())
	return err
}

// keepLog copies the container's log to the host before the container goes.
//
// The lender's log is the one record of what every provider answered every
// member of the group, and podman deletes it with the container. It is written
// from out here because the lender's own mounts are read-only, and they stay
// that way: a process that holds real credentials writes nothing (R150, R151).
//
// Best effort. A log that could not be kept is not a reason to leave a lender
// running.
func (b lenderBox) keepLog(ctx context.Context) {
	if b.Spec.LogDir == "" {
		return
	}
	res, err := b.Runner.Run(ctx, run.Opts{ReadOnly: true}, "podman", "logs", "--timestamps", b.name())
	// The lender logs to stderr and prints its closing summary to stdout, and
	// podman keeps the two streams apart. Stderr first puts the summary last,
	// where it happened.
	text := res.Stderr + res.Stdout
	if err != nil || strings.TrimSpace(text) == "" {
		return
	}
	if err := os.MkdirAll(b.Spec.LogDir, 0o700); err != nil {
		return
	}
	name := time.Now().UTC().Format("20060102T150405Z") + ".log"
	if err := os.WriteFile(filepath.Join(b.Spec.LogDir, name), []byte(text), 0o600); err != nil {
		return
	}
	// The newest few, so a host that makes and removes groups all day does not
	// fill a disk with them. The names sort by time.
	kept, _ := filepath.Glob(filepath.Join(b.Spec.LogDir, "*.log"))
	slices.Sort(kept)
	for len(kept) > lenderLogsKept {
		_ = os.Remove(kept[0])
		kept = kept[1:]
	}
	// LogDir is <logs>/<root>/<group>, and the count above only ever visits the
	// group being stopped. A root or a group that is gone is never stopped again,
	// so its logs are aged out from here, by whichever lender stops next.
	sweepLenderLogs(filepath.Dir(filepath.Dir(b.Spec.LogDir)), time.Now().Add(-lenderLogsMaxAge))
}

// sweepLenderLogs removes every kept log older than the cutoff, and then the
// group and root directories that emptied.
func sweepLenderLogs(root string, cutoff time.Time) {
	logs, _ := filepath.Glob(filepath.Join(root, "*", "*", "*.log"))
	for _, l := range logs {
		if info, err := os.Stat(l); err == nil && info.ModTime().Before(cutoff) {
			_ = os.Remove(l)
		}
	}
	// Groups before roots. os.Remove refuses a directory that still holds
	// something, which is the whole check.
	for _, pattern := range []string{filepath.Join(root, "*", "*"), filepath.Join(root, "*")} {
		dirs, _ := filepath.Glob(pattern)
		for _, d := range dirs {
			_ = os.Remove(d)
		}
	}
}

// lenderLogsKept is how many of a group's lender logs stay on the host.
const lenderLogsKept = 20

// lenderLogsMaxAge is how long a kept log stays. Long enough to come back to a
// run that went wrong last week, short enough that throwaway roots and groups
// do not collect.
const lenderLogsMaxAge = 14 * 24 * time.Hour

// lenderBoxReady is a var only so a test can shorten it.
var lenderBoxReady = 20 * time.Second
