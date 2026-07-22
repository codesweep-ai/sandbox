package seed

import (
	"strings"
	"testing"
)

const (
	hostPub  = "ssh-ed25519 AAAAHOST host@laptop\n"
	userPub  = "ssh-ed25519 AAAAUSER user-tier@sandbox\n"
	agentPub = "ssh-ed25519 AAAAAGENT agent-tier@sandbox\n"
)

// The trust matrix: G (agent tier) must NEVER be authorized in
// a user sandbox; a --solo agent must hold NO tier private key.
func TestTrustMatrix(t *testing.T) {
	t.Run("user sandbox authorizes H+U, never G", func(t *testing.T) {
		ak := string(AuthorizedKeys(hostPub, userPub, agentPub, User))
		if !strings.Contains(ak, "AAAAHOST") || !strings.Contains(ak, "AAAAUSER") {
			t.Fatalf("user authorized_keys must contain H and U:\n%s", ak)
		}
		if strings.Contains(ak, "AAAAAGENT") {
			t.Fatalf("SECURITY: agent key G leaked into a user sandbox:\n%s", ak)
		}
	})

	t.Run("agent sandbox authorizes H+U+G", func(t *testing.T) {
		ak := string(AuthorizedKeys(hostPub, userPub, agentPub, Agent))
		for _, want := range []string{"AAAAHOST", "AAAAUSER", "AAAAAGENT"} {
			if !strings.Contains(ak, want) {
				t.Fatalf("agent authorized_keys missing %s:\n%s", want, ak)
			}
		}
	})

	t.Run("tier key selection", func(t *testing.T) {
		cases := []struct {
			typ  Type
			solo bool
			want string
		}{
			{User, false, TierUserKey},
			{Agent, false, TierAgentKey},
			{Agent, true, ""}, // --solo: no outbound key
			{User, true, ""},  // solo user (rejected at CLI, but the rule holds)
		}
		for _, c := range cases {
			if got := TierKey(c.typ, c.solo); got != c.want {
				t.Errorf("TierKey(%s, solo=%v) = %q, want %q", c.typ, c.solo, got, c.want)
			}
		}
	})

	t.Run("solo agent authorized_keys is still normal (inbound allowed)", func(t *testing.T) {
		// authorized_keys does not depend on solo — peers/host can still ssh IN.
		ak := string(AuthorizedKeys(hostPub, userPub, agentPub, Agent))
		if !strings.Contains(ak, "AAAAAGENT") {
			t.Fatalf("solo agent must still be reachable (normal authorized_keys):\n%s", ak)
		}
	})
}

func TestSSHClientConfig(t *testing.T) {
	t.Run("agent gets guard + publickey", func(t *testing.T) {
		cfg, err := SSHClientConfig(Agent, TierAgentKey, "10.89.0.1")
		if err != nil {
			t.Fatal(err)
		}
		mustContain(t, cfg, "Match exec")
		mustContain(t, cfg, "10[.]89[.]0[.]") // fabric regexp anchor
		mustContain(t, cfg, "IdentitiesOnly yes")
		mustContain(t, cfg, "IdentityFile ~/.ssh/"+TierAgentKey)
		mustContain(t, cfg, "PreferredAuthentications publickey")
		mustContain(t, cfg, "StrictHostKeyChecking accept-new")
	})

	t.Run("user omits publickey pin", func(t *testing.T) {
		cfg, err := SSHClientConfig(User, TierUserKey, "10.89.0.1")
		if err != nil {
			t.Fatal(err)
		}
		mustContain(t, cfg, "IdentityFile ~/.ssh/"+TierUserKey)
		if strings.Contains(cfg, "PreferredAuthentications") {
			t.Fatalf("user config must NOT pin PreferredAuthentications:\n%s", cfg)
		}
	})

	t.Run("solo sandbox omits guard + IdentityFile", func(t *testing.T) {
		// No tier key => no Match-exec guard, no IdentityFile pin. Gateway may be empty.
		cfg, err := SSHClientConfig(Agent, "", "")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(cfg, "Match exec") || strings.Contains(cfg, "IdentityFile") {
			t.Fatalf("solo config must omit the guard and IdentityFile:\n%s", cfg)
		}
		// Agent still pins publickey so an outbound attempt fails fast.
		mustContain(t, cfg, "PreferredAuthentications publickey")
	})

	t.Run("fail-closed when gateway is not resolvable and a tier key is held", func(t *testing.T) {
		// Never emit a config that silently omits the guard (would let ssh -A leak H to peers).
		if _, err := SSHClientConfig(Agent, TierAgentKey, "not-an-ip"); err == nil {
			t.Fatal("expected an error when the fabric gateway is unresolvable for a non-solo sandbox")
		}
	})
}

func TestFabricRegexp(t *testing.T) {
	cases := map[string]string{
		"10.89.0.1":   "^10[.]89[.]0[.]",
		"192.168.5.1": "^192[.]168[.]5[.]",
	}
	for gw, want := range cases {
		if got := fabricRegexp(gw); got != want {
			t.Errorf("fabricRegexp(%s) = %q, want %q", gw, got, want)
		}
	}
}

func TestGitIdentityFile(t *testing.T) {
	if got := (GitIdentity{}).File(); got != "" {
		t.Errorf("empty identity should render nothing, got %q", got)
	}
	got := GitIdentity{Name: "Ada", Email: "ada@x.io"}.File()
	want := "name\tAda\nemail\tada@x.io\n"
	if got != want {
		t.Errorf("File() = %q, want %q", got, want)
	}
	got = GitIdentity{Name: "Ada\nLovelace", Email: "ada\t@example.test"}.File()
	want = "name\tAda Lovelace\nemail\tada @example.test\n"
	if got != want {
		t.Errorf("File() should keep one record per line: got %q, want %q", got, want)
	}
}

func mustContain(t *testing.T, s, sub string) {
	t.Helper()
	if !strings.Contains(s, sub) {
		t.Fatalf("expected config to contain %q:\n%s", sub, s)
	}
}
