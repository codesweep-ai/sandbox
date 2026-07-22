package fcdisk

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/codesweep-ai/sandbox/internal/run"
)

// TestRepoKeyByteStream pins the exact byte stream hashed by RepoKey (for-each-ref
// output then a "HEAD:<ref>\n" line), and that it yields a 40-hex key.
func TestRepoKeyByteStream(t *testing.T) {
	f := run.NewFake()
	f.On("for-each-ref", run.Result{Stdout: "abc123 refs/heads/main\n"}, nil)
	f.On("symbolic-ref", run.Result{Stdout: "refs/heads/main\n"}, nil)

	got := RepoKey(context.Background(), f, "/some/repo")

	want := sha256.Sum256([]byte("abc123 refs/heads/main\nHEAD:refs/heads/main\n"))
	wantHex := hex.EncodeToString(want[:])[:40]
	if got != wantHex {
		t.Fatalf("RepoKey = %q, want %q", got, wantHex)
	}
	if len(got) != 40 {
		t.Fatalf("RepoKey length = %d, want 40", len(got))
	}
}

// TestRepoKeyDetachedHead falls back to rev-parse when symbolic-ref yields nothing.
func TestRepoKeyDetachedHead(t *testing.T) {
	f := run.NewFake()
	f.On("for-each-ref", run.Result{Stdout: "deadbeef refs/tags/v1\n"}, nil)
	f.On("symbolic-ref", run.Result{Stdout: ""}, nil)
	f.On("rev-parse", run.Result{Stdout: "deadbeefcafe\n"}, nil)

	got := RepoKey(context.Background(), f, "/r")
	want := sha256.Sum256([]byte("deadbeef refs/tags/v1\nHEAD:deadbeefcafe\n"))
	if got != hex.EncodeToString(want[:])[:40] {
		t.Fatalf("detached-HEAD key mismatch: %q", got)
	}
}

// TestRepoKeyFailure returns "" when the source repo can't be read.
func TestRepoKeyFailure(t *testing.T) {
	f := run.NewFake()
	f.On("for-each-ref", run.Result{}, &run.ExitError{ExitCode: 128})
	if got := RepoKey(context.Background(), f, "/nope"); got != "" {
		t.Fatalf("RepoKey on failure = %q, want empty", got)
	}
}

// TestPruneCacheDir removes stale *.ext4 and old build temps but keeps fresh disks.
func TestPruneCacheDir(t *testing.T) {
	dir := t.TempDir()
	fresh := filepath.Join(dir, "a-key1.ext4")
	stale := filepath.Join(dir, "b-key2.ext4")
	temp := filepath.Join(dir, "c-key3.ext4.tmp.123")
	for _, p := range []string{fresh, stale, temp} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-20 * 24 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(temp, old, old); err != nil {
		t.Fatal(err)
	}

	pruneCacheDir(dir, 14)

	if !exists(fresh) {
		t.Errorf("fresh disk was pruned")
	}
	if exists(stale) {
		t.Errorf("stale disk (>14d) was not pruned")
	}
	if exists(temp) {
		t.Errorf("old build temp (>1d) was not pruned")
	}

	// ttlDays<=0 disables pruning entirely.
	again := filepath.Join(dir, "d-key4.ext4")
	if err := os.WriteFile(again, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(again, old, old); err != nil {
		t.Fatal(err)
	}
	pruneCacheDir(dir, 0)
	if !exists(again) {
		t.Errorf("ttlDays=0 should disable pruning")
	}
}

// TestStoreKeyHash pins storeKey to sha256 of the concatenated images/layers JSON.
func TestStoreKeyHash(t *testing.T) {
	f := run.NewFake()
	f.On("overlay-images/images.json", run.Result{Stdout: "IMAGES-LAYERS"}, nil)

	got := storeKey(context.Background(), f, "img", "cs-sandbox-shared-x")
	want := sha256.Sum256([]byte("IMAGES-LAYERS"))
	if got != hex.EncodeToString(want[:])[:40] {
		t.Fatalf("storeKey = %q", got)
	}
}
