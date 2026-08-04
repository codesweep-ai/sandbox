// Package forward manages host<->sandbox port forwards as tracked, detached
// `ssh -N -L/-D` processes over the published SSH port (engine-agnostic). Each
// forward is recorded under instances/<group>/<name>/forwards/<hostport> so it
// can be listed and torn down. The detached ssh children are supervised directly via
// os/exec + Setpgid + pid files rather than the capturing Runner, which is for
// commands that run to completion.
package forward

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/codesweep-ai/sandbox/internal/hostcfg"
	"github.com/codesweep-ai/sandbox/internal/hostenv"
	"github.com/codesweep-ai/sandbox/internal/ports"
	"github.com/codesweep-ai/sandbox/internal/state"
)

// Record is one active forward.
type Record struct {
	PID      int
	Kind     string // "L" (local) or "D" (SOCKS)
	HostPort int
	Target   string
	Bind     string
}

// dir is the instance's own forwards directory. It takes (group, name) rather
// than a user reference on purpose: a reference is whatever the caller typed,
// and keying on it put records at <instances>/<ref>/forwards — beside the group
// directories, owned by nothing, so `group rm` could not reclaim them and a
// destroy spelled differently could not find them.
func dir(instDir, group, name string) string {
	return filepath.Join(state.Dir(instDir, group, name), "forwards")
}

// legacyDir is where records landed before they were keyed on identity. Swept
// on teardown so forwards started by an older build are still killed, and their
// stray directory reclaimed, instead of leaking a live ssh process that nothing
// lists. Remove once no such directory can plausibly remain.
func legacyDir(instDir, ref string) string {
	return filepath.Join(instDir, ref, "forwards")
}

// Start launches a forward. kind "L" needs target "host:port"; kind "D" is a
// SOCKS proxy.
func Start(h hostenv.Host, tierDir, instDir, group, name string, port int, kind string, hostPort int, target, bind string) (*Record, error) {
	d := dir(instDir, group, name)
	if err := os.MkdirAll(d, 0o700); err != nil {
		return nil, err
	}
	rec := filepath.Join(d, strconv.Itoa(hostPort))
	if _, err := os.Stat(rec); err == nil {
		return nil, fmt.Errorf("host port %d already forwarded for %s (unforward it first)", hostPort, name)
	}
	if portBusy(hostPort) {
		return nil, fmt.Errorf("host port %d is already in use", hostPort)
	}

	args := forwardArgs(h, tierDir, group, name, port, kind, hostPort, target, bind)

	logf, err := os.Create(rec + ".log")
	if err != nil {
		return nil, err
	}
	cmd := exec.Command("ssh", args...)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // detach into its own group
	if err := cmd.Start(); err != nil {
		logf.Close()
		return nil, err
	}
	_ = logf.Close()
	pid := cmd.Process.Pid
	// Release the child so it keeps running after we return.
	_ = cmd.Process.Release()

	time.Sleep(600 * time.Millisecond)
	if !alive(pid) {
		tail, _ := os.ReadFile(rec + ".log")
		_ = os.Remove(rec + ".log")
		return nil, fmt.Errorf("forward %d failed: %s", hostPort, strings.TrimSpace(lastLine(string(tail))))
	}
	r := &Record{PID: pid, Kind: kind, HostPort: hostPort, Target: target, Bind: bind}
	if err := writeRecord(rec, r); err != nil {
		kill(pid)
		_ = os.Remove(rec + ".log")
		return nil, err
	}
	return r, nil
}

