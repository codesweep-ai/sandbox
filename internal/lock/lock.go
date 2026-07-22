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
type Lock struct {
	path  string
	mu    sync.Mutex
	depth int
	f     *os.File
}

// New returns a Lock over <dir>/.create.lock.
func New(instDir string) *Lock {
	return &Lock{path: filepath.Join(instDir, ".create.lock")}
}

// Acquire takes the lock (blocking). Nested acquires in the same process just
// bump the depth counter.
func (l *Lock) Acquire() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.depth++
	if l.depth > 1 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		l.depth--
		return err
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		l.depth--
		return err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		_ = f.Close()
		l.depth--
		return err
	}
	l.f = f
	return nil
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
