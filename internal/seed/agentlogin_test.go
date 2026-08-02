package seed

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeProfile builds a host ~/.cs-<agent> profile with the given files.
func writeProfile(t *testing.T, home, agent string, files map[string]string) {
	t.Helper()
	pd := filepath.Join(home, ".cs-"+agent)
	for name, content := range files {
		p := filepath.Join(pd, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// TestWriteAgentLoginsInheritsRequested: each requested agent's login is snapshotted
// into the seed at 0600, and reported back to the caller.
func TestWriteAgentLoginsInheritsRequested(t *testing.T) {
	home, seedDir := t.TempDir(), t.TempDir()
	writeProfile(t, home, "claude", map[string]string{".credentials.json": `{"tok":"max"}`})
	writeProfile(t, home, "codex", map[string]string{"auth.json": `{"tok":"chatgpt"}`})
	writeProfile(t, home, "opencode", map[string]string{"auth.json": `{"tok":"oc"}`})

	carried, err := WriteAgentLogins(seedDir, home, []string{"claude", "codex", "opencode"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(carried, ",") != "claude,codex,opencode" {
		t.Errorf("carried = %v, want all three agents", carried)
	}
	for _, c := range []struct{ agent, file string }{
		{"claude", ".credentials.json"}, {"codex", "auth.json"}, {"opencode", "auth.json"},
	} {
		p := filepath.Join(seedDir, c.agent, c.file)
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatalf("%s not seeded: %v", p, err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %o, want 600", p, fi.Mode().Perm())
		}
	}
}

// TestWriteAgentLoginsIsOptIn: nothing is carried by default, and asking for one
// agent never carries the other.
func TestWriteAgentLoginsIsOptIn(t *testing.T) {
	home := t.TempDir()
	writeProfile(t, home, "claude", map[string]string{".credentials.json": `{"tok":"max"}`})
	writeProfile(t, home, "codex", map[string]string{"auth.json": `{"tok":"chatgpt"}`})
	writeProfile(t, home, "opencode", map[string]string{"auth.json": `{"tok":"oc"}`})

	seedDir := t.TempDir()
	carried, err := WriteAgentLogins(seedDir, home, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(carried) != 0 {
		t.Errorf("carried %v with no --inherit-agent-login, want nothing", carried)
	}
	if _, err := os.Stat(filepath.Join(seedDir, "claude", ".credentials.json")); err == nil {
		t.Error("a login was seeded without being asked for")
	}

	seed2 := t.TempDir()
	carried, err = WriteAgentLogins(seed2, home, []string{"claude"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(carried, ",") != "claude" {
		t.Errorf("carried = %v, want just claude", carried)
	}
	for _, other := range []string{"codex", "opencode"} {
		if _, err := os.Stat(filepath.Join(seed2, other, "auth.json")); err == nil {
			t.Errorf("%s login carried when only claude was requested", other)
		}
	}
}

// TestWriteAgentLoginsCarriesOnlyTheCredential: the rest of a host profile stays on
// the host. It matters most for opencode, whose profile dir also holds the session
// db and a provider-key env file right next to the credential.
func TestWriteAgentLoginsCarriesOnlyTheCredential(t *testing.T) {
	home, seedDir := t.TempDir(), t.TempDir()
	writeProfile(t, home, "opencode", map[string]string{
		"auth.json":     `{"tok":"oc"}`,
		"env":           "FIREWORKS_API_KEY=secret",
		"opencode.db":   "sqlite",
		"opencode.json": `{"model":"p/m"}`,
	})

	if _, err := WriteAgentLogins(seedDir, home, []string{"opencode"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(seedDir, "opencode", "auth.json")); err != nil {
		t.Fatalf("credential not seeded: %v", err)
	}
	for _, leaked := range []string{"env", "opencode.db", "opencode.json"} {
		if _, err := os.Stat(filepath.Join(seedDir, "opencode", leaked)); !os.IsNotExist(err) {
			t.Errorf("%s carried into the seed alongside the credential", leaked)
		}
	}
}

// TestWriteAgentLoginsNoHostLogin: asking to inherit a login the host doesn't have
// is not an error — it advises how to sign in instead, naming that agent's own
// login command.
func TestWriteAgentLoginsNoHostLogin(t *testing.T) {
	for _, tc := range []struct{ agent, wantMissing, wantLoginCmd string }{
		{"claude", "no host Claude login", "cs-claude"},
		{"codex", "no host Codex login", "cs-codex login"},
		{"opencode", "no host Opencode login", "cs-opencode providers login"},
	} {
		t.Run(tc.agent, func(t *testing.T) {
			home, seedDir := t.TempDir(), t.TempDir()
			var notes []string
			carried, err := WriteAgentLogins(seedDir, home, []string{tc.agent}, func(m string) { notes = append(notes, m) })
			if err != nil {
				t.Fatal(err)
			}
			if len(carried) != 0 {
				t.Errorf("carried = %v, want nothing", carried)
			}
			joined := strings.Join(notes, "\n")
			if !strings.Contains(joined, tc.wantMissing) || !strings.Contains(joined, tc.wantLoginCmd) ||
				!strings.Contains(joined, "agent-login") {
				t.Errorf("expected an advisory naming %q and agent-login, got:\n%s", tc.wantLoginCmd, joined)
			}
		})
	}
}

// TestWriteAgentLoginsClearsStale: re-creating a sandbox that no longer inherits a
// login must not leave the previous carry behind in the seed.
func TestWriteAgentLoginsClearsStale(t *testing.T) {
	home, seedDir := t.TempDir(), t.TempDir()
	writeProfile(t, home, "claude", map[string]string{".credentials.json": `{"tok":"max"}`})

	if _, err := WriteAgentLogins(seedDir, home, []string{"claude"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(seedDir, "claude", ".credentials.json")); err != nil {
		t.Fatalf("setup: login not seeded: %v", err)
	}
	if _, err := WriteAgentLogins(seedDir, home, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(seedDir, "claude")); !os.IsNotExist(err) {
		t.Error("stale carry survived a create that inherits nothing")
	}
}

// TestAgentNamesAndValidAgent pin the set the CLI validates --inherit-agent-login against.
func TestAgentNamesAndValidAgent(t *testing.T) {
	if got := strings.Join(AgentNames(), ","); got != "claude,codex,opencode" {
		t.Errorf("AgentNames() = %s", got)
	}
	for _, ok := range AgentNames() {
		if !ValidAgent(ok) {
			t.Errorf("ValidAgent(%q) = false", ok)
		}
	}
	if ValidAgent("bogus") {
		t.Error("ValidAgent(bogus) should be false")
	}
}
