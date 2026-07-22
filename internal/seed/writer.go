package seed

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/codesweep-ai/sandbox/internal/run"
)

// Input is everything needed to materialize a per-instance seed directory. The
// pure trust-model decisions are already resolved into content by the caller;
// Write mechanically lays down files with correct modes (the SSH/trust
// material; the engine-specific repos manifest and agent credentials are
// seeded separately).
type Input struct {
	Type Type
	Solo bool

	HostPubs     string // concatenated ~/.ssh/*.pub (H)
	UserTierPub  string // id_cs-sandbox_user.pub contents (U)
	AgentTierPub string // id_cs-sandbox_agent.pub contents (G)

	// Source paths of the private tier key + its .pub to install (empty for solo).
	TierName     string // "id_cs-sandbox_user" | "id_cs-sandbox_agent" | ""
	TierPrivPath string
	TierPubPath  string

	FabricGW    string // podman gateway, e.g. 10.89.0.1 (required if TierName != "")
	HostHosts   string // "ip name1 name2" line for the guest /etc/hosts (or "")
	InjectedEnv string // resolved KEY=VALUE block (or "")
	GitIdent    GitIdentity
}

// Write materializes the seed at <seedDir>, creating host_keys via ssh-keygen
// (through the Runner) if absent. Returns the generated ssh_config or an error
// (notably the fail-closed error from SSHClientConfig when the fabric gateway is
// unresolvable for a non-solo sandbox).
func Write(ctx context.Context, r run.Runner, seedDir string, in Input) error {
	hostKeys := filepath.Join(seedDir, "host_keys")
	if err := os.MkdirAll(hostKeys, 0o700); err != nil {
		return err
	}

	// authorized_keys: H (+U) (+G for agent) — identical for solo and non-solo.
	ak := AuthorizedKeys(in.HostPubs, in.UserTierPub, in.AgentTierPub, in.Type)
	if err := writeFile(seedDir, "authorized_keys", ak, 0o600); err != nil {
		return err
	}

	// tier private key (+ .pub) the sandbox ssh's out with — omitted for solo.
	if in.TierName != "" {
		if err := installFile(in.TierPrivPath, filepath.Join(seedDir, in.TierName), 0o600); err != nil {
			return err
		}
		if err := installFile(in.TierPubPath, filepath.Join(seedDir, in.TierName+".pub"), 0o644); err != nil {
			return err
		}
	}

	// host_hosts: reach the host by name from inside.
	if in.HostHosts != "" {
		if err := writeFile(seedDir, "host_hosts", []byte(in.HostHosts+"\n"), 0o644); err != nil {
			return err
		}
	}

	// in-instance ssh client config (the Match-exec peer guard).
	tierKey := TierKey(in.Type, in.Solo)
	cfg, err := SSHClientConfig(in.Type, tierKey, in.FabricGW)
	if err != nil {
		return fmt.Errorf("seed %s: %w", filepath.Base(filepath.Dir(seedDir)), err)
	}
	if err := writeFile(seedDir, "ssh_config", []byte(cfg), 0o600); err != nil {
		return err
	}

	// --env / --env-file block, or remove a stale one.
	if in.InjectedEnv != "" {
		if err := writeFile(seedDir, "inject-env", []byte(in.InjectedEnv), 0o600); err != nil {
			return err
		}
	} else {
		_ = os.Remove(filepath.Join(seedDir, "inject-env"))
	}

	// stable per-instance host keys (generated once).
	if !hostKeysExist(hostKeys) {
		if _, err := r.Run(ctx, run.Opts{}, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "", "-f", filepath.Join(hostKeys, "ssh_host_ed25519_key")); err != nil {
			return fmt.Errorf("generate host ed25519 key: %w", err)
		}
		if _, err := r.Run(ctx, run.Opts{}, "ssh-keygen", "-q", "-t", "rsa", "-b", "3072", "-N", "", "-C", "", "-f", filepath.Join(hostKeys, "ssh_host_rsa_key")); err != nil {
			return fmt.Errorf("generate host rsa key: %w", err)
		}
	}

	// git identity (non-secret).
	if gi := in.GitIdent.File(); gi != "" {
		if err := writeFile(seedDir, "git_identity", []byte(gi), 0o600); err != nil {
			return err
		}
	} else {
		_ = os.Remove(filepath.Join(seedDir, "git_identity"))
	}
	return nil
}

func hostKeysExist(dir string) bool {
	m, _ := filepath.Glob(filepath.Join(dir, "ssh_host_*_key"))
	return len(m) > 0
}

func writeFile(dir, name string, data []byte, mode os.FileMode) error {
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, mode); err != nil {
		return err
	}
	return os.Chmod(p, mode)
}

func installFile(src, dst string, mode os.FileMode) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.WriteFile(dst, data, mode); err != nil {
		return err
	}
	return os.Chmod(dst, mode)
}
