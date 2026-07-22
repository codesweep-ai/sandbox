package seed

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// agentProfile describes one coding agent's host profile and how its auth is
// snapshotted into the per-instance seed.
type agentProfile struct {
	name     string   // "claude" | "codex": the seed subdir and ~/.cs-<name> profile dir
	credFile string   // subscription credential filename inside the profile dir
	keyVars  []string // provider env allowlist auto-captured from the host environment
	loginCmd string   // host command to sign in (used in the "no auth" advisory)
}

var agentProfiles = []agentProfile{
	{"claude", ".credentials.json", ClaudeKeyVars, "cs-claude"},
	{"codex", "auth.json", CodexKeyVars, "cs-codex login"},
}

// pathValuedCredVars are cloud credential *file paths* that can't be auto-carried
// (they point at host files); the user must place them under ~/.cs-<agent>/creds.
var pathValuedCredVars = []string{"GOOGLE_APPLICATION_CREDENTIALS", "AWS_SHARED_CREDENTIALS_FILE", "AWS_CONFIG_FILE"}

// WriteAgentAuth snapshots the host's Claude/Codex auth into the per-instance
// seed, mirroring the bash create's carry model (docs/design.md "Bundled agent
// tooling and auth"). For each agent:
//
//   - the subscription credential (~/.cs-<agent>/<credFile>) is copied to
//     <seed>/<agent>/<credFile>, carried into BOTH sandbox types and NOT gated by
//     --no-agent-keys (it's the login you signed in with on the host);
//   - unless noAgentKeys, the provider env is written to <seed>/<agent>/env — the
//     allowlisted provider vars set in the host environment, then the declarative
//     ~/.cs-<agent>/env appended last (so it wins on re-source) — and
//     ~/.cs-<agent>/creds/ is copied to <seed>/<agent>/creds.
//
// All secret material is owner-only (0600 files, 0700 dirs); the guest entrypoint
// installs it into the home volume first-boot-only. sandboxType ("user"|"agent")
// and note (may be nil) drive human-facing advisories. noAgentAuth (--no-agent-auth)
// carries nothing at all — the sandbox starts login-free; noAgentKeys (--no-agent-keys)
// carries the subscription credential but not the provider env/creds.
func WriteAgentAuth(seedDir, home, sandboxType string, noAgentAuth, noAgentKeys bool, look Lookup, note func(string)) error {
	say := func(m string) {
		if note != nil {
			note(m)
		}
	}
	if noAgentAuth {
		say("not carrying host agent auth (--no-agent-auth): the sandbox starts login-free — sign in inside it, or run 'cs-sandbox claude-login <name>' / 'codex-login <name>'")
	}
	for _, ap := range agentProfiles {
		pd := filepath.Join(home, ".cs-"+ap.name)
		sd := filepath.Join(seedDir, ap.name)
		if err := os.RemoveAll(sd); err != nil { // clear any stale carry from a prior create
			return err
		}
		if noAgentAuth {
			continue // carry nothing; the cleared dir means the entrypoint installs no login
		}

		credSeeded := false
		if cred := filepath.Join(pd, ap.credFile); fileExists(cred) {
			if err := installSecretFile(cred, filepath.Join(sd, ap.credFile)); err != nil {
				return err
			}
			credSeeded = true
		}

		keysSeeded := false
		if !noAgentKeys {
			wrote, err := seedAgentEnv(sd, pd, ap.keyVars, look)
			if err != nil {
				return err
			}
			keysSeeded = wrote
			bedrockVertexNote(ap, pd, look, say)
			if wrote && sandboxType == string(Agent) {
				say(fmt.Sprintf("note: this agent sandbox will carry your %s provider credentials — use --no-agent-keys to skip", ap.name))
			}
		}

		if !credSeeded && !keysSeeded {
			say(fmt.Sprintf("no host %s auth — not seeding (run '%s' to log in, set ~/.cs-%s/env for an API key, or 'cs-sandbox %s-login <name>' after create)",
				capitalize(ap.name), ap.loginCmd, ap.name, ap.name))
		}
	}
	return nil
}

// seedAgentEnv builds <seed>/<agent>/env from the auto-captured provider vars then
// the declarative profile env (appended last so it wins), and copies the profile
// creds/ dir. Returns whether it carried anything.
func seedAgentEnv(sd, pd string, keyVars []string, look Lookup) (bool, error) {
	var b strings.Builder
	b.WriteString(ProviderEnvLines(keyVars, look)) // (1) allowlisted vars set in the host env
	if data, err := os.ReadFile(filepath.Join(pd, "env")); err == nil {
		b.Write(data) // (2) declarative env, appended last so a re-source overrides (1)
		if n := len(data); n > 0 && data[n-1] != '\n' {
			b.WriteByte('\n')
		}
	}

	wrote := false
	if b.Len() > 0 {
		if err := writeSecretFile(filepath.Join(sd, "env"), []byte(b.String())); err != nil {
			return false, err
		}
		wrote = true
	}
	if isDir(filepath.Join(pd, "creds")) {
		if err := copyTreeSecure(filepath.Join(pd, "creds"), filepath.Join(sd, "creds")); err != nil {
			return false, err
		}
		wrote = true
	}
	return wrote, nil
}

// bedrockVertexNote warns when a Claude cloud-provider flag is set but its
// file-based credentials (a host path) weren't carried — the user must place them
// under ~/.cs-claude/{env,creds/}.
func bedrockVertexNote(ap agentProfile, pd string, look Lookup, say func(string)) {
	if ap.name != "claude" || fileExists(filepath.Join(pd, "env")) || isDir(filepath.Join(pd, "creds")) {
		return
	}
	cloudFlag := false
	for _, f := range []string{"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY"} {
		if _, ok := look(f); ok {
			cloudFlag = true
		}
	}
	if !cloudFlag {
		return
	}
	for _, p := range pathValuedCredVars {
		if _, ok := look(p); ok {
			say(fmt.Sprintf("note: carried a Bedrock/Vertex flag but not $%s (a host file path); put file creds under ~/.cs-claude/{env,creds/} so they carry into instances", p))
			return
		}
	}
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

// copyTreeSecure recursively copies src to dst, dirs at 0700 and regular files at
// 0600 (owner-only, matching the bash `chmod -R go-rwx`). Non-regular files are
// skipped.
func copyTreeSecure(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return writeSecretFile(target, data)
	})
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular()
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// capitalize upper-cases the first ASCII letter ("claude" -> "Claude").
func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
