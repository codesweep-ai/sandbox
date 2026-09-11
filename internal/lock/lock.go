// Package lock provides the host-wide create lock. Concurrent creates race on
// SSH-port/VM-IP allocation, the one-per-host fabric, and image builds; we
// serialize only the short, race-sensitive setup prefix. flock auto-releases on
// fd close (incl. crash), so a dead create can't wedge it. Reentrant via a
// depth counter.
package lock

import (
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"
)

// Lock is a reentrant, process-wide file lock over a lock path.
//
// A Lock is taken either exclusively or shared, and one instance is used in one
// mode for its whole life. Mixing them on the same path from one process is a
// deadlock rather than an upgrade: flock resolves modes per open file
// description, so a second instance asking for LOCK_EX waits on the LOCK_SH the
// first one is holding, and both are this process.
type Lock struct {
	path   string
	mu     sync.Mutex
	depth  int
	shared bool
	f      *os.File
}

// New returns a Lock over <dir>/.create.lock.
func New(instDir string) *Lock {
	return NewAt(filepath.Join(instDir, ".create.lock"))
}

// NewAt returns a Lock over an explicit lock-file path, for a resource whose
// scope is not the instance dir — the firecracker artifact cache, say, which
// several sandbox roots can share via CS_SANDBOX_FC_CACHE and which therefore
// has to be serialized on the cache rather than on any one root.
func NewAt(path string) *Lock { return &Lock{path: path} }

// Acquire takes the lock exclusively (blocking). Nested acquires in the same
// process just bump the depth counter.
func (l *Lock) Acquire() error {
	_, err := l.acquire(true)
	return err
}

// AcquireShared takes the lock in SHARED mode (blocking), so that several
// holders can say a resource is in use at once while a would-be remover of it
// cannot act.
//
// Shared rather than exclusive because these holders do not race each other:
// what each is saying is "I am using this", and any number of them can say it
// truthfully at the same time. Only the side that would take the resource away
// needs the exclusive mode, and it needs it against all of them.
//
// Blocking, where the remover's side does not block: waiting here is waiting out
// one idle check, and waiting there would be waiting out every create.
func (l *Lock) AcquireShared() error {
	l.mu.Lock()
	l.shared = true
	l.mu.Unlock()
	_, err := l.acquire(true)
	return err
}

// TryAcquire takes the lock without blocking. ok=false means another holder has
// it — the caller can report that ("waiting for …") before blocking on Acquire.
// Note that flock is per open file description, so two Locks over the same path
// contend even inside one process: reentrancy is per *Lock, not per process.
func (l *Lock) TryAcquire() (ok bool, err error) { return l.acquire(false) }

func (l *Lock) acquire(block bool) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.depth++
	if l.depth > 1 {
		return true, nil
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		l.depth--
		return false, err
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		l.depth--
		return false, err
	}
	how := unix.LOCK_EX
	if l.shared {
		how = unix.LOCK_SH
	}
	if !block {
		how |= unix.LOCK_NB
	}
	if err := unix.Flock(int(f.Fd()), how); err != nil {
		_ = f.Close()
		l.depth--
		if !block && (err == unix.EWOULDBLOCK || err == unix.EINTR) {
			return false, nil // held by someone else, not a failure
		}
		return false, err
	}
	l.f = f
	return true, nil
}

// Release drops one level of the lock; the underlying flock is released when the
// outermost Release runs.
func (l *Lock) Release() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.depth == 0 {
		return
	}
	l.depth--
	if l.depth > 0 {
		return
	}
	if l.f != nil {
		_ = unix.Flock(int(l.f.Fd()), unix.LOCK_UN)
		_ = l.f.Close()
		l.f = nil
	}
}

// With runs fn while holding the lock.
func (l *Lock) With(fn func() error) error {
	if err := l.Acquire(); err != nil {
		return err
	}
	defer l.Release()
	return fn()
}
