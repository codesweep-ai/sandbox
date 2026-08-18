package fcdisk

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/codesweep-ai/sandbox/internal/run"
)

// repoCacheDir is the content-addressed bare-repo ext4 cache (shared across VMs).
func (c Cache) repoCacheDir() string { return filepath.Join(c.Dir, "repo-disks") }

// storeCacheDir is the content-addressed shared-image-store ext4 cache.
func (c Cache) storeCacheDir() string { return filepath.Join(c.Dir, "store-disks") }

// sha40 returns the first 40 hex chars of sha256(data) (matching `sha256sum | cut -c1-40`).
func sha40(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:40]
}

// sha12 returns the first 12 hex chars of sha256(data).
func sha12(data string) string {
	sum := sha256.Sum256([]byte(data))
	return hex.EncodeToString(sum[:])[:12]
}

// RepoKey hashes a source repo's clonable state (ref tips + HEAD) to a 40-hex
// key, "" on failure. Two clones of the same repo at the same state get the same
// key, so their bare-repo ext4 is byte-identical.
func RepoKey(ctx context.Context, r run.Runner, src string) string {
	refs, err := r.Run(ctx, run.Opts{ReadOnly: true}, "git", "-C", src, "for-each-ref", "--format=%(objectname) %(refname)")
	if err != nil {
		return ""
	}
	head := run.Output(ctx, r, "git", "-C", src, "symbolic-ref", "-q", "HEAD")
	if head == "" {
		head = run.Output(ctx, r, "git", "-C", src, "rev-parse", "HEAD")
	}
	// The byte stream fed to sha256sum: for-each-ref's output (each line
	// newline-terminated) then a "HEAD:<ref>\n" line.
	blob := refs.Stdout + "HEAD:" + head + "\n"
	return sha40([]byte(blob))
}

// RepoDisk returns the path to a cached bare-repo ext4 for src, building it on a
// miss (git clone --bare -> ext4), and reuses it on a hit (bumping its mtime so
// TTL GC keeps an actively-reused disk). Returns "" if the state key can't be
// computed.
func (c Cache) RepoDisk(ctx context.Context, r run.Runner, src, label string) (string, error) {
	key := RepoKey(ctx, r, src)
	if len(key) != 40 {
		return "", fmt.Errorf("fc: repo disk (%s): cannot compute state key", label)
	}
	srcid := sha12(src)
	dir := c.repoCacheDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	cachePath := filepath.Join(dir, srcid+"-"+key+".ext4")
	if _, err := os.Stat(cachePath); err == nil {
		now := time.Now()
		_ = os.Chtimes(cachePath, now, now) // survive TTL GC; never creates
		if _, err := os.Stat(cachePath); err == nil {
			return cachePath, nil
		}
	}
	// Miss: build into a private temp, then atomically publish (parallel creates
	// of the same key race harmlessly — identical bytes, last mv wins).
	tg := fmt.Sprintf("%s.git.%d", cachePath, os.Getpid())
	td := fmt.Sprintf("%s.tmp.%d", cachePath, os.Getpid())
	_ = os.RemoveAll(tg)
	_ = os.RemoveAll(td)
	cleanup := func() { _ = os.RemoveAll(tg); _ = os.RemoveAll(td) }
	if _, err := r.Run(ctx, run.Opts{}, "git", "clone", "-q", "--bare", src, tg); err != nil {
		cleanup()
		return "", fmt.Errorf("fc: repo disk (%s): clone: %w", label, err)
	}
	if err := BuildExt4Dir(ctx, r, tg, td, 96); err != nil {
		cleanup()
		return "", fmt.Errorf("fc: repo disk (%s): %w", label, err)
	}
	_ = os.RemoveAll(tg)
	if err := os.Rename(td, cachePath); err != nil {
		cleanup()
		return "", err
	}
	return cachePath, nil
}

// RepoCacheGC drops cached repo disks (and orphaned build temps) not touched
// within ttlDays (0 disables).
// RepoCacheGC prunes cached repo disks older than ttlDays. Paths present in
// inUse are never pruned — see pruneCacheDir.
func (c Cache) RepoCacheGC(ttlDays int, inUse map[string]bool) {
	pruneCacheDir(c.repoCacheDir(), ttlDays, inUse)
}

