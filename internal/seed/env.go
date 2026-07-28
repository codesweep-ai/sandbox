package seed

import (
	"fmt"
	"regexp"
	"strings"
)

// envNameRe validates an environment variable name from --env / --env-file.
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
