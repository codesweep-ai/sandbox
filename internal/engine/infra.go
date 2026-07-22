package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/codesweep-ai/sandbox/internal/lock"
	"github.com/codesweep-ai/sandbox/internal/run"
	"github.com/codesweep-ai/sandbox/internal/seed"
)

// EnsureNetwork creates the shared podman network if absent (tolerates a
// parallel create that wins the race).
func (d Deps) EnsureNetwork(ctx context.Context) error {
	if _, err := d.Runner.Run(ctx, run.Opts{ReadOnly: true}, "podman", "network", "exists", d.Network); err == nil {
		return nil
	}
	_, _ = d.Runner.Run(ctx, run.Opts{}, "podman", "network", "create", d.Network)
	if _, err := d.Runner.Run(ctx, run.Opts{ReadOnly: true}, "podman", "network", "exists", d.Network); err != nil {
		return fmt.Errorf("could not create podman network %q", d.Network)
	}
	return nil
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
