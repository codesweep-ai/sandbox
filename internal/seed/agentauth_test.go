package seed

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeHome builds a host home with the given ~/.cs-<agent> profile files and
// returns a Lookup over the provided env.
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

func envLookup(env map[string]string) Lookup {
	return func(k string) (string, bool) { v, ok := env[k]; return v, ok }
}

// TestWriteAgentAuthSubscription: a host subscription credential is snapshotted
// into the seed at 0600, for both Claude and Codex, regardless of --no-agent-keys.
func TestWriteAgentAuthSubscription(t *testing.T) {
	home := t.TempDir()
	writeProfile(t, home, "claude", map[string]string{".credentials.json": `{"tok":"max"}`})
	writeProfile(t, home, "codex", map[string]string{"auth.json": `{"tok":"chatgpt"}`})
	seedDir := t.TempDir()

	// noAgentKeys=true must NOT suppress the subscription credential.
	if err := WriteAgentAuth(seedDir, home, "agent", false, true, envLookup(nil), nil); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ agent, file, want string }{
		{"claude", ".credentials.json", "max"},
		{"codex", "auth.json", "chatgpt"},
	} {
		p := filepath.Join(seedDir, tc.agent, tc.file)
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("%s not seeded: %v", tc.agent, err)
		}
		if !strings.Contains(string(data), tc.want) {
			t.Errorf("%s content = %q, want %q", p, data, tc.want)
		}
		if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %o, want 600", p, fi.Mode().Perm())
		}
	}
}

// TestWriteAgentAuthProviderEnv: without a subscription, the provider env is
// carried — auto-captured host vars first, then the declarative profile env
// appended (so it wins on re-source) — and --no-agent-keys suppresses all of it.
func TestWriteAgentAuthProviderEnv(t *testing.T) {
	home := t.TempDir()
	writeProfile(t, home, "claude", map[string]string{"env": "ANTHROPIC_API_KEY=from-file\n"})
	env := map[string]string{"ANTHROPIC_API_KEY": "from-env", "ANTHROPIC_BASE_URL": "https://x"}
	seedDir := t.TempDir()

	if err := WriteAgentAuth(seedDir, home, "agent", false, false, envLookup(env), nil); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(seedDir, "claude", "env"))
	if err != nil {
		t.Fatalf("claude env not seeded: %v", err)
	}
	s := string(got)
	// Auto-captured var present...
	if !strings.Contains(s, "export ANTHROPIC_BASE_URL=") {
		t.Errorf("missing auto-captured ANTHROPIC_BASE_URL:\n%s", s)
	}
	// ...and the declarative file is appended AFTER the auto-captured lines (the
	// auto-captured value is shell-quoted: export ANTHROPIC_API_KEY='from-env').
	if i, j := strings.Index(s, "export ANTHROPIC_API_KEY="), strings.Index(s, "ANTHROPIC_API_KEY=from-file"); !(i >= 0 && j > i) {
		t.Errorf("declarative env must be appended last (auto line %d, file line %d):\n%s", i, j, s)
	}

	// --no-agent-keys suppresses the whole provider env.
	seed2 := t.TempDir()
	if err := WriteAgentAuth(seed2, home, "agent", false, true, envLookup(env), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(seed2, "claude", "env")); !os.IsNotExist(err) {
		t.Errorf("--no-agent-keys should not write claude/env (err=%v)", err)
	}
}

// TestWriteAgentAuthCredsDir: a profile creds/ dir is copied owner-only.
func TestWriteAgentAuthCredsDir(t *testing.T) {
	home := t.TempDir()
	writeProfile(t, home, "claude", map[string]string{"creds/sa.json": "service-account"})
	seedDir := t.TempDir()

	if err := WriteAgentAuth(seedDir, home, "agent", false, false, envLookup(nil), nil); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(seedDir, "claude", "creds", "sa.json")
	data, err := os.ReadFile(p)
	if err != nil || string(data) != "service-account" {
		t.Fatalf("creds not carried: %v (%q)", err, data)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Errorf("carried cred mode = %o, want 600", fi.Mode().Perm())
	}
}

// TestWriteAgentAuthNoAuthNote: with no host auth at all, nothing is seeded and a
// per-agent advisory is emitted.
func TestWriteAgentAuthNoAuthNote(t *testing.T) {
	home := t.TempDir() // empty: no profiles
	seedDir := t.TempDir()
	var notes []string
	if err := WriteAgentAuth(seedDir, home, "agent", false, false, envLookup(nil), func(m string) { notes = append(notes, m) }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(seedDir, "claude")); !os.IsNotExist(err) {
		t.Errorf("nothing should be seeded for claude with no host auth")
	}
	joined := strings.Join(notes, "\n")
	for _, want := range []string{"no host Claude auth", "no host Codex auth"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing advisory %q in:\n%s", want, joined)
		}
	}
}

// TestWriteAgentAuthNoAgentAuth: --no-agent-auth carries nothing at all — even a
// present host subscription is not seeded — and clears any prior carry.
func TestWriteAgentAuthNoAgentAuth(t *testing.T) {
	home := t.TempDir()
	writeProfile(t, home, "claude", map[string]string{".credentials.json": `{"tok":"max"}`})
	writeProfile(t, home, "codex", map[string]string{"auth.json": `{"tok":"chatgpt"}`})
	seedDir := t.TempDir()
	// A stale carry that must be cleared.
	stale := filepath.Join(seedDir, "claude", ".credentials.json")
	if err := os.MkdirAll(filepath.Dir(stale), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("OLD"), 0o600); err != nil {
		t.Fatal(err)
	}

	var notes []string
	if err := WriteAgentAuth(seedDir, home, "agent", true, false, envLookup(nil), func(m string) { notes = append(notes, m) }); err != nil {
		t.Fatal(err)
	}
	for _, agent := range []string{"claude", "codex"} {
		if _, err := os.Stat(filepath.Join(seedDir, agent)); !os.IsNotExist(err) {
			t.Errorf("--no-agent-auth must seed no %s auth (err=%v)", agent, err)
		}
	}
	if !strings.Contains(strings.Join(notes, "\n"), "--no-agent-auth") {
		t.Errorf("expected a --no-agent-auth advisory, got: %v", notes)
	}
}

// TestWriteAgentAuthClearsStale: a prior carry is removed when re-seeding a home
// that no longer has that auth (no stale credential lingers).
func TestWriteAgentAuthClearsStale(t *testing.T) {
	home := t.TempDir()
	seedDir := t.TempDir()
	// Pre-existing stale carry in the seed.
	stale := filepath.Join(seedDir, "claude", ".credentials.json")
	if err := os.MkdirAll(filepath.Dir(stale), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("OLD"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteAgentAuth(seedDir, home, "agent", false, false, envLookup(nil), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale credential not cleared on re-seed")
	}
}
