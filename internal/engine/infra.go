package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/codesweep-ai/sandbox/internal/lock"
	"github.com/codesweep-ai/sandbox/internal/paths"
	"github.com/codesweep-ai/sandbox/internal/run"
	"github.com/codesweep-ai/sandbox/internal/seed"
	"github.com/codesweep-ai/sandbox/internal/state"
)

// EnsureNetwork creates the shared podman network if absent (tolerates a
// parallel create that wins the race). A custom network that already exists
// must verify as a cs-sandbox managed isolated network: silently adopting an
// unverified bridge would void the isolation boundary campaign members rely
// on, so verification fails closed.
func (d Deps) EnsureNetwork(ctx context.Context) error {
	if _, err := d.Runner.Run(ctx, run.Opts{ReadOnly: true}, "podman", "network", "exists", d.Network); err == nil {
		return d.verifyManagedIsolation(ctx)
	}
	_, _ = d.Runner.Run(ctx, run.Opts{}, networkCreateArgv(d.Network)...)
	if _, err := d.Runner.Run(ctx, run.Opts{ReadOnly: true}, "podman", "network", "exists", d.Network); err != nil {
		return fmt.Errorf("could not create podman network %q", d.Network)
	}
	return d.verifyManagedIsolation(ctx)
}

// verifyManagedIsolation confirms a custom network carries both the
// isolate=true option and the managed label. The default network keeps its
// historical non-isolated behavior and is exempt.
func (d Deps) verifyManagedIsolation(ctx context.Context) error {
	if d.Network == state.DefaultNetwork {
		return nil
	}
	got := strings.TrimSpace(run.Output(ctx, d.Runner, "podman", "network", "inspect", d.Network,
		"--format", `{{ index .Options "isolate" }}|{{ index .Labels "cs-sandbox.managed" }}`))
	if got != "true|1" {
		return fmt.Errorf("network %q exists but is not a cs-sandbox managed isolated network (isolate|managed = %q, want %q); choose a fresh network name or remove the conflicting network", d.Network, got, "true|1")
	}
	return nil
}

// Custom networks are campaign isolation boundaries. Netavark otherwise
// forwards traffic between bridge networks in the same rootless network
// namespace, so distinct subnets alone are not sufficient. The default
// network keeps its historical behavior for backward compatibility.
func networkCreateArgv(network string) []string {
	argv := []string{"podman", "network", "create"}
	if network != state.DefaultNetwork {
		argv = append(argv, "--opt", "isolate=true", "--label", "cs-sandbox.managed=1")
	}
	return append(argv, network)
}

// ReclaimNetwork removes an unused custom network that cs-sandbox created.
// It is deliberately best-effort: an attached endpoint, a VM registered by a
// different instances root, or a user-created network all make it leave the
// network in place.
func (d Deps) ReclaimNetwork(ctx context.Context) {
	if d.Network == "" || d.Network == state.DefaultNetwork {
		return
	}
	insts, _ := state.List(d.InstDir)
	for _, in := range insts {
		if state.NetworkName(in) == d.Network {
			return
		}
	}
	if entries, err := os.ReadDir(filepath.Join(paths.FCNetFor(d.Network), "hosts.d")); err == nil && len(entries) > 0 {
		return
	}
	managed := strings.TrimSpace(run.Output(ctx, d.Runner, "podman", "network", "inspect", d.Network,
		"--format", `{{ index .Labels "cs-sandbox.managed" }}`))
	if managed != "1" {
		return
	}
	if attached := strings.TrimSpace(run.Output(ctx, d.Runner, "podman", "ps", "-a",
		"--filter", "network="+d.Network, "--format", "{{.Names}}")); attached != "" {
		return
	}
	_, _ = d.Runner.Run(ctx, run.Opts{}, "podman", "network", "rm", d.Network)
}

// EnsureTierKeys generates the U and G tier keys once.
func (d Deps) EnsureTierKeys(ctx context.Context) error {
	// Key generation happens before the engine-specific Create lock. Take the
	// same host-wide lock here so two first-time creates cannot both target the
	// same key files.
	return lock.New(d.InstDir).With(func() error {
		if err := os.MkdirAll(d.TierDir, 0o700); err != nil {
			return err
		}
		for _, k := range []string{seed.TierUserKey, seed.TierAgentKey} {
			path := filepath.Join(d.TierDir, k)
			pub := path + ".pub"
			privOK, err := regularFile(path)
			if err != nil {
				return err
			}
			pubOK, err := regularFile(pub)
			if err != nil {
				return err
			}
			if privOK && pubOK {
				if err := os.Chmod(path, 0o600); err != nil {
					return err
				}
				if err := os.Chmod(pub, 0o644); err != nil {
					return err
				}
				continue
			}
			if privOK {
				// Recover a missing public half from the existing private key
				// instead of replacing the trust identity.
				res, err := d.Runner.Run(ctx, run.Opts{ReadOnly: true}, "ssh-keygen", "-y", "-f", path)
				if err != nil {
					return fmt.Errorf("recover tier public key %s: %w", k, err)
				}
				key := strings.TrimSpace(res.Stdout)
				if key == "" {
					return fmt.Errorf("recover tier public key %s: ssh-keygen returned no key", k)
				}
				data := key + "\n"
				if err := os.WriteFile(pub, []byte(data), 0o644); err != nil {
					return fmt.Errorf("write tier public key %s: %w", k, err)
				}
				continue
			}
			if pubOK {
				if err := os.Remove(pub); err != nil {
					return err
				}
			}
			comment := "user-tier@sandbox"
			if k == seed.TierAgentKey {
				comment = "agent-tier@sandbox"
			}
			if _, err := d.Runner.Run(ctx, run.Opts{}, "ssh-keygen", "-t", "ed25519", "-N", "", "-C", comment, "-f", path); err != nil {
				return fmt.Errorf("generate tier key %s: %w", k, err)
			}
		}
		return nil
	})
}

func regularFile(path string) (bool, error) {
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if !fi.Mode().IsRegular() {
		return false, fmt.Errorf("%s is not a regular file", path)
	}
	return true, nil
}
