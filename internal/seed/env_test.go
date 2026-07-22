package seed

import (
	"strings"
	"testing"
)

func lookup(m map[string]string) Lookup {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestEmitEnvKV(t *testing.T) {
	env := lookup(map[string]string{"FOO": "bar", "EMPTY": ""})
	cases := []struct {
		tok     string
		want    string
		wantErr bool
	}{
		{"KEY=value", "KEY=value", false},
		{"KEY=with=eq", "KEY=with=eq", false}, // only first '=' splits the name
		{"FOO", "FOO=bar", false},             // bare key pulls from env
		{"EMPTY", "EMPTY=", false},            // set-but-empty passes through
		{"UNSET", "", true},                   // bare unset => skipped
		{"1BAD=x", "", true},                  // invalid name
		{"has space=x", "", true},             // invalid name
	}
	for _, c := range cases {
		got, err := EmitEnvKV(c.tok, env)
		if (err != nil) != c.wantErr {
			t.Errorf("EmitEnvKV(%q) err=%v wantErr=%v", c.tok, err, c.wantErr)
			continue
		}
		if got != c.want {
			t.Errorf("EmitEnvKV(%q) = %q, want %q", c.tok, got, c.want)
		}
	}
}

func TestResolveInjectedEnv(t *testing.T) {
	env := lookup(map[string]string{"HOST_ONLY": "hv"})
	block, warns := ResolveInjectedEnv(
		[]string{"A=1", "HOST_ONLY", "BAD NAME=x"},
		[][]string{{"# comment", "", "B=2", "  C=3  "}},
		env,
	)
	want := "A=1\nHOST_ONLY=hv\nB=2\nC=3\n"
	if block != want {
		t.Errorf("block = %q, want %q", block, want)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "BAD NAME") {
		t.Errorf("expected one warning about BAD NAME, got %v", warns)
	}
}

func TestProviderEnvLines(t *testing.T) {
	env := lookup(map[string]string{
		"ANTHROPIC_API_KEY": "sk-abc",
		"AWS_REGION":        "us-west-2",
		"IGNORED":           "x",
	})
	got := ProviderEnvLines(ClaudeKeyVars, env)
	if !strings.Contains(got, "export ANTHROPIC_API_KEY='sk-abc'") {
		t.Errorf("missing quoted key line:\n%s", got)
	}
	if !strings.Contains(got, "export AWS_REGION='us-west-2'") {
		t.Errorf("missing region line:\n%s", got)
	}
	if strings.Contains(got, "IGNORED") {
		t.Errorf("non-allowlisted var leaked:\n%s", got)
	}
	// allowlist order is preserved: ANTHROPIC_API_KEY precedes AWS_REGION.
	if strings.Index(got, "ANTHROPIC_API_KEY") > strings.Index(got, "AWS_REGION") {
		t.Errorf("allowlist order not preserved:\n%s", got)
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"":       "''",
		"simple": "'simple'",
		"a b":    "'a b'",
		"it's":   `'it'\''s'`,
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}
