// Package store manages shared image stores — the podman volume
// cs-sandbox-shared-<name> holding an image set that sandboxes reuse read-only
// via --image-store. Stores are seeded by a nested ROOTLESS engine in a helper
// container, the same kind of engine (and the same keep-id id mapping) that every
// sandbox reads them with.
package store

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/codesweep-ai/sandbox/internal/run"
)

const volPrefix = "cs-sandbox-shared-"

// storePodman addresses the store's graphroot explicitly, so store work never
// touches the helper container's own (throwaway) rootless store. The runroot sits
// under the user's XDG_RUNTIME_DIR because the engine runs unprivileged and cannot
// create a directory at the root of /run.
const storePodman = `/usr/bin/podman --root /seed --runroot "$XDG_RUNTIME_DIR/seed-rr" --storage-driver overlay`

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
	initScript := asUser(storePodman + ` images >/dev/null 2>&1 || true`)
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
		// The host store is the caller's own rootless store, so the seeding engine —
		// which runs as that same uid under keep-id — can read it directly. The config
		// goes in the SEEDING USER's home, not /etc: a rootless engine reads only its
		// own storage.conf, so additionalimagestores in the system file is ignored.
		script = `mkdir -p /home/seeder/.config/containers
printf "[storage]\ndriver = \"overlay\"\n\n[storage.options]\nadditionalimagestores = [\"/var/lib/host-store\"]\n" > /home/seeder/.config/containers/storage.conf
` + asUser(`
set -e
for img in "$@"; do
  echo ">> copying $img  (host store -> shared store)"
  /usr/bin/podman save "$img" | `+storePodman+` load
done
echo "== shared store now contains: =="
`+storePodman+` images`)
	} else {
		extra = []string{"--entrypoint", "/bin/bash"}
		script = asUser(`
set -e
for img in "$@"; do
  echo ">> pulling $img"
  ` + storePodman + ` pull "$img"
done
echo "== shared store now contains: =="
` + storePodman + ` images`)
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
	script := asUser(storePodman + ` images --format "{{.Repository}}:{{.Tag}}  {{.Size}}" 2>/dev/null`)
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
// at /seed.
//
// The flags match a sandbox's, because the engine that WRITES a store has to be
// the same kind as the engine that reads it. A store is read by the nested
// rootless podman of every sandbox that mounts it, and rootless and rootful
// engines disagree about the store's on-disk ownership: a rootful seeder leaves
// the graphroot mode 0700 root-owned, which a rootless reader cannot even open,
// and image uid 0 stored under the wrong id. Seeded through the same keep-id
// mapping, the ids line up in every sandbox of the same host user.
func (m Manager) helperRun(name string, preImage ...string) []string {
	argv := []string{"podman", "run", "--rm", "--userns=keep-id", "--user", "0:0",
		"--cap-add=SYS_ADMIN", "--cap-add=SETFCAP",
		"--security-opt", "label=disable", "--security-opt", "unmask=ALL",
		"-v", vol(name) + ":/seed"}
	argv = append(argv, preImage...)
	argv = append(argv, m.Image)
	return argv
}

// asUser wraps a root-side seeding script so it bootstraps rootless podman and
// then runs the storage work as the unprivileged user, whose ids a sandbox's own
// nested engine reproduces. $STORE_USER is that user, resolved from the keep-id
// entry podman injects for the caller's uid.
func asUser(script string) string {
	return `
set -e
# The caller's own id inside this userns: the one keep-id maps to id 0 of the parent
# (rootless) namespace, which is the host user. /seed cannot answer this — podman chowns
# a fresh volume to the container's --user, which is root.
_own() { awk '$2 <= 0 && 0 < $2 + $3 { print $1 - $2; exit }' "$1"; }
STORE_UID=$(_own /proc/self/uid_map); STORE_GID=$(_own /proc/self/gid_map)
STORE_USER=$(getent passwd "$STORE_UID" | cut -d: -f1)
if [ -z "$STORE_USER" ]; then
  STORE_USER=seeder
  getent group "$STORE_GID" >/dev/null || groupadd -g "$STORE_GID" "$STORE_USER"
  useradd -u "$STORE_UID" -g "$STORE_GID" -M -d /home/seeder -s /bin/bash "$STORE_USER"
fi
mkdir -p /home/seeder && chown -R "$STORE_UID:$STORE_GID" /home/seeder && chown "$STORE_UID:$STORE_GID" /seed
/sandbox/nested-rootless "$STORE_USER" "$STORE_UID" "$STORE_GID"
runuser -u "$STORE_USER" -- env HOME=/home/seeder XDG_RUNTIME_DIR=/run/user/$STORE_UID \
  bash -c ` + shellQuote(script) + ` _ "$@"
`
}

// shellQuote renders s as a single-quoted shell word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
