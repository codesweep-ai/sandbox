// Package covemit emits behavioral-coverage run-records from this repo's
// tests, for consumption by a downstream harness that tracks a coverage
// matrix whose cells are filled only by executed proofs. This repo's
// contract tests (which execute the shipped remote-tool scripts) and seed
// tests are that matrix's "scripts"/"unit" tier evidence. Emission is
// entirely opt-in: records are written only when CS_SANDBOX_COVERAGE_LOG
// names a file to append them to; ordinary test runs write nothing.
//
// The JSONL record schema is owned by the consumer; records carry repo
// "sandbox" and this repository's HEAD commit.
package covemit

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// TB is the slice of *testing.T used here.
type TB interface {
	Helper()
	Name() string
	Fatalf(format string, args ...any)
}

type record struct {
	Behavior string `json:"behavior"`
	Adapter  string `json:"adapter,omitempty"`
	Role     string `json:"role,omitempty"`
	Tier     string `json:"tier"`
	Test     string `json:"test"`
	Repo     string `json:"repo"`
	Commit   string `json:"commit"`
	Time     string `json:"time"`
}

// Prove appends one proof record to $CS_SANDBOX_COVERAGE_LOG; a no-op when the
// variable is unset. Call it immediately after the assertion that proves the
// behavior. Behavior ids are validated downstream against the consumer's
// rubric when the log is folded.
func Prove(t TB, behavior, adapter, role, tier string) {
	t.Helper()
	buffer := logPath()
	if buffer == "" {
		return
	}
	name := t.Name()
	if i := strings.IndexByte(name, '/'); i >= 0 {
		name = name[:i]
	}
	rec := record{Behavior: behavior, Adapter: adapter, Role: role, Tier: tier,
		Test: name, Repo: "sandbox", Commit: headCommit(), Time: time.Now().UTC().Format(time.RFC3339)}
	line, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("covemit: %v", err)
	}
	f, err := os.OpenFile(buffer, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("covemit: %v", err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		t.Fatalf("covemit: %v", err)
	}
}

// logPath is the file to append records to, or "" when emission is off.
func logPath() string {
	if p := os.Getenv("CS_SANDBOX_COVERAGE_LOG"); p != "" {
		return p
	}
	// Undocumented compatibility shim for the first consumer, whose build sets
	// this name. Honouring it keeps that build working untouched, and matters
	// more than it looks: dropping the name outright would not fail, it would
	// silently stop emitting, leaving a coverage matrix quietly empty — the
	// inverse of the mistake this package exists to prevent. Remove once no
	// caller sets it.
	return os.Getenv("COVMAP_BUFFER")
}

var (
	once   sync.Once
	commit string
)

func headCommit() string {
	once.Do(func() {
		out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output()
		if err != nil {
			commit = "unknown"
			return
		}
		commit = strings.TrimSpace(string(out))
	})
	return commit
}