// StoreDisk returns the path to a cached RO image-store ext4 built from the
// shared podman volume cs-sandbox-shared-<name>, building it on a miss and
// reusing it on a hit. image is the runner image used to read/tar the store as
// the keep-id root.
func (c Cache) StoreDisk(ctx context.Context, r run.Runner, image, name string) (string, error) {
	vol := "cs-sandbox-shared-" + name
	if _, err := r.Run(ctx, run.Opts{ReadOnly: true}, "podman", "volume", "exists", vol); err != nil {
		return "", fmt.Errorf("fc: image-store %q: volume %s does not exist", name, vol)
	}
	key := storeKey(ctx, r, image, vol)
	if len(key) != 40 {
		return "", fmt.Errorf("fc: image-store %q: cannot compute state key", name)
	}
	dir := c.storeCacheDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	cachePath := filepath.Join(dir, name+"-"+key+".ext4")
	if _, err := os.Stat(cachePath); err == nil {
		now := time.Now()
		_ = os.Chtimes(cachePath, now, now)
		if _, err := os.Stat(cachePath); err == nil {
			return cachePath, nil
		}
	}
	// Miss: tar the store from inside the keep-id container (image-root presents
	// as uid 0 there), then rebuild it as ext4 under host fakeroot preserving that
	// logical (root=0) ownership.
	pid := os.Getpid()
	ttar := fmt.Sprintf("%s.tar.%d", cachePath, pid)
	tdir := fmt.Sprintf("%s.d.%d", cachePath, pid)
	td := fmt.Sprintf("%s.tmp.%d", cachePath, pid)
	cleanup := func() { _ = os.RemoveAll(ttar); _ = os.RemoveAll(tdir); _ = os.RemoveAll(td) }
	cleanup()

	// Streamed to the file rather than captured: this tar is the whole sandbox
	// image, so capturing it held gigabytes in a buffer and again in the string
	// copied out of it. The shipped image is 9.3 GB.
	if _, err := r.Run(ctx, run.Opts{StdoutFile: ttar}, "podman", "run", "--rm", "--userns=keep-id",
		"--user", "0:0", "--security-opt", "label=disable", "-v", vol+":/store:ro",
		"--entrypoint", "tar", image, "-C", "/store", "-cf", "-", "."); err != nil {
		cleanup()
		return "", fmt.Errorf("fc: image-store %q: tar: %w", name, err)
	}
	// Size the ext4 to the tar's content plus overhead, then unpack + mke2fs -d
	// under one fakeroot so the extracted ownership survives into the image.
	fi, err := os.Stat(ttar)
	if err != nil {
		cleanup()
		return "", err
	}
	szk := int(fi.Size() / 1024)
	mib := szk/1024 + szk/4096 + 128
	if _, err := r.Run(ctx, run.Opts{}, "truncate", "-s", strconv.Itoa(mib)+"M", td); err != nil {
		cleanup()
		return "", err
	}
	// The chmod pre-pass is not optional here: mke2fs reads the tree as the real user,
	// and a store holds container image layers, so it holds the mode-0000 /etc/gshadow
	// every distro base image ships. Without it the build dies on the first one.
	script := fmt.Sprintf("mkdir -p %q && tar -C %q -xf %q && "+
		"{ find %q ! -readable -exec chmod u+rX {} + 2>/dev/null || true; } && "+
		"mke2fs -F -q -t ext4 -d %q %q",
		tdir, tdir, ttar, tdir, tdir, td)
	if _, err := r.Run(ctx, run.Opts{}, "fakeroot", "--", "bash", "-c", script); err != nil {
		cleanup()
		return "", fmt.Errorf("fc: image-store %q: mke2fs: %w", name, err)
	}
	_ = os.RemoveAll(ttar)
	_ = os.RemoveAll(tdir)
	if err := os.Rename(td, cachePath); err != nil {
		cleanup()
		return "", err
	}
	return cachePath, nil
}

// storeKey hashes the store's images.json + layers.json (read from inside the
// keep-id container that can see them) to a 40-hex key, "" on failure.
func storeKey(ctx context.Context, r run.Runner, image, vol string) string {
	res, err := r.Run(ctx, run.Opts{ReadOnly: true}, "podman", "run", "--rm",
		"--userns=keep-id", "--user", "0:0", "--security-opt", "label=disable",
		"-v", vol+":/store:ro", "--entrypoint", "sh", image, "-c",
		"cat /store/overlay-images/images.json /store/overlay-layers/layers.json 2>/dev/null")
	if err != nil {
		return ""
	}
	return sha40([]byte(res.Stdout))
}

// StoreCacheGC drops cached store disks (and orphaned build temps) not touched
// within ttlDays (0 disables).
// StoreCacheGC prunes cached image-store disks older than ttlDays. Paths present
// in inUse are never pruned — see pruneCacheDir.
func (c Cache) StoreCacheGC(ttlDays int, inUse map[string]bool) {
	pruneCacheDir(c.storeCacheDir(), ttlDays, inUse)
}

// pruneCacheDir deletes *.ext4 older than ttlDays and *.ext4.* build temps older
// than a day.
func pruneCacheDir(dir string, ttlDays int, inUse map[string]bool) {
	if ttlDays <= 0 {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	now := time.Now()
	ttl := time.Duration(ttlDays) * 24 * time.Hour
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		age := now.Sub(info.ModTime())
		name := e.Name()
		switch {
		case filepath.Ext(name) == ".ext4":
			// Instances attach these cached disks directly rather than copying
			// them, so a disk an instance still references must outlive the TTL:
			// unlinking it would not disturb a *running* VM (the open fd keeps
			// the inode alive) but would break the next `start`.
			if inUse[filepath.Join(dir, name)] {
				continue
			}
			if age > ttl {
				_ = os.Remove(filepath.Join(dir, name))
			}
		default:
			// Orphaned build temps (foo.ext4.git.PID, foo.ext4.tmp.PID, …): >1 day.
			if age > 24*time.Hour {
				_ = os.RemoveAll(filepath.Join(dir, name))
			}
		}
	}
}
