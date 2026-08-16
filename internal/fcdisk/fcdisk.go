// Package fcdisk builds the on-disk artifacts a firecracker microVM boots from:
// the cached kernel/initrd/base-rootfs, the per-instance reflink rootfs copy, and
// the seed.ext4 disk (the cs-sandbox.conf + trust seed + share manifests, packed
// with `fakeroot mke2fs -d`), per SPEC.md §12.3.
//
// EnsureArtifacts (build.go) BUILDS any missing/stale artifacts — the firecracker
// binary download, the fedora guest kernel + initrd + modules, and the base
// rootfs — leaving a populated cache untouched. cache.go holds the
// content-addressed repo/image-store disk caches.
package fcdisk

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/codesweep-ai/sandbox/internal/run"
	"github.com/codesweep-ai/sandbox/internal/seed"
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

// FirecrackerVersion is the release tag of the cached firecracker binary, read
// from the `fc-version` stamp EnsureArtifacts writes. It returns "" when nothing
// is cached, or when the binary was downloaded before the version was tracked —
// so report it as unknown rather than assuming the current pin.
func (c Cache) FirecrackerVersion() string { return c.readStamp("fc-version") }

// Kernel is the cached vmlinux.elf path.
func (c Cache) Kernel() string { return filepath.Join(c.Dir, "vmlinux.elf") }

// Initrd is the cached initrd.img path.
func (c Cache) Initrd() string { return filepath.Join(c.Dir, "initrd.img") }

// BaseRootfs is the cached base rootfs ext4 path.
func (c Cache) BaseRootfs() string { return filepath.Join(c.Dir, "base-rootfs.ext4") }

// ReflinkRootfs makes the per-instance writable rootfs as a CoW copy of the base
// (cp --reflink=auto -f base-rootfs.ext4 <idir>/rootfs.ext4). It takes the
// artifact lock: a build rewriting the base in place while this copy runs would
// hand the new instance a torn disk.
func (c Cache) ReflinkRootfs(ctx context.Context, r run.Runner, dst string) error {
	return c.withArtifactLock(func() error {
		_, err := r.Run(ctx, run.Opts{}, "cp", "--reflink=auto", "-f", c.BaseRootfs(), dst)
		return err
	})
}

// GrowRootfs grows a per-instance rootfs disk to gb GiB in place: extend the
// (sparse) file, then extend the ext4 inside it. Data is preserved — this is the
// grow direction only, and a request at or below the current size is a no-op
// rather than an error, so a --disk that merely matches what a sandbox already
// has costs nothing on every create.
//
// Host-side by necessity: the guest carries no e2fsprogs, so it cannot grow its
// own root at boot, and virtio-blk fixes the capacity a running VM sees anyway.
// The sandbox must therefore be stopped, which at create time it is.
//
// The disk is sparse and reflink-shared with the base, so the extension costs no
// space until the guest writes into it — a large --disk is a ceiling, not an
// allocation.
func GrowRootfs(ctx context.Context, r run.Runner, img string, gb int) error {
	fi, err := os.Stat(img)
	if err != nil {
		return err
	}
	want := int64(gb) << 30
	if want <= fi.Size() {
		return nil
	}
	if _, err := r.Run(ctx, run.Opts{}, "truncate", "-s", strconv.Itoa(gb)+"G", img); err != nil {
		return err
	}
	// resize2fs refuses a filesystem it has not seen checked, so e2fsck first.
	// Exit 1 is "errors found and corrected", which is a success for our purpose;
	// only 2+ (reboot needed / uncorrected / usage) is a real failure.
	if _, err := r.Run(ctx, run.Opts{}, "e2fsck", "-fp", img); err != nil {
		var ee *run.ExitError
		if !errors.As(err, &ee) || ee.ExitCode > 1 {
			return fmt.Errorf("fc: checking %s before growing it to %d GiB: %w", img, gb, err)
		}
	}
	if _, err := r.Run(ctx, run.Opts{}, "resize2fs", img); err != nil {
		return fmt.Errorf("fc: growing %s to %d GiB: %w", img, gb, err)
	}
	return nil
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
//
// mke2fs reads the tree as the real user, so one file the source ships unreadable
// stops the build dead — and an image store holds container image layers, which means
// a mode-0000 /etc/gshadow from every distro base image in it. The same pre-pass the
// base rootfs uses clears that, inside the one fakeroot so the ownership recorded in
// the image stays fakeroot's rather than the caller's.
func BuildExt4Dir(ctx context.Context, r run.Runner, dir, img string, margin int) error {
	mib, err := DiskMiB(dir, margin)
	if err != nil {
		return err
	}
	_ = os.Remove(img)
	if _, err := r.Run(ctx, run.Opts{}, "truncate", "-s", strconv.Itoa(mib)+"M", img); err != nil {
		return err
	}
	script := `set -e
find "$FC_DIR" ! -readable -exec chmod u+rX {} + 2>/dev/null || true
mke2fs -F -q -t ext4 -d "$FC_DIR" "$FC_IMG"`
	env := []string{"FC_DIR=" + dir, "FC_IMG=" + img}
	if _, err := r.Run(ctx, run.Opts{Env: env}, "fakeroot", "--", "bash", "-c", script); err != nil {
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

	// Optional per-agent credential trees. Driven by seed.AgentNames() rather
	// than a list repeated here: a hardcoded {"claude", "codex"} silently dropped
	// opencode's tree, so --inherit-agent-login opencode reported success and
	// produced a microVM with no login. The podman path was unaffected, which is
	// what kept it hidden.
	for _, sub := range seed.AgentNames() {
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
