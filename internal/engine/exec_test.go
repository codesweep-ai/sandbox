package engine

import (
	"strings"
	"testing"

	"github.com/codesweep-ai/sandbox/internal/hostenv"
	"github.com/codesweep-ai/sandbox/internal/state"
)

// TestShellQuote: `exec` means "run this argv", on both engines. The podman
// engine gets that for free — it hands the vector to `podman exec`. The
// firecracker engine goes over ssh, which joins its command words with spaces
// and lets the remote shell re-parse them, so each word has to survive that
// round trip byte for byte or argument boundaries are silently lost.
func TestShellQuote(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"plain", `'plain'`},
		{"one two", `'one two'`},
		{"$HOME", `'$HOME'`},
		{"a;b|c", `'a;b|c'`},
		{"*.go", `'*.go'`},
		{"it's", `'it'\''s'`},
	} {
		if got := shellQuote(tc.in); got != tc.want {
			t.Errorf("shellQuote(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

// An ssh-borne exec must key known_hosts by the host-global object name. Two
// groups holding the same fixture present different host keys, so one alias for
// both fails the second connection with "host key changed" — under BatchMode,
// with nobody there to accept the new key. README walkthrough 8 runs exactly
// that pair.
func TestSSHArgsKeysHostKeyOnTheObjectName(t *testing.T) {
	alias := func(group string) string {
		fe := NewFirecracker(Deps{Group: group, Host: hostenv.Host{User: "dev"}})
		args := fe.sshArgs("api", 2200)
		for i := 0; i < len(args)-1; i++ {
			if args[i] == "-o" && strings.HasPrefix(args[i+1], "HostKeyAlias=") {
				return strings.TrimPrefix(args[i+1], "HostKeyAlias=")
			}
		}
		return ""
	}
	seen := map[string]string{}
	for _, group := range []string{"cache-redis", "cache-memory", state.DefaultGroup} {
		got := alias(group)
		if want := state.ObjectName(group, "api"); got != want {
			t.Errorf("group %q: HostKeyAlias %q, want %q", group, got, want)
		}
		if other, dup := seen[got]; dup {
			t.Errorf("groups %q and %q collide on HostKeyAlias %q", other, group, got)
		}
		seen[got] = group
	}
	// An unset group is the default group, and has to reach the same entry.
	if got, want := alias(""), state.ObjectName(state.DefaultGroup, "api"); got != want {
		t.Errorf("unset group: HostKeyAlias %q, want %q", got, want)
	}
}
