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

	pruneCacheDir(dir, 14, nil)

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
	pruneCacheDir(dir, 0, nil)
	if !exists(again) {
		t.Errorf("ttlDays=0 should disable pruning")
	}
}

// TestPruneCacheDirKeepsInUse covers the invariant that makes attaching cached
// disks directly safe: a disk an instance still references survives the TTL.
// Pruning it would not disturb a running VM (the open fd keeps the inode alive)
// but would leave the next `start` with no disk to attach.
func TestPruneCacheDirKeepsInUse(t *testing.T) {
	dir := t.TempDir()
	held := filepath.Join(dir, "held-key1.ext4")
	unheld := filepath.Join(dir, "unheld-key2.ext4")
	for _, p := range []string{held, unheld} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-30 * 24 * time.Hour)
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}

	pruneCacheDir(dir, 14, map[string]bool{held: true})

	if !exists(held) {
		t.Errorf("in-use disk was pruned despite being referenced by an instance")
	}
	if exists(unheld) {
		t.Errorf("stale unreferenced disk was not pruned")
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

// TestStoreKeyIgnoresSeedTimestamps: seeding one image into two stores rewrites
// each layer's "created" stamp and nothing else, so the key must not move.
// While it did, every run missed the cache and rebuilt a disk it already had —
// 10 GB per run of the nested-VM test, kept for the TTL.
func TestStoreKeyIgnoresSeedTimestamps(t *testing.T) {
	const images = `[{"id":"img1","names":["localhost/sandbox-slim:ci"]}]`
	first := images + `[{"id":"l1","created":"2026-08-18T10:00:00Z"},
	                    {"id":"l2","created":"2026-08-18T10:00:01Z","parent":"l1"}]`
	again := images + `[{"id":"l1","created":"2026-08-19T22:31:04Z"},
	                    {"id":"l2","created":"2026-08-19T22:31:09Z","parent":"l1"}]`
	if storeStateKey([]byte(first)) != storeStateKey([]byte(again)) {
		t.Errorf("key moved when only the seed timestamps did")
	}
	if got := storeStateKey([]byte(first)); len(got) != 40 {
		t.Errorf("key length = %d, want 40", len(got))
	}
	// The other half: a key that ignored too much would be worse than one that
	// moves too often, because the wrong disk would mount and pass.
	moved := images + `[{"id":"l1","created":"2026-08-18T10:00:00Z"},
	                    {"id":"l2-rebuilt","created":"2026-08-18T10:00:01Z","parent":"l1"}]`
	if storeStateKey([]byte(first)) == storeStateKey([]byte(moved)) {
		t.Errorf("key held still when a layer id changed")
	}
	if storeStateKey([]byte(images)) == storeStateKey([]byte(first)) {
		t.Errorf("key held still when a whole layer set was dropped")
	}
}

// TestStoreKeyNonJSONFallsBack: an unfamiliar podman layout must still yield a
// usable key. It degrades to hashing the raw bytes — per-seeding, so it rebuilds
// more than it needs to, which is the safe direction to fail in.
func TestStoreKeyNonJSONFallsBack(t *testing.T) {
	raw := []byte("not json at all")
	want := sha256.Sum256(raw)
	if got := storeStateKey(raw); got != hex.EncodeToString(want[:])[:40] {
		t.Fatalf("storeStateKey = %q, want the raw-bytes hash", got)
	}
}