// forwardArgs builds the `ssh` argv for a forward: the connection options, then
// -N (no command) + fail-fast batch flags, then -L host:port:target (local) or
// -D bind:port (SOCKS), then the login target.
//
// It takes (group, name) rather than a bare name so the connection is keyed by
// the host-global object name, as every other managed connection is. A bare
// HostKeyAlias is not unique: the same fixture in two groups is what groups are
// for, and both would claim the one known_hosts entry, so the second forward
// died on "host key changed" with BatchMode leaving nobody to answer.
func forwardArgs(h hostenv.Host, tierDir, group, name string, port int, kind string, hostPort int, target, bind string) []string {
	args := hostcfg.SSHOptions(h, tierDir, state.ObjectName(group, name), port)
	args = append(args, "-N", "-o", "ExitOnForwardFailure=yes", "-o", "BatchMode=yes")
	if kind == "D" {
		args = append(args, "-D", fmt.Sprintf("%s:%d", bind, hostPort))
	} else {
		args = append(args, "-L", fmt.Sprintf("%s:%d:%s", bind, hostPort, target))
	}
	return append(args, fmt.Sprintf("%s@127.0.0.1", h.User))
}

// List returns the live forwards for an instance, GC-ing dead ones.
func List(instDir, group, name string) ([]Record, error) {
	d := dir(instDir, group, name)
	entries, err := os.ReadDir(d)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Record
	for _, e := range entries {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		p := filepath.Join(d, e.Name())
		r, err := readRecord(p)
		if err != nil {
			continue
		}
		if !alive(r.PID) {
			_ = os.Remove(p)
			_ = os.Remove(p + ".log")
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].HostPort < out[j].HostPort })
	return out, nil
}

// Remove tears down forwards matching target ("all" or a host port).
func Remove(instDir, group, name, target string) (int, error) {
	recs, err := List(instDir, group, name)
	if err != nil {
		return 0, err
	}
	d := dir(instDir, group, name)
	n := 0
	for _, r := range recs {
		if target != "all" && target != strconv.Itoa(r.HostPort) {
			continue
		}
		kill(r.PID)
		p := filepath.Join(d, strconv.Itoa(r.HostPort))
		_ = os.Remove(p)
		_ = os.Remove(p + ".log")
		n++
	}
	_ = os.Remove(d) // rmdir if empty
	return n, nil
}

// KillAll tears down every forward for an instance (used by stop/rm/destroy).
// It also sweeps the pre-identity location, so an upgrade does not strand a
// running ssh process that no command can see any more.
func KillAll(instDir, group, name string) {
	_, _ = Remove(instDir, group, name, "all")
	sweepLegacy(instDir, state.ObjectName(group, name))
	if group == state.DefaultGroup {
		sweepLegacy(instDir, name) // a default-group member was addressed bare
	}
}

// sweepLegacy kills anything recorded at the old path for ref and reclaims the
// directory. Records there predate identity-keyed forwards; they are only ever
// removed, never written.
func sweepLegacy(instDir, ref string) {
	d := legacyDir(instDir, ref)
	entries, err := os.ReadDir(d)
	if err != nil {
		return
	}
	for _, e := range entries {
		p := filepath.Join(d, e.Name())
		if r, err := readRecord(p); err == nil {
			kill(r.PID)
		}
		_ = os.Remove(p)
	}
	_ = os.Remove(d)
	_ = os.Remove(filepath.Dir(d)) // the stray <instances>/<ref>/ itself, if now empty
}

func writeRecord(path string, r *Record) error {
	var b strings.Builder
	fmt.Fprintf(&b, "pid=%d\nkind=%s\nhostport=%d\ntarget=%s\nbind=%s\n", r.PID, r.Kind, r.HostPort, r.Target, r.Bind)
	return os.WriteFile(path, []byte(b.String()), 0o600)
}

func readRecord(path string) (Record, error) {
	f, err := os.Open(path)
	if err != nil {
		return Record{}, err
	}
	defer f.Close()
	var r Record
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			continue
		}
		switch k {
		case "pid":
			r.PID, _ = strconv.Atoi(v)
		case "kind":
			r.Kind = v
		case "hostport":
			r.HostPort, _ = strconv.Atoi(v)
		case "target":
			r.Target = v
		case "bind":
			r.Bind = v
		}
	}
	return r, sc.Err()
}

func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

func kill(pid int) {
	if pid > 0 {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
}

func portBusy(p int) bool { return ports.LoopbackBusy(p) }

func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return lines[len(lines)-1]
}
