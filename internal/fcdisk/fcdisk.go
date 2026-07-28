// Package fcdisk builds the on-disk artifacts a firecracker microVM boots from:
// the cached kernel/initrd/base-rootfs, the per-instance reflink rootfs copy, and
// the seed.ext4 disk (the cs-sandbox.conf + trust seed + share manifests, packed
// with `fakeroot mke2fs -d`), per docs/firecracker.md.
//
// EnsureArtifacts (build.go) BUILDS any missing/stale artifacts — the firecracker
// binary download, the fedora guest kernel + initrd + modules, and the base
// rootfs — leaving a populated cache untouched. cache.go holds the
// content-addressed repo/image-store disk caches.
package fcdisk

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/codesweep-ai/sandbox/internal/run"
)

// Cache holds the paths of the shared firecracker artifact cache.
type Cache struct {
	Dir      string       // firecracker artifact cache dir (XDG cache: $XDG_CACHE_HOME/cs-sandbox)
	Progress func(string) // optional sink for human-facing progress lines; nil = silent
}

// say emits a progress line, or nothing when no reporter is wired (e.g. tests).
func (c Cache) say(format string, a ...any) {
	if c.Progress != nil {
		c.Progress(fmt.Sprintf(format, a...))
	}
}

// FirecrackerBin is the cached firecracker binary path.
func (c Cache) FirecrackerBin() string { return filepath.Join(c.Dir, "bin", "firecracker") }

// Kernel is the cached vmlinux.elf path.
func (c Cache) Kernel() string { return filepath.Join(c.Dir, "vmlinux.elf") }

// Initrd is the cached initrd.img path.
func (c Cache) Initrd() string { return filepath.Join(c.Dir, "initrd.img") }

// BaseRootfs is the cached base rootfs ext4 path.
func (c Cache) BaseRootfs() string { return filepath.Join(c.Dir, "base-rootfs.ext4") }

// ReflinkRootfs makes the per-instance writable rootfs as a CoW copy of the base
// (cp --reflink=auto -f base-rootfs.ext4 <idir>/rootfs.ext4).
func (c Cache) ReflinkRootfs(ctx context.Context, r run.Runner, dst string) error {
	_, err := r.Run(ctx, run.Opts{}, "cp", "--reflink=auto", "-f", c.BaseRootfs(), dst)
	return err
}

// DiskMiB sizes an ext4 disk (in MiB) to a directory's content plus overhead and
// a margin.
func DiskMiB(dir string, margin int) (int, error) {
	var total int64
	err := filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		total += (info.Size() + 4095) / 4096 * 4096 // round up to block, approximating du -k
		return nil
	})
	if err != nil {
		return 0, err
	}
	kb := int(total / 1024)
	return kb/1024 + kb/4096 + margin, nil
}

// BuildExt4Dir packs a directory into a fresh ext4 image via fakeroot mke2fs -d
// (truncate -s <mib>M <img>; fakeroot -- mke2fs -F -q -t ext4 -d <dir> <img>).
func BuildExt4Dir(ctx context.Context, r run.Runner, dir, img string, margin int) error {
	mib, err := DiskMiB(dir, margin)
	if err != nil {
		return err
	}
	_ = os.Remove(img)
	if _, err := r.Run(ctx, run.Opts{}, "truncate", "-s", strconv.Itoa(mib)+"M", img); err != nil {
		return err
	}
	if _, err := r.Run(ctx, run.Opts{}, "fakeroot", "--", "mke2fs", "-F", "-q", "-t", "ext4", "-d", dir, img); err != nil {
		return err
	}
	return nil
}

// SeedInput carries the values written into cs-sandbox.conf and the share
// manifests, alongside the already-materialized seed dir (authorized_keys, host
// keys, tier key, ssh_config, …).
type SeedInput struct {
	SeedDir  string // <idir>/seed (already materialized by seed.Write)
	FCSeed   string // <idir>/fc-seed (the staging dir packed into seed.ext4)
	User     string
	UID, GID int
	Hostname string
	Type     string
	Yolo     bool
	IP       string
	GW       string
	DNS      string

	// Manifests, one line each (already assembled by the caller).
	Repos       string // \x1f-separated: dir, branch, base, name, email
	Snapshots   string // one name per line
	ImageStores string // one name per line
}

