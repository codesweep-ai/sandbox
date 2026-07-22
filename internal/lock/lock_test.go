package lock

import "testing"

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
