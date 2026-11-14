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
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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

// goMod is the module manifest, embedded so the image build can install the
// sibling cs- tools at the versions THIS repository pins rather than at
// whatever their branch tips happen to be. It is read for those versions, not
// copied into the build context: the context is rootfs/, every file of which
// `COPY . /sandbox` puts inside the guest.
//
//go:embed go.mod
var goMod embed.FS

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
	case "guest/init", "guest/vsock-connect", "rootfs/entrypoint", "rootfs/nested-rootless", "rootfs/supervise":
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

// ToolPins reports the versions this module pins for the sibling cs- tools the
// image ships, as module path -> version. It prefers the checkout's go.mod over
// the embedded copy, the same preference sourceFS applies to the image tree, so
// a checkout that has moved a pin builds an image carrying it.
//
// The manifest is scanned rather than parsed: a require line is "path version"
// once trimmed, which is all this needs. A module the manifest does not mention
// is simply absent, and the caller decides whether that is fatal.
func ToolPins(assetDir string) (map[string]string, error) {
	data, err := os.ReadFile(filepath.Join(assetDir, "go.mod"))
	if err != nil {
		if data, err = fs.ReadFile(goMod, "go.mod"); err != nil {
			return nil, err
		}
	}
	pins := map[string]string{}
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || !strings.HasPrefix(fields[0], "github.com/codesweep-ai/") {
			continue
		}
		if strings.HasPrefix(fields[1], "v") {
			pins[fields[0]] = fields[1]
		}
	}
	return pins, nil
}

// TierPins reads image/tiers.env — the tier images the leaf Containerfile is
// built ON, and the one place a bump happens.
//
// Two entries, one per family: AGENTS_REF for the shipped image and
// SLIM_AGENTS_REF for the CI one. Both are pinned here rather than as ARG
// defaults in the Containerfiles because the slim Containerfiles are DERIVED
// (see image/ci-slim.sh) and a derived file cannot carry a pin of its own.
//
// Same fallback as everything else here: the checkout's copy when there is one,
// the embedded copy otherwise, so a downloaded binary can still build.
func TierPins(assetDir string) (map[string]string, error) {
	var data []byte
	if d, ok := onDiskImage(assetDir); ok {
		if b, err := os.ReadFile(filepath.Join(d, "tiers.env")); err == nil {
			data = b
		}
	}
	if data == nil {
		b, err := fs.ReadFile(image(), "tiers.env")
		if err != nil {
			return nil, err
		}
		data = b
	}
	pins := map[string]string{}
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			pins[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return pins, nil
}

// AgentPins reads the ARG defaults of image/Containerfile.agents, as name ->
// value: the agent CLI versions the agents tier installs, CLAUDE_CODE_VERSION,
// CODEX_VERSION and OPENCODE_VERSION among them.
//
// `install-agent-clis` reads them here so that a host gets the CLIs its
// sandboxes run, from the one place they are pinned. Same fallback as the other
// pins: the checkout's copy when there is one, the embedded copy otherwise.
func AgentPins(assetDir string) (map[string]string, error) {
	var data []byte
	if d, ok := onDiskImage(assetDir); ok {
		if b, err := os.ReadFile(filepath.Join(d, "Containerfile.agents")); err == nil {
			data = b
		}
	}
	if data == nil {
		b, err := fs.ReadFile(image(), "Containerfile.agents")
		if err != nil {
			return nil, err
		}
		data = b
	}
	pins := map[string]string{}
	for line := range strings.SplitSeq(string(data), "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "ARG ")
		if !ok {
			continue
		}
		if k, v, ok := strings.Cut(strings.TrimSpace(rest), "="); ok {
			pins[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return pins, nil
}

// GuestInitPath returns a host path to the guest init (image/guest/init): the
// on-disk checkout copy when available, otherwise the embedded copy materialized
// into cacheDir under its content's name (contentName), so its content hash —
// the base-rootfs stamp key — is identical to the on-disk copy's. Returns "" (no error) if neither the
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
	p := filepath.Join(cacheDir, contentName("guest-init", "", data))
	if err := materialize(p, data, 0o755); err != nil {
		return ""
	}
	return p
}

// contentName names a materialized guest file by its bytes: stem, a dash, the
// first 12 hex digits of their sha256, then ext. Two cs-sandbox versions whose
// embedded copies differ share one cache, and under one name each create
// rewrote the other's file, so a create could hash the other version's init
// into its own base-rootfs stamp (SBX-122). Under its content's name a file
// only ever holds those bytes.
func contentName(stem, ext string, data []byte) string {
	sum := sha256.Sum256(data)
	return stem + "-" + hex.EncodeToString(sum[:])[:12] + ext
}

// materialize puts data at p for other processes to read. Every create calls
// it, and other creates hash the same file at the same time: the guest init's
// hash is in the base-rootfs stamp and the initramfs source's hash names the
// kernel set. A file that already holds data is left alone. Otherwise the
// bytes go to a temporary file beside p, renamed over it, so a reader sees the
// old file or the new one, never a truncated one. os.WriteFile truncates
// first, and a reader in that gap hashed an empty file, which rebuilt the base
// rootfs with an empty init or looked for a kernel set that does not exist.
func materialize(p string, data []byte, mode os.FileMode) error {
	if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode().Perm() == mode {
		if cur, err := os.ReadFile(p); err == nil && bytes.Equal(cur, data) {
			return nil
		}
	}
	f, err := os.CreateTemp(filepath.Dir(p), "."+filepath.Base(p)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(data)
	cerr := f.Close()
	if err := errors.Join(werr, cerr, os.Chmod(tmp, mode)); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
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
	p := filepath.Join(cacheDir, contentName("guest-initramfs-init", ".c", data))
	if err := materialize(p, data, 0o644); err != nil {
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