// BuildSeedExt4 assembles the fc-seed staging dir from the materialized seed dir,
// writes cs-sandbox.conf + the share manifests, then packs it into seed.ext4.
func (c Cache) BuildSeedExt4(ctx context.Context, r run.Runner, in SeedInput, seedImg string) error {
	sd := in.FCSeed
	_ = os.RemoveAll(sd)
	if err := os.MkdirAll(sd, 0o755); err != nil {
		return err
	}

	// Copy the trust material from the materialized seed dir into the staging dir.
	// A missing source is fine (optional files like inject-env/git_identity), but a
	// failed WRITE must propagate — a seed disk silently missing authorized_keys or
	// the peer-restricting ssh_config while still holding the tier key is unsafe.
	copyIf := func(rel, dstName string, mode os.FileMode) error {
		data, err := os.ReadFile(filepath.Join(in.SeedDir, rel))
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return fmt.Errorf("fc-seed: read %s: %w", rel, err)
		}
		if err := os.WriteFile(filepath.Join(sd, dstName), data, mode); err != nil {
			return fmt.Errorf("fc-seed: write %s: %w", dstName, err)
		}
		return nil
	}
	for _, f := range []struct {
		rel, dst string
		mode     os.FileMode
	}{
		{"authorized_keys", "authorized_keys", 0o600},
		{"ssh_config", "ssh_config", 0o600},
		{"host_hosts", "host_hosts", 0o644},
		{"inject-env", "inject-env", 0o600},
		{"git_identity", "git_identity", 0o600},
	} {
		if err := copyIf(f.rel, f.dst, f.mode); err != nil {
			return err
		}
	}

	// Host keys (ssh_host_*) live under seed/host_keys/.
	if entries, err := os.ReadDir(filepath.Join(in.SeedDir, "host_keys")); err == nil {
		for _, e := range entries {
			if !strings.HasPrefix(e.Name(), "ssh_host_") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(in.SeedDir, "host_keys", e.Name()))
			if err != nil {
				return fmt.Errorf("fc-seed: read host key %s: %w", e.Name(), err)
			}
			mode := os.FileMode(0o600)
			if strings.HasSuffix(e.Name(), ".pub") {
				mode = 0o644
			}
			if err := os.WriteFile(filepath.Join(sd, e.Name()), data, mode); err != nil {
				return fmt.Errorf("fc-seed: write host key %s: %w", e.Name(), err)
			}
		}
	}

	// Tier keys (id_cs-sandbox_*): the private key the sandbox ssh's out with.
	if entries, err := os.ReadDir(in.SeedDir); err == nil {
		for _, e := range entries {
			if !strings.HasPrefix(e.Name(), "id_cs-sandbox_") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(in.SeedDir, e.Name()))
			if err != nil {
				return fmt.Errorf("fc-seed: read tier key %s: %w", e.Name(), err)
			}
			mode := os.FileMode(0o600)
			if strings.HasSuffix(e.Name(), ".pub") {
				mode = 0o644
			}
			if err := os.WriteFile(filepath.Join(sd, e.Name()), data, mode); err != nil {
				return fmt.Errorf("fc-seed: write tier key %s: %w", e.Name(), err)
			}
		}
	}

	// Optional Claude/Codex credential trees.
	for _, sub := range []string{"claude", "codex"} {
		src := filepath.Join(in.SeedDir, sub)
		if fi, err := os.Stat(src); err == nil && fi.IsDir() {
			if _, err := r.Run(ctx, run.Opts{}, "cp", "-a", src, filepath.Join(sd, sub)); err != nil {
				return err
			}
		}
	}

	// cs-sandbox.conf — the guest reads this first.
	var b strings.Builder
	fmt.Fprintf(&b, "CS_SANDBOX_USER=%s\n", shellQuote(in.User))
	fmt.Fprintf(&b, "CS_SANDBOX_UID=%d\n", in.UID)
	fmt.Fprintf(&b, "CS_SANDBOX_GID=%d\n", in.GID)
	fmt.Fprintf(&b, "CS_SANDBOX_HOSTNAME=%s\n", shellQuote(in.Hostname))
	fmt.Fprintf(&b, "CS_SANDBOX_TYPE=%s\n", shellQuote(in.Type))
	fmt.Fprintf(&b, "CS_SANDBOX_YOLO=%s\n", boolFlag(in.Yolo))
	fmt.Fprintf(&b, "CS_SANDBOX_IP=%s\n", shellQuote(in.IP))
	fmt.Fprintf(&b, "CS_SANDBOX_GW=%s\n", shellQuote(in.GW))
	fmt.Fprintf(&b, "CS_SANDBOX_DNS=%s\n", shellQuote(in.DNS))
	if err := os.WriteFile(filepath.Join(sd, "cs-sandbox.conf"), []byte(b.String()), 0o644); err != nil {
		return err
	}

	// Share manifests (always written, possibly empty).
	if err := os.WriteFile(filepath.Join(sd, "repos"), []byte(in.Repos), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(sd, "snapshots"), []byte(in.Snapshots), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(sd, "imagestores"), []byte(in.ImageStores), 0o644); err != nil {
		return err
	}

	if err := BuildExt4Dir(ctx, r, sd, seedImg, 16); err != nil {
		return err
	}
	_ = os.RemoveAll(sd)
	return nil
}

func boolFlag(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
