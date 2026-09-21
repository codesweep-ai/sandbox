package renew

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/codesweep-ai/sandbox/internal/lend"
)

// Renewing a login inside a container built from this project's own sandbox image.
//
// The alternative was to run the host's own client, and it failed on finding it.
// Measured on the machine this was written on, both clients live under
// ~/.nvm/versions/node/v24.4.1/bin: a directory absent from any minimal PATH,
// whose name changes with the node version, and which `bash -c` does not reach
// because a non-interactive shell reads no startup files at all. So a renewer
// started from a terminal worked and the same renewer started by a service manager
// did not. Widening PATH, recording resolved paths and probing login shells each
// fixed a slice of that and each added a new way to be wrong.
//
// The image has none of it. /opt/claude/bin/claude and /opt/codex/bin/codex are
// fixed paths installed against verified checksums, with no version manager, no
// shell profile, and no ~/.cs-<agent>/env to take the client off the OAuth path.
// A controlled environment is the one thing a container is unambiguously good at.
//
// What it costs is that the client in the image is pinned and the host's is not,
// so this must not hand the host whatever document that client produces. It
// copies back only the values a refresh rotates — lend.RenewSpec.Fields — and
// leaves every other field at the host's own value. The project already assumes
// the image's client can READ a host credential, since that is what
// --inherit-agent-login does; this avoids assuming the reverse.

// runInContainer renews the staged credential in place.
//
// The stage holds the host's real credential while this runs, which is why the
// caller creates it owner-only and removes it as soon as the renewal is over,
// whether it worked or not.
func runInContainer(ctx context.Context, cfg Config, spec lend.RenewSpec, stage string) error {
	if cfg.Image == "" {
		why := cfg.ImageErr
		if why == "" {
			why = "no image was resolved for this host"
		}
		return fmt.Errorf("no sandbox image to renew in: %s", why)
	}
	argv := containerArgv(cfg.Image, spec, stage)
	cfg.Log.Debug("renewing in a container", slog.String("argv", strings.Join(argv, " ")))
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	// podman inherits this process's environment, and must: rootless podman finds
	// its own image store through HOME and XDG_RUNTIME_DIR, and a stripped
	// environment sends it looking in the wrong place, concluding the image is not
	// local and trying to pull 5.88 GB in the middle of a four-minute window.
	// Measured, when this was wrong: 31 seconds and then a timeout.
	//
	// Inheriting here does not leak anything into the container. podman forwards no
	// host variable unless it is named with -e or --env-host, so what the client
	// sees is exactly what containerArgv sets — which is where the isolation that
	// matters belongs, and why an ANTHROPIC_API_KEY in the renewer's environment
	// still cannot reach the client and quietly replace the refresh.
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return fmt.Errorf("renewing in a container did not finish within %s", attemptTimeout)
	}
	return fmt.Errorf("renewing in a container failed: %w: %s", err, lastLine(string(out)))
}

// containerArgv is the podman invocation, kept separate so a test can read it
// without running anything.
//
// On no group's network, deliberately: a renewal talks to the provider and
// nothing talks to it, so it joins no sandbox's network and publishes no port.
// And --rm, deliberately: it exists for one turn, and what it holds while it runs
// is a real credential.
func containerArgv(image string, spec lend.RenewSpec, stage string) []string {
	argv := []string{
		"podman", "run", "--rm",
		// Never pull. The image is a precondition, checked by Possible, and
		// a renewal has minutes to work in — fetching gigabytes inside that window
		// is not a recovery, it is a different failure with a longer timeout.
		"--pull=never",
		// The same reason the lender needs it: the mount is a host directory
		// owned by the invoking user, and under SELinux the container's access is
		// denied without either this or relabelling it.
		"--security-opt", "label=disable",
		"-v", stage + ":" + stage + ":rw",
		// HOME is the whole of the configuration. The wrappers derive their
		// profile directory from it and override any CLAUDE_CONFIG_DIR or
		// CODEX_HOME they are handed, so pointing HOME at the stage is what makes
		// them read the staged credential and nothing else.
		"-e", "HOME=" + stage,
		"-w", stage,
		"--entrypoint", spec.Bin,
		image,
	}
	// The turn itself. Whatever follows the image is handed to the entrypoint, and
	// without it the client starts with no prompt and exits before it
	// authenticates, which refreshes nothing.
	return append(argv, spec.Args...)
}

// stageProfile is the staged profile directory: the stage is the client's HOME,
// so the profile sits under it exactly as it does under the host's home.
func stageProfile(stage string, spec lend.RenewSpec) string {
	return filepath.Join(stage, spec.ProfileDir)
}

