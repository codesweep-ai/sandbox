package lock

import (
	"path/filepath"
	"testing"
)

func TestReentrant(t *testing.T) {
	l := New(t.TempDir())
	if err := l.Acquire(); err != nil {
		t.Fatal(err)
	}
	if err := l.Acquire(); err != nil { // nested acquire just bumps depth
		t.Fatal(err)
	}
	if l.depth != 2 {
		t.Fatalf("depth = %d, want 2", l.depth)
	}
	l.Release()
	if l.depth != 1 || l.f == nil {
		t.Fatal("inner release must not drop the underlying lock")
	}
	l.Release()
	if l.depth != 0 || l.f != nil {
		t.Fatal("outer release must drop the underlying lock")
	}
}

// TestTryAcquireContends: a second holder of the same lock file is refused
// without blocking, and succeeds once the first releases. flock is per open file
// description, so two Locks contend even in one process — which is exactly the
// property the artifact cache relies on.
func TestTryAcquireContends(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".artifacts.lock")
	held, other := NewAt(path), NewAt(path)

	if err := held.Acquire(); err != nil {
		t.Fatal(err)
	}
	ok, err := other.TryAcquire()
	if err != nil {
		t.Fatalf("TryAcquire on a held lock = %v, want no error", err)
	}
	if ok {
		t.Fatal("TryAcquire succeeded while another holder had the lock")
	}
	if other.depth != 0 {
		t.Errorf("depth = %d after a refused TryAcquire, want 0", other.depth)
	}

	held.Release()
	ok, err = other.TryAcquire()
	if err != nil || !ok {
		t.Fatalf("TryAcquire after release = (%v, %v), want (true, nil)", ok, err)
	}
	other.Release()
}

// TestTryAcquireReentrant: a nested TryAcquire on a Lock this caller already
// holds is not contention — it bumps the depth like Acquire.
func TestTryAcquireReentrant(t *testing.T) {
	l := NewAt(filepath.Join(t.TempDir(), ".artifacts.lock"))
	if err := l.Acquire(); err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	ok, err := l.TryAcquire()
	if err != nil || !ok {
		t.Fatalf("nested TryAcquire = (%v, %v), want (true, nil)", ok, err)
	}
	l.Release()
	if l.depth != 1 || l.f == nil {
		t.Fatal("inner release must not drop the underlying lock")
	}
}

func TestWith(t *testing.T) {
	l := New(t.TempDir())
	ran := false
	if err := l.With(func() error { ran = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Fatal("With should run fn")
	}
	if l.depth != 0 {
		t.Fatalf("depth should be 0 after With, got %d", l.depth)
	}
}
