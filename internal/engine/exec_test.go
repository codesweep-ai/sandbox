package engine

import "testing"

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
