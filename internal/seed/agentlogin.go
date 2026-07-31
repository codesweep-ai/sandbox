package seed

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// agentProfile describes one coding agent's host profile and the credential that
// can be inherited from it.
type agentProfile struct {
	name     string // "claude" | "codex": the seed subdir and ~/.cs-<name> profile dir
	credFile string // subscription credential filename inside the profile dir
	loginCmd string // in-sandbox command to log in (used in advisories)
	fallback string // optional standard host profile dir when ~/.cs-<name> is absent
}

var agentProfiles = []agentProfile{
	{"claude", ".credentials.json", "cs-claude", ".claude"},
	{"codex", "auth.json", "cs-codex login", ""},
	// No fallback dir: opencode's personal auth.json lives in its DATA dir
	// (~/.local/share/opencode, beside the session db), and API-key envs are the
	// primary opencode path anyway — inheriting personal state stays opt-in-less.
	{"opencode", "auth.json", "cs-opencode providers login", ""},
}

// AgentNames returns the agents whose login can be inherited, in a stable order.
func AgentNames() []string {
	out := make([]string, 0, len(agentProfiles))
	for _, ap := range agentProfiles {
		out = append(out, ap.name)
	}
	return out
}

// ValidAgent reports whether name is an agent this tool knows about.
func ValidAgent(name string) bool {
	for _, ap := range agentProfiles {
		if ap.name == name {
			return true
		}
	}
	return false
}

// WriteAgentLogins snapshots the host login of each agent in `inherit` into the
// per-instance seed; every other agent's seed dir is cleared so the sandbox comes
// up login-free for it. Inheriting is opt-in (`--inherit-agent-login`), because
// copying your credentials into a sandbox — especially one an autonomous agent
// drives — should be a decision rather than a default.
//
// Only the subscription credential is ever carried. Provider API keys are not:
// use --env/--env-file (and --snapshot for credential files) if a sandbox needs
// them, which keeps that choice explicit and visible in the create command.
//
// Secrets are owner-only (0600 files, 0700 dirs); the guest init installs them
// first-boot-only. Returns the agents whose login was actually carried, for the
// caller to report. note may be nil.
func WriteAgentLogins(seedDir, home string, inherit []string, note func(string)) ([]string, error) {
	say := func(m string) {
		if note != nil {
			note(m)
		}
	}
	want := map[string]bool{}
	for _, a := range inherit {
		want[a] = true
	}

	var carried []string
	for _, ap := range agentProfiles {
		sd := filepath.Join(seedDir, ap.name)
		if err := os.RemoveAll(sd); err != nil { // clear any carry from a prior create
			return nil, err
		}
		if !want[ap.name] {
			continue
		}
		cred := filepath.Join(home, ".cs-"+ap.name, ap.credFile)
		if !fileExists(cred) && ap.fallback != "" {
			fallback := filepath.Join(home, ap.fallback, ap.credFile)
			if fileExists(fallback) {
				cred = fallback
				say(fmt.Sprintf("inheriting host %s login from ~/%s (isolated ~/.cs-%s credential not found)",
					capitalize(ap.name), ap.fallback, ap.name))
			}
		}
		if !fileExists(cred) {
			say(fmt.Sprintf("no host %s login to inherit — run '%s' on the host first, or log in with 'cs-sandbox agent-login %s <name>' after create",
				capitalize(ap.name), ap.loginCmd, ap.name))
			continue
		}
		if err := installSecretFile(cred, filepath.Join(sd, ap.credFile)); err != nil {
			return nil, err
		}
		carried = append(carried, ap.name)
	}
	return carried, nil
}

// installSecretFile copies src to dst at 0600, creating parents at 0700.
func installSecretFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return writeSecretFile(dst, data)
}

// writeSecretFile writes data to dst at 0600, creating parents at 0700.
func writeSecretFile(dst string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		return err
	}
	return os.Chmod(dst, 0o600)
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular()
}

// capitalize upper-cases the first ASCII letter ("claude" -> "Claude").
func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