// stageCredential writes the host's credential where the container's client will
// look for it.
func stageCredential(stage string, spec lend.RenewSpec, doc []byte) error {
	if err := os.MkdirAll(stageProfile(stage, spec), 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(stageProfile(stage, spec), spec.File), doc, 0o600)
}

// readStaged returns what the client left behind, refusing anything that is not a
// JSON document rather than copying nonsense back over a working credential.
func readStaged(stage string, spec lend.RenewSpec) ([]byte, error) {
	p := filepath.Join(stageProfile(stage, spec), spec.File)
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("the renewal left no credential behind: %w", err)
	}
	var probe map[string]any
	if err := json.Unmarshal(b, &probe); err != nil {
		return nil, fmt.Errorf("the renewed credential is not JSON: %w", err)
	}
	return b, nil
}

// writeHostCredential replaces the host's credential atomically, because the
// lender reads this file on every request and must never see a partial document.
//
// The temporary file is made in the same directory, since a rename across
// filesystems is not atomic, and at 0600 because while it exists it holds the
// same secret as the file it is about to become.
func writeHostCredential(path string, doc []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".credential-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(name) // a no-op once the rename has happened
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tmp.Write(append(doc, '\n')); err != nil {
		return err
	}
	// Durable before it is visible: a credential truncated by a crash is a login
	// nobody can use and nobody can explain.
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// Possible reports, per slot, why it could not be renewed right now — an
// empty map meaning everything needed is in place.
//
// For create and doctor, which should say so while somebody is still there to act
// on it. A credential with seven hours left is genuinely fine and will fail at hour
// eight, and the reason will be the image rather than anything about the
// credential: two problems with nothing in common, worth separating before the fact
// rather than after.
//
// It takes every slot at once and runs at most one container, whatever the number
// of slots, because the question is about the image and there is only one image.
func Possible(ctx context.Context, cfg Config, slots []lend.Slot) map[string]error {
	out := map[string]error{}
	want := map[string]string{} // slot id -> the binary it needs
	for _, s := range slots {
		if spec, ok := s.RenewSpec(); ok {
			want[s.ID] = spec.Bin
		}
	}
	if len(want) == 0 {
		return out
	}
	if cfg.Image == "" {
		why := cfg.ImageErr
		if why == "" {
			why = "no image was resolved for this host"
		}
		for id := range want {
			out[id] = fmt.Errorf("the %s login cannot be renewed: there is no sandbox image to renew it in — %s\n"+
				"  renewal runs the client from the image rather than one on this host's PATH, so that it behaves "+
				"the same from a terminal and from a service manager\n"+
				"  run 'cs-sandbox build' to make one", id, why)
		}
		return out
	}

	missing, err := probeImage(ctx, cfg, want)
	if err != nil {
		for id := range want {
			out[id] = fmt.Errorf("cannot tell whether the %s login could be renewed: the image %s could not be "+
				"inspected — %w", id, cfg.Image, err)
		}
		return out
	}
	for id, bin := range want {
		if missing[bin] {
			out[id] = fmt.Errorf("the %s login cannot be renewed: %s is not in the image %s\n"+
				"  renewal runs this project's own wrapper out of the image, which the leaf images carry and a "+
				"tier image does not\n"+
				"  point CS_SANDBOX_IMAGE at a sandbox or sandbox-slim image, or run 'cs-sandbox build'",
				id, bin, cfg.Image)
		}
	}
	return out
}

// probeImage asks the image, once, which of these paths it does not have.
//
// One `test -x` per path in a single shell, because the cost here is starting the
// container and not the checking: create and doctor both call this, and neither
// should pay per credential for a question about one image.
func probeImage(ctx context.Context, cfg Config, want map[string]string) (map[string]bool, error) {
	var script strings.Builder
	for _, bin := range want {
		fmt.Fprintf(&script, "[ -x %q ] || echo %q\n", bin, bin)
	}
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	// Inherits the environment for the same reason the renewal does: rootless
	// podman needs it to find its own local image store.
	cmd := exec.CommandContext(pctx, "podman", "run", "--rm", "--pull=never",
		"--security-opt", "label=disable",
		"--entrypoint", "/bin/sh", cfg.Image, "-c", script.String())
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, lastLine(string(out)))
	}
	missing := map[string]bool{}
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			missing[line] = true
		}
	}
	return missing, nil
}

// probeTimeout bounds the image probe. It is a container start and a shell
// builtin, so anything beyond this is podman being unwell rather than the check
// being slow.
const probeTimeout = 30 * time.Second
