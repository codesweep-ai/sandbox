package lend

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The lender is the one long-lived process this tool has, and it is supervised
// the way the port forwards already are: a detached child under its own process
// group, with a record beside it holding the pid and the address.
//
// The record sits at the instances root rather than under one instance, because
// one lender serves every sandbox on the host. Nothing daemonizes itself — the
// child runs in the foreground and the caller detaches it — which is what lets
// the same command work under systemd, under launchd, and in a terminal.

// DefaultPort is where the lender listens. It sits above the ranges R42
// allocates from (2200-2399 sandboxes, 2400-2499 group gateways), so a lender
// and a sandbox can never claim the same port.
const DefaultPort = 2500

// DefaultBind is the listen address.
//
// Not loopback, and it cannot be: a sandbox reaches the host at 169.254.1.2,
// which arrives on the host's ordinary side, where a server bound to 127.0.0.1
// refuses the connection (R52). Which of the host's addresses that is depends on
// the network the host is on and changes when it moves, so the lender binds all
// of them and refuses every caller that is not this host.
var DefaultBind = fmt.Sprintf("0.0.0.0:%d", DefaultPort)

// Daemon supervises the host's lender process from the instances root.
type Daemon struct{ Dir string }

func (d Daemon) recordPath() string { return filepath.Join(d.Dir, "lender") }

// LogPath is where a detached lender's output goes, so a start that failed can
// be explained rather than merely reported.
func (d Daemon) LogPath() string { return filepath.Join(d.Dir, "lender.log") }

// Status returns the recorded pid and address, and whether that process is
// still alive.
func (d Daemon) Status() (pid int, addr string, alive bool) {
	f, err := os.Open(d.recordPath())
	if err != nil {
		return 0, "", false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			continue
		}
		switch k {
		case "pid":
			pid, _ = strconv.Atoi(v)
		case "addr":
			addr = v
		}
	}
	return pid, addr, pid > 0 && syscall.Kill(pid, 0) == nil
}

// Start launches a detached lender and waits for it to answer.
//
// exe and args are the caller's own command line, so this package names no
// subcommand of a CLI it knows nothing about.
func (d Daemon) Start(exe string, args []string, addr, probeAddr string) error {
	if err := os.MkdirAll(d.Dir, 0o700); err != nil {
		return err
	}
	logf, err := os.OpenFile(d.LogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, args...)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // detach into its own group
	if err := cmd.Start(); err != nil {
		logf.Close()
		return err
	}
	_ = logf.Close()
	pid := cmd.Process.Pid
	_ = cmd.Process.Release() // keeps running after we return

	// Wait for the port rather than for the process: a lender that started and
	// then failed to bind is a create that would otherwise report success and
	// hand back a sandbox whose first model call is refused.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := Probe(probeAddr); err == nil {
			return d.writeRecord(pid, addr)
		}
		if syscall.Kill(pid, 0) != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGTERM)
	tail, _ := os.ReadFile(d.LogPath())
	why := lastLine(string(tail))
	if why == "" {
		why = "it wrote nothing to " + d.LogPath()
	}
	return fmt.Errorf("the lender did not come up on %s: %s", addr, why)
}

func (d Daemon) writeRecord(pid int, addr string) error {
	p := d.recordPath()
	if err := os.WriteFile(p, fmt.Appendf(nil, "pid=%d\naddr=%s\n", pid, addr), 0o600); err != nil {
		return err
	}
	return os.Chmod(p, 0o600)
}

// Stop ends the lender and clears its record. Stopping one that is not running
// is not an error: the caller wants it gone, and it is.
//
// The recorded pid is never signalled on its own. A pid file can outlive the
// process it names — a host reboot is enough — and the number is then whatever
// the kernel handed out next, so a signal sent on that evidence goes to a
// stranger. The address is asked first, and only a pid whose port answers as a
// lender is signalled.
func (d Daemon) Stop() error {
	pid, addr, alive := d.Status()
	if alive && addr != "" && Probe(ProbeAddr(addr)) == nil {
		if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
			return err
		}
	}
	if err := os.Remove(d.recordPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Probe reports whether a lender is answering at this address. It asks for the
// health endpoint rather than opening a socket, because something else holding
// the port is exactly the case worth telling apart.
func Probe(addr string) error {
	c := &http.Client{Timeout: 2 * time.Second}
	res, err := c.Get("http://" + addr + "/healthz")
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("something is listening on %s but it is not a lender (HTTP %d)", addr, res.StatusCode)
	}
	return nil
}

// ProbeAddr turns a bind address into one that can be dialled. A wildcard bind
// is not a destination, so the health check goes to loopback on the same port.
func ProbeAddr(bind string) string {
	host, port, err := net.SplitHostPort(bind)
	if err != nil {
		return bind
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		return net.JoinHostPort("127.0.0.1", port)
	}
	return bind
}

// GuestURL is the base URL a sandbox is given: the host as seen from inside,
// and the port the lender listens on. guestHost is whatever the guest reaches
// this host by, a name as readily as an address.
func GuestURL(guestHost, bind string) string {
	_, port, err := net.SplitHostPort(bind)
	if err != nil {
		port = strconv.Itoa(DefaultPort)
	}
	return "http://" + net.JoinHostPort(guestHost, port)
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
