// Package store manages shared image stores — the podman volume
// cs-sandbox-shared-<name> holding an image set that sandboxes reuse read-only
// via --image-store. Stores are seeded with the rootful nested engine (in a
// helper container) so images are owned by the keep-id subuid base every sandbox
// shares.
package store

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/codesweep-ai/sandbox/internal/run"
)

const volPrefix = "cs-sandbox-shared-"

var nameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// Manager drives store operations through the Runner + image.
type Manager struct {
	Runner run.Runner
	Image  string
}

func vol(name string) string { return volPrefix + name }

// ValidName reports whether a store name is acceptable.
func ValidName(name string) error {
	if name == "." || name == ".." || len(name) > 200 || !nameRe.MatchString(name) {
		return fmt.Errorf("invalid store name %q", name)
	}
	return nil
}

// Exists reports whether the store volume exists.
func (m Manager) Exists(ctx context.Context, name string) bool {
	_, err := m.Runner.Run(ctx, run.Opts{ReadOnly: true}, "podman", "volume", "exists", vol(name))
	return err == nil
}

// Create makes an empty, initialized store.
func (m Manager) Create(ctx context.Context, name string) error {
	if err := ValidName(name); err != nil {
		return err
	}
	if m.Exists(ctx, name) {
		return fmt.Errorf("shared store %q already exists", name)
	}
	if _, err := m.Runner.Run(ctx, run.Opts{}, "podman", "volume", "create", vol(name)); err != nil {
		return err
	}
	// Initialize the overlay store layout so an unseeded store mounts cleanly.
	initScript := `/usr/bin/podman --root /seed --runroot /run/seed-rr --storage-driver overlay images >/dev/null 2>&1 || true`
	argv := append(m.helperRun(name, "--entrypoint", "/bin/bash"), "-c", initScript)
	_, err := m.Runner.Run(ctx, run.Opts{}, argv...)
	return err
}

// Seed populates a store by pulling images from a registry, or (fromHost)
// copying+re-owning images already in the host store.
func (m Manager) Seed(ctx context.Context, name string, images []string, fromHost bool) error {
	if err := ValidName(name); err != nil {
		return err
	}
	if len(images) == 0 {
		return fmt.Errorf("seed-store needs at least one image")
	}
	if !m.Exists(ctx, name) {
		if _, err := m.Runner.Run(ctx, run.Opts{}, "podman", "volume", "create", vol(name)); err != nil {
			return err
		}
	}
	var extra []string
	var script string
	if fromHost {
		hostStore := run.Output(ctx, m.Runner, "podman", "info", "--format", "{{.Store.GraphRoot}}")
		if hostStore == "" {
			return fmt.Errorf("cannot resolve host image store")
		}
		for _, img := range images {
			if _, err := m.Runner.Run(ctx, run.Opts{ReadOnly: true}, "podman", "image", "exists", img); err != nil {
				return fmt.Errorf("image %q is not in the host store — build/pull it first, or drop --from-host", img)
			}
		}
		extra = []string{"-v", hostStore + ":/var/lib/host-store:ro", "--entrypoint", "/bin/bash"}
		script = `
set -e
mkdir -p /etc/containers
printf "[storage]\ndriver = \"overlay\"\n\n[storage.options]\nadditionalimagestores = [\"/var/lib/host-store\"]\n" > /etc/containers/storage.conf
for img in "$@"; do
  echo ">> copying $img  (host store -> shared store)"
  /usr/bin/podman save "$img" | /usr/bin/podman --root /seed --runroot /run/seed-rr --storage-driver overlay load
done
echo "== shared store now contains: =="
/usr/bin/podman --root /seed --runroot /run/seed-rr --storage-driver overlay images`
	} else {
		extra = []string{"--entrypoint", "/bin/bash"}
		script = `
set -e
for img in "$@"; do
  echo ">> pulling $img"
  /usr/bin/podman --root /seed --runroot /run/seed-rr --storage-driver overlay pull "$img"
done
echo "== shared store now contains: =="
/usr/bin/podman --root /seed --runroot /run/seed-rr --storage-driver overlay images`
	}
	argv := m.helperRun(name, extra...)
	argv = append(argv, "-c", script, "_")
	argv = append(argv, images...)
	_, err := m.Runner.Run(ctx, run.Opts{Interactive: true}, argv...)
	return err
}

// List returns the store names.
func (m Manager) List(ctx context.Context) []string {
	out := run.Output(ctx, m.Runner, "podman", "volume", "ls",
		"--filter", "name="+volPrefix, "--format", "{{.Name}}")
	var names []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, volPrefix) {
			names = append(names, strings.TrimPrefix(line, volPrefix))
		}
	}
	return names
}

// Images lists the images in one store (best-effort).
func (m Manager) Images(ctx context.Context, name string) (string, error) {
	script := `/usr/bin/podman --root /seed --runroot /run/seed-rr --storage-driver overlay images --format "{{.Repository}}:{{.Tag}}  {{.Size}}" 2>/dev/null`
	argv := append(m.helperRun(name, "--entrypoint", "/bin/bash"), "-c", script)
	res, err := m.Runner.Run(ctx, run.Opts{}, argv...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(res.Stdout), nil
}

// Remove deletes a store, refusing if in use unless force.
func (m Manager) Remove(ctx context.Context, name string, force bool) error {
	if !m.Exists(ctx, name) {
		return fmt.Errorf("no such shared store: %s", name)
	}
	users := run.Output(ctx, m.Runner, "podman", "ps", "-a", "--filter", "volume="+vol(name), "--format", "{{.Names}}")
	if strings.TrimSpace(users) != "" && !force {
		return fmt.Errorf("shared store %q is in use by: %s — remove those sandboxes first, or use rm-store -f", name, strings.ReplaceAll(users, "\n", " "))
	}
	argv := []string{"podman", "volume", "rm"}
	if force {
		argv = append(argv, "-f")
	}
	argv = append(argv, vol(name))
	_, err := m.Runner.Run(ctx, run.Opts{}, argv...)
	return err
}

// helperRun builds the common `podman run --rm --userns=keep-id …` argv up to
// and including the image; preImage flags (e.g. --entrypoint, extra -v) go
// before the image, and callers append the post-image command. Mounts the store
// at /seed and runs the rootful nested engine for storage ops.
func (m Manager) helperRun(name string, preImage ...string) []string {
	argv := []string{"podman", "run", "--rm", "--userns=keep-id", "--user", "0:0",
		"--cap-add=SYS_ADMIN", "--security-opt", "label=disable",
		"-v", vol(name) + ":/seed"}
	argv = append(argv, preImage...)
	argv = append(argv, m.Image)
	return argv
}
