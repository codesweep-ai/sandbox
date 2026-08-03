package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/codesweep-ai/sandbox/internal/fcnet"
	"github.com/codesweep-ai/sandbox/internal/lock"
	"github.com/codesweep-ai/sandbox/internal/paths"
	"github.com/codesweep-ai/sandbox/internal/run"
	"github.com/codesweep-ai/sandbox/internal/seed"
	"github.com/codesweep-ai/sandbox/internal/state"
)

// EnsureNetwork creates the shared podman network if absent (tolerates a
// parallel create that wins the race).
func (d Deps) EnsureNetwork(ctx context.Context) error {
	if _, err := d.Runner.Run(ctx, run.Opts{ReadOnly: true}, "podman", "network", "exists", d.Network); err == nil {
		verr := d.verifyManagedIsolation(ctx)
		if verr == nil {
			return nil
		}
		return d.healDefaultFabric(ctx, verr)
	}
	_, _ = d.Runner.Run(ctx, run.Opts{}, networkCreateArgv(d.Network)...)
	if _, err := d.Runner.Run(ctx, run.Opts{ReadOnly: true}, "podman", "network", "exists", d.Network); err != nil {
		return fmt.Errorf("could not create podman network %q", d.Network)
	}
	return d.verifyManagedIsolation(ctx)
}

// networkCreateArgv builds the create command for a group's network. Every
// group network is isolated: netavark otherwise forwards traffic between
// bridges in the same rootless namespace, so separate bridges and subnets alone
// are NOT a boundary. isolate=true blocks that forwarding without the loss of
// outbound internet that --internal would cause.
func networkCreateArgv(network string) []string {
	return []string{"podman", "network", "create",
		"--opt", "isolate=true", "--label", "cs-sandbox.managed=1", network}
}

// verifyManagedIsolation confirms a network we are about to use really is an
// isolated network we created. Adopting a pre-existing bridge of unknown
// provenance would silently void the boundary every group member relies on, so
// this fails closed rather than trusting the name.
func (d Deps) verifyManagedIsolation(ctx context.Context) error {
	got := strings.TrimSpace(run.Output(ctx, d.Runner, "podman", "network", "inspect", d.Network,
		"--format", `{{ index .Options "isolate" }}|{{ index .Labels "cs-sandbox.managed" }}`))
	if got != "true|1" {
		return fmt.Errorf("network %q exists but is not a cs-sandbox managed isolated network "+
			"(isolate|managed = %q, want \"true|1\"); choose a different group name or remove that network",
			d.Network, got)
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

// healDefaultFabric upgrades a pre-isolation default fabric in place. isolate is
// a creation-time option, so the only way to add it is to recreate the network.
//
// Only the default fabric is ever healed. An unlabelled network bearing a
// group's name may well be the user's own, and silently deleting it would be far
// worse than refusing — so named groups keep failing closed.
//
// Measured, and the reason this is not simply exempted: isolate=true does NOT
// block a non-isolated peer. A pre-isolation default fabric is therefore
// reachable from every group, which would make it a bridge between all of them.
func (d Deps) healDefaultFabric(ctx context.Context, verr error) error {
	if d.Network != state.NetworkName(state.DefaultGroup) {
		return verr
	}
	keepalive := fcnet.KeepaliveFor(d.Network)
	var others []string
	out := run.Output(ctx, d.Runner, "podman", "ps", "-a",
		"--filter", "network="+d.Network, "--format", "{{.Names}}")
	for _, n := range strings.Split(strings.TrimSpace(out), "\n") {
		if n = strings.TrimSpace(n); n != "" && n != keepalive {
			others = append(others, n)
		}
	}
	if len(others) > 0 {
		return fmt.Errorf("the default network %q predates group isolation and must be recreated, "+
			"but %d sandbox(es) are still attached (%s); destroy them first, then retry",
			d.Network, len(others), strings.Join(others, ", "))
	}
	d.note("upgrading the default network to an isolated one (it predates group isolation)")
	// Tear the fabric down FIRST. Its dnsmasq holds an address on netavark's own
	// bridge, and an address left there keeps the bridge alive past the network's
	// removal — carrying the old gateway with it. The recreated network gets a
	// fresh subnet, podman later hands the freed one to some other network on a
	// different bridge, and two bridges then answer for the same subnet: that
	// network's members lose outbound traffic for reasons nothing in podman
	// explains. Down is a no-op when there is no fabric to stop.
	fcnet.Fabric{
		Runner: d.Runner, Network: d.Network,
		NetDir: paths.FCNetFor(state.DefaultGroup),
	}.Down(ctx)
	_, _ = d.Runner.Run(ctx, run.Opts{}, "podman", "rm", "-f", keepalive)
	if _, err := d.Runner.Run(ctx, run.Opts{}, "podman", "network", "rm", "-f", d.Network); err != nil {
		return fmt.Errorf("could not remove the pre-isolation default network: %w", err)
	}
	_, _ = d.Runner.Run(ctx, run.Opts{}, networkCreateArgv(d.Network)...)
	return d.verifyManagedIsolation(ctx)
}

// ReclaimNetwork removes a group's network once nothing references it. A group
// owns its network, so this is a definite operation rather than the best-effort
// sweep it would have to be if membership were only inferred from members.
func (d Deps) ReclaimNetwork(ctx context.Context) {
	if d.Network == "" || d.Network == state.NetworkName(state.DefaultGroup) {
		return // the default fabric is shared host-wide and never reclaimed
	}
	_, _ = d.Runner.Run(ctx, run.Opts{}, "podman", "network", "rm", "-f", d.Network)
}

// RemoveGateway tears down a group's keepalive/gateway container.
func (d Deps) RemoveGateway(ctx context.Context) {
	_, _ = d.Runner.Run(ctx, run.Opts{}, "podman", "rm", "-f", fcnet.KeepaliveFor(d.Network))
}
