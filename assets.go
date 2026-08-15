// Package assets holds the image build assets embedded into the cs-sandbox
// binary. It is not this module's entry point: cs-sandbox is a command-line
// tool rather than a library, and the program is cmd/cs-sandbox.
//
//	go install github.com/codesweep-ai/sandbox/cmd/cs-sandbox@latest
//
// The package sits at the module root only because a //go:embed directive
// cannot reach a parent directory and the tree it embeds, image/, is there.
// Everything the tool actually does lives under internal/.
//
// What it embeds — the Containerfile, the guest rootfs skeleton, and the guest
// init — is what lets a single downloaded binary build the sandbox image and
// boot microVMs with no source checkout. When run from a checkout the on-disk
// image/ tree is preferred (see internal/paths.AssetDir); this embedded copy is
// the fallback that makes the binary self-contained.
package assets

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"embed"
)

// fixedMtime pins extracted-file timestamps so the build context — and thus the
// resulting image id — is reproducible and identical whether the assets came
// from a checkout or the embedded copy. 2024-01-01T00:00:00Z.
var fixedMtime = time.Unix(1704067200, 0)

//go:embed all:image
var embedded embed.FS

// image returns the embedded tree rooted so paths look like "Containerfile",
// "guest/init", "rootfs/entrypoint".
func image() fs.FS {
	sub, err := fs.Sub(embedded, "image")
	if err != nil {
		panic(err) // guaranteed by the embed directive at build time
	}
	return sub
}

// isExec reports whether an embedded path must be extracted executable — embed.FS
// reports every file as 0444, so exec bits are restored on extraction.
func isExec(rel string) bool {
	if strings.HasPrefix(rel, "rootfs/home/.local/bin/") {
		return true
	}
	switch rel {
	case "guest/init", "guest/vsock-connect", "rootfs/entrypoint":
		return true
	}
	return false
}

// sourceFS is the image tree to build from: the on-disk checkout when present,
// else the embedded copy. Both are read through fs.FS so the extractor is
// source-agnostic.
func sourceFS(assetDir string) fs.FS {
	if d, ok := onDiskImage(assetDir); ok {
		return os.DirFS(d)
	}
	return image()
}

// extractFS writes src to dst with normalized modes (isExec) and a fixed mtime,
// so the build context is byte-for-byte reproducible regardless of source.
func extractFS(src fs.FS, dst string) error {
	return fs.WalkDir(src, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		target := filepath.Join(dst, p)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := fs.ReadFile(src, p)
		if err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if isExec(p) {
			mode = 0o755
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, data, mode); err != nil {
			return err
		}
		return os.Chtimes(target, fixedMtime, fixedMtime)
	})
}

// onDiskImage returns <assetDir>/image if it holds a Containerfile (a checkout).
func onDiskImage(assetDir string) (string, bool) {
	if assetDir == "" {
		return "", false
	}
	d := filepath.Join(assetDir, "image")
	if fi, err := os.Stat(filepath.Join(d, "Containerfile")); err == nil && !fi.IsDir() {
		return d, true
	}
	return "", false
}

// ImageDir returns a temp directory holding Containerfile + rootfs/ + guest/
// ready for `podman build`. It always extracts (from the checkout or the
// embedded copy) with normalized modes + a fixed mtime, so the same image id
// results regardless of where the assets came from. cleanup removes the temp
// dir; call it when the build finishes.
func ImageDir(assetDir string) (dir string, cleanup func(), err error) {
	tmp, err := os.MkdirTemp("", "cs-sandbox-image-")
	if err != nil {
		return "", func() {}, err
	}
	if err := extractFS(sourceFS(assetDir), tmp); err != nil {
		_ = os.RemoveAll(tmp)
		return "", func() {}, err
	}
	return tmp, func() { _ = os.RemoveAll(tmp) }, nil
}

// GuestInitPath returns a host path to the guest init (image/guest/init): the
// on-disk checkout copy when available, otherwise the embedded copy materialized
// once into cacheDir (a stable path so its content hash — the base-rootfs stamp
// key — is identical to the on-disk copy). Returns "" (no error) if neither the
// checkout nor cacheDir is usable.
func GuestInitPath(assetDir, cacheDir string) string {
	if d, ok := onDiskImage(assetDir); ok {
		p := filepath.Join(d, "guest", "init")
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	if cacheDir == "" {
		return ""
	}
	data, err := fs.ReadFile(image(), "guest/init")
	if err != nil {
		return ""
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return ""
	}
	p := filepath.Join(cacheDir, "guest-init")
	if err := os.WriteFile(p, data, 0o755); err != nil {
		return ""
	}
	return p
}

// GuestInitramfsSrcPath returns a host path to the initramfs init source
// (image/guest/initramfs-init.c), materializing the embedded copy into cacheDir
// when there is no checkout — the same pattern as GuestInitPath, and for the
// same reason: its content hash keys the cached initrd.img, so the path must
// resolve identically whether the assets came from a checkout or the binary.
// Returns "" (no error) if neither source is usable; the boot-artifact build
// then fails with an actionable error rather than booting something stale.
func GuestInitramfsSrcPath(assetDir, cacheDir string) string {
	const rel = "guest/initramfs-init.c"
	if d, ok := onDiskImage(assetDir); ok {
		p := filepath.Join(d, "guest", "initramfs-init.c")
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	if cacheDir == "" {
		return ""
	}
	data, err := fs.ReadFile(image(), rel)
	if err != nil {
		return ""
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return ""
	}
	p := filepath.Join(cacheDir, "guest-initramfs-init.c")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		return ""
	}
	return p
}

// HostHelpers returns the guest ~/.local/bin tools (cs-claude/cs-codex families +
// docs) as an fs.FS, from the checkout when available else embedded — for the
// `install-agent-tools` command. Same source dir the guest home skeleton seeds
// into ~/.local/bin, so host and sandbox get the identical tools in the same path.
func HostHelpers(assetDir string) (fs.FS, error) {
	return fs.Sub(sourceFS(assetDir), "rootfs/home/.local/bin")
}
