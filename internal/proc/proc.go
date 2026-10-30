// Package proc decides whether a recorded pid is still the process this tool
// started, and signals it only when it is.
//
// A pid alone is not an identity. Records outlive a host reboot, and the kernel
// hands numbers out again, so a stale record can name an unrelated process of
// the same user, which signal 0 calls alive and SIGTERM kills. A process is
// taken as the recorded one only when the pid exists, its start time matches the
// recorded one, and its argv passes the caller's check. The signal then goes
// through a pidfd opened before that check, so the check and the signal refer
// to the same process.
package proc

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

// Ident is a process as a record names it.
type Ident struct {
	PID int
	// Start is field 22 of /proc/<pid>/stat, in clock ticks since boot. It is
	// empty in a record written before start times were kept, and where the
	// host has no /proc.
	Start string
}

// Of is the identity of a process this caller has just started.
func Of(pid int) Ident {
	s, _ := statOf(pid)
	return Ident{PID: pid, Start: s.start}
}

// Match reports whether a process's argv is that of the process recorded.
type Match func(argv []string) bool

// Handle is an open reference to a process verified to be the recorded one.
type Handle struct {
	id    Ident
	match Match
	fd    int // a pidfd, or -1 where the kernel offers none
	// verified is whether the start time matched rather than the argv alone,
	// which is the bar a SIGKILL has to clear.
	verified bool
}

// Find returns a handle on the recorded process, or nil when that process is
// gone or the pid now belongs to another one. A record with no start time is
// judged by its argv alone, and its handle reports Verified false.
func Find(id Ident, match Match) *Handle {
	if id.PID <= 0 {
		return nil
	}
	h := &Handle{id: id, match: match, fd: openFD(id.PID)}
	if h.fd == errGone {
		return nil
	}
	if !h.ours() {
		h.Close()
		return nil
	}
	h.verified = id.Start != "" && hasProc()
	return h
}

// Alive reports whether the recorded process is running. It is Find without
// keeping the handle.
func Alive(id Ident, match Match) bool {
	h := Find(id, match)
	if h == nil {
		return false
	}
	h.Close()
	return true
}

// Verified reports whether the process matched on its start time as well as
// its argv.
func (h *Handle) Verified() bool { return h.verified }

// Running reports whether the process is still running. A zombie is not.
func (h *Handle) Running() bool { return h.ours() }

// Signal sends sig to the process. Without a pidfd it checks the identity
// again first, which narrows the window a reused pid could slip through but
// cannot close it.
func (h *Handle) Signal(sig syscall.Signal) error {
	if h.fd >= 0 {
		return sendFD(h.fd, sig)
	}
	if !h.ours() {
		return syscall.ESRCH
	}
	return syscall.Kill(h.id.PID, sig)
}

// Close releases the pidfd.
func (h *Handle) Close() {
	if h.fd >= 0 {
		closeFD(h.fd)
		h.fd = -1
	}
}

// ours reports whether the pid is still the recorded, running process. With a
// pidfd, a signal 0 through it comes last: it succeeds only while the process
// the fd refers to still holds the pid, so the /proc reads before it were
// about that process.
func (h *Handle) ours() bool {
	if hasProc() {
		s, ok := statOf(h.id.PID)
		if !ok || s.state == 'Z' || s.state == 'X' {
			return false
		}
		if h.id.Start != "" && s.start != h.id.Start {
			return false
		}
		if h.match != nil {
			argv, ok := argvOf(h.id.PID)
			if !ok || !h.match(argv) {
				return false
			}
		}
	}
	if h.fd >= 0 {
		return sendFD(h.fd, 0) == nil
	}
	return syscall.Kill(h.id.PID, 0) == nil
}

// stat is the part of /proc/<pid>/stat this package reads.
type stat struct {
	state byte
	start string
}

// parseStat reads the state (field 3) and start time (field 22) out of a
// /proc/<pid>/stat line. The comm field can hold spaces and parentheses, so
// the fields are counted from the last ')'.
func parseStat(line string) (stat, bool) {
	_, rest, found := strings.CutLast(line, ")")
	if !found {
		return stat{}, false
	}
	f := strings.Fields(rest)
	// f[0] is field 3, so field 22 is f[19].
	if len(f) < 20 || len(f[0]) != 1 {
		return stat{}, false
	}
	return stat{state: f[0][0], start: f[19]}, true
}

// ReadFile reads a pid file: the pid on its first line, and the start time on
// its second where the writer recorded one.
func ReadFile(path string) (Ident, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Ident{}, err
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	pid, err := strconv.Atoi(strings.TrimSpace(lines[0]))
	if err != nil {
		return Ident{}, err
	}
	id := Ident{PID: pid}
	if len(lines) > 1 {
		id.Start = strings.TrimSpace(lines[1])
	}
	return id, nil
}

// WriteFile records id in the format ReadFile reads.
func WriteFile(path string, id Ident) error {
	rec := strconv.Itoa(id.PID) + "\n"
	if id.Start != "" {
		rec += id.Start + "\n"
	}
	return os.WriteFile(path, []byte(rec), 0o644)
}

// VMMPidFile is the file in a microVM's instance directory that records its
// launcher.
const VMMPidFile = "fc.pid"

// VMMConfig is the instance file a microVM boots from, named on its argv.
const VMMConfig = "run.json"

// VMMArgv matches the argv of a microVM launched from instance directory idir:
// it names that directory's config file. A relative instances directory is
// also matched in its absolute form.
func VMMArgv(idir string) Match {
	want := []string{filepath.Join(idir, VMMConfig)}
	if abs, err := filepath.Abs(want[0]); err == nil && abs != want[0] {
		want = append(want, abs)
	}
	return func(argv []string) bool {
		return slices.ContainsFunc(argv, func(a string) bool { return slices.Contains(want, a) })
	}
}

// FindVMM returns a handle on the microVM launched from idir, or nil when none
// is running there.
func FindVMM(idir string) *Handle {
	id, err := ReadFile(filepath.Join(idir, VMMPidFile))
	if err != nil {
		return nil
	}
	return Find(id, VMMArgv(idir))
}

// VMMRunning reports whether the microVM launched from idir is running.
func VMMRunning(idir string) bool {
	h := FindVMM(idir)
	if h == nil {
		return false
	}
	h.Close()
	return true
}
