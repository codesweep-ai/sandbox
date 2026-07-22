package hostenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeKey(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPubKeysAndIdentity(t *testing.T) {
	home := t.TempDir()
	ssh := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(ssh, 0o700); err != nil {
		t.Fatal(err)
	}
	// id_ed25519 has a matching private key; id_orphan.pub does not.
	writeKey(t, ssh, "id_ed25519.pub", "ssh-ed25519 AAAAA me@host\n")
	writeKey(t, ssh, "id_ed25519", "PRIVATE\n")
	writeKey(t, ssh, "id_orphan.pub", "ssh-ed25519 BBBBB orphan@host\n")

	h := Host{Home: home}

	keys, ok := h.PubKeys()
	if !ok {
		t.Fatal("expected PubKeys ok")
	}
	if !strings.Contains(keys, "AAAAA") || !strings.Contains(keys, "BBBBB") {
		t.Errorf("PubKeys should include both .pub files:\n%s", keys)
	}

	lines := h.IdentityLines()
	if !strings.Contains(lines, "IdentityFile ~/.ssh/id_ed25519") {
		t.Errorf("IdentityLines should include the key with a private half:\n%s", lines)
	}
	if strings.Contains(lines, "id_orphan") {
		t.Errorf("IdentityLines must skip a .pub with no private key:\n%s", lines)
	}

	block := h.IdentityBlock()
	if !strings.HasSuffix(block, "IdentitiesOnly yes") {
		t.Errorf("IdentityBlock should end with IdentitiesOnly yes:\n%s", block)
	}
}

// A key filename with a space would otherwise emit an unquoted IdentityFile,
// which makes ssh reject the whole config file.
func TestIdentityLinesQuotesSpacedKeyName(t *testing.T) {
	home := t.TempDir()
	ssh := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(ssh, 0o700); err != nil {
		t.Fatal(err)
	}
	writeKey(t, ssh, "my key.pub", "ssh-ed25519 AAAAA me@host\n")
	writeKey(t, ssh, "my key", "PRIVATE\n")

	lines := Host{Home: home}.IdentityLines()
	if !strings.Contains(lines, `IdentityFile "~/.ssh/my key"`) {
		t.Errorf("spaced key name should be quoted:\n%s", lines)
	}
}

func TestQuoteConfigArg(t *testing.T) {
	// Unspaced paths stay bare — quoting only where it is needed keeps the
	// generated config diff-free on Linux.
	if got := QuoteConfigArg("/home/dev/.ssh/id"); got != "/home/dev/.ssh/id" {
		t.Errorf("unspaced arg should be untouched, got %q", got)
	}
	if got := QuoteConfigArg("/a b/c"); got != `"/a b/c"` {
		t.Errorf("spaced arg should be quoted, got %q", got)
	}
	if got := QuoteConfigArg("/a\tb"); got != "\"/a\tb\"" {
		t.Errorf("tabbed arg should be quoted, got %q", got)
	}
	if got := QuoteConfigArg(`/a "quoted"\b`); got != `"/a \"quoted\"\\b"` {
		t.Errorf("quotes and backslashes should be escaped, got %q", got)
	}
}

func TestPubKeysEmpty(t *testing.T) {
	home := t.TempDir()
	_ = os.MkdirAll(filepath.Join(home, ".ssh"), 0o700)
	h := Host{Home: home}
	if _, ok := h.PubKeys(); ok {
		t.Error("expected ok=false with no keys")
	}
	if h.IdentityBlock() != "" {
		t.Error("IdentityBlock should be empty with no keys")
	}
}

func TestSSHPaths(t *testing.T) {
	h := Host{Home: "/home/u"}
	if h.SSHConfigFile() != "/home/u/.ssh/config.d/cs-sandbox" {
		t.Errorf("SSHConfigFile = %s", h.SSHConfigFile())
	}
}
