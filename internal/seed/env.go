package seed

import (
	"fmt"
	"regexp"
	"strings"
)

// Provider env vars auto-captured (when set at create time) into an instance's
// agent profile, so an API-key / cloud setup carries forward like a subscription
// login. Scalar values only — path-valued vars are deliberately omitted (they
// point at host files; carry those via ~/.cs-<agent>/{env,creds/}). Ports of
// CLAUDE_KEY_VARS / CODEX_KEY_VARS.
var ClaudeKeyVars = []string{
	"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL",
	"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY",
	"AWS_REGION", "AWS_DEFAULT_REGION", "AWS_PROFILE", "AWS_ACCESS_KEY_ID",
	"AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_BEARER_TOKEN_BEDROCK",
	"CLOUD_ML_REGION", "ANTHROPIC_VERTEX_PROJECT_ID", "GCLOUD_PROJECT",
	"GOOGLE_CLOUD_PROJECT", "ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL",
	"ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL",
}

var CodexKeyVars = []string{"OPENAI_API_KEY", "OPENAI_BASE_URL", "AZURE_OPENAI_API_KEY"}

var envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Lookup resolves an env var to (value, isSet). Injectable so tests need no
// real process environment.
type Lookup func(string) (string, bool)

// EmitEnvKV normalizes one --env/--env-file token to a KEY=VALUE line.
// "KEY=VALUE" is kept verbatim; a bare "KEY" pulls the value from the
// environment (docker --env-file style). Returns an error (to be logged as a
// note and skipped) for an invalid name or an unset pass-through var.
func EmitEnvKV(tok string, look Lookup) (string, error) {
	if i := strings.IndexByte(tok, '='); i >= 0 {
		k := tok[:i]
		if !envNameRe.MatchString(k) {
			return "", fmt.Errorf("ignoring invalid env entry: %s", tok)
		}
		return tok, nil
	}
	k := tok
	if !envNameRe.MatchString(k) {
		return "", fmt.Errorf("ignoring invalid env name: %s", tok)
	}
	v, ok := look(k)
	if !ok {
		return "", fmt.Errorf("ignoring --env %s: not set in the environment", k)
	}
	return k + "=" + v, nil
}

// ResolveInjectedEnv builds the single KEY=VALUE block written to the seed
// inject-env file from the ordered --env tokens and --env-file line sets.
// File lines: '#' comments and blank lines are ignored; each remaining line is
// a token (KEY=VALUE or bare KEY). Warnings for skipped tokens are returned for
// the caller to surface. The block has no trailing newline beyond each line's.
func ResolveInjectedEnv(envTokens []string, fileLineSets [][]string, look Lookup) (block string, warnings []string) {
	var b strings.Builder
	emit := func(tok string) {
		line, err := EmitEnvKV(tok, look)
		if err != nil {
			warnings = append(warnings, err.Error())
			return
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	for _, t := range envTokens {
		emit(t)
	}
	for _, lines := range fileLineSets {
		for _, raw := range lines {
			line := strings.TrimSpace(raw)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			emit(line)
		}
	}
	return b.String(), warnings
}

// ProviderEnvLines returns `export VAR=<shell-quoted-value>` lines for each
// allowlisted var that is set, in allowlist order. The building block of the
// agent env's provider-var auto-capture.
func ProviderEnvLines(allowlist []string, look Lookup) string {
	var b strings.Builder
	for _, v := range allowlist {
		if val, ok := look(v); ok {
			fmt.Fprintf(&b, "export %s=%s\n", v, shellQuote(val))
		}
	}
	return b.String()
}

// shellQuote single-quotes a value for safe `source`ing (POSIX-correct).
// Single-quote quoting is byte-different from printf %q for some values but
// semantically equivalent when sourced.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
