package covemit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeTB stands in for *testing.T so a proof's own record can be inspected, and
// so a Fatalf inside Prove is observable instead of ending the test.
type fakeTB struct {
	name  string
	fatal string
}

func (f *fakeTB) Helper()      {}
func (f *fakeTB) Name() string { return f.name }
func (f *fakeTB) Fatalf(format string, args ...any) {
	f.fatal = fmt.Sprintf(format, args...)
}

func readRecords(t *testing.T, path string) []record {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read buffer: %v", err)
	}
	var out []record
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		var r record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("record is not JSON: %v\n%s", err, line)
		}
		out = append(out, r)
	}
	return out
}

// TestProveWritesNothingWithoutALog is the property that lets these calls sit
// in ordinary tests: with no log configured — every normal run and CI — Prove
// must touch nothing at all. If it ever wrote to a default path, every test run
// would litter the repo.
func TestProveWritesNothingWithoutALog(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CS_SANDBOX_COVERAGE_LOG", "")
	t.Setenv("COVMAP_BUFFER", "")
	tb := &fakeTB{name: "TestSomething"}

	Prove(tb, "some-behavior", "claude", "", "unit")

	if tb.fatal != "" {
		t.Errorf("Prove failed with no log configured: %s", tb.fatal)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 0 {
		t.Errorf("Prove wrote %d file(s) with no log configured", len(ents))
	}
}

// TestProveAppendsOneRecordPerProof: the log is a running record of proofs, so
// a second call must add a line rather than replace the first — several tests
// (and several packages) append to one log in a single run.
func TestProveAppendsOneRecordPerProof(t *testing.T) {
	buf := filepath.Join(t.TempDir(), "coverage.jsonl")
	t.Setenv("CS_SANDBOX_COVERAGE_LOG", buf)

	Prove(&fakeTB{name: "TestOne"}, "interrupt", "codex", "", "scripts")
	Prove(&fakeTB{name: "TestTwo"}, "login-seeding", "", "", "unit")

	recs := readRecords(t, buf)
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2", len(recs))
	}
	if recs[0].Behavior != "interrupt" || recs[0].Adapter != "codex" ||
		recs[0].Tier != "scripts" || recs[0].Test != "TestOne" {
		t.Errorf("first record = %+v", recs[0])
	}
	if recs[1].Behavior != "login-seeding" || recs[1].Adapter != "" || recs[1].Test != "TestTwo" {
		t.Errorf("second record = %+v", recs[1])
	}
	// Every record is attributable to a repo state and a moment, or it cannot be
	// folded into a matrix later.
	for i, r := range recs {
		if r.Repo != "sandbox" || r.Commit == "" || r.Time == "" {
			t.Errorf("record %d is not attributable: %+v", i, r)
		}
	}
}

// TestProveRecordsTheParentTestName: these assertions usually live inside a
// t.Run over a table, so t.Name() is "TestX/case". The case is an implementation
// detail of the table; recording it would make the same proof look like a
// different one each time a case is renamed.
func TestProveRecordsTheParentTestName(t *testing.T) {
	buf := filepath.Join(t.TempDir(), "coverage.jsonl")
	t.Setenv("CS_SANDBOX_COVERAGE_LOG", buf)

	Prove(&fakeTB{name: "TestRemoteOutputStatusContract/cs-claude-remote-output/finished"},
		"status-contract", "claude", "", "scripts")

	recs := readRecords(t, buf)
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	if recs[0].Test != "TestRemoteOutputStatusContract" {
		t.Errorf("Test = %q, want the parent test name only", recs[0].Test)
	}
}

// TestProveHonoursTheLegacyVariable: COVMAP_BUFFER is the name the first
// consumer's build sets. It is deliberately undocumented, but it has to keep
// working: dropping it would not fail loudly, it would silently stop emitting
// and leave that consumer's matrix quietly empty.
func TestProveHonoursTheLegacyVariable(t *testing.T) {
	buf := filepath.Join(t.TempDir(), "legacy.jsonl")
	t.Setenv("CS_SANDBOX_COVERAGE_LOG", "")
	t.Setenv("COVMAP_BUFFER", buf)

	Prove(&fakeTB{name: "TestLegacy"}, "interrupt", "claude", "", "scripts")

	if recs := readRecords(t, buf); len(recs) != 1 || recs[0].Test != "TestLegacy" {
		t.Errorf("legacy variable did not emit: %+v", recs)
	}
}

// TestDocumentedVariableWinsOverLegacy: while both names are honoured, a run
// that sets both must write to one file, not two — otherwise a consumer
// migrating between them would double-count every proof.
func TestDocumentedVariableWinsOverLegacy(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, "current.jsonl")
	legacy := filepath.Join(dir, "legacy.jsonl")
	t.Setenv("CS_SANDBOX_COVERAGE_LOG", current)
	t.Setenv("COVMAP_BUFFER", legacy)

	Prove(&fakeTB{name: "TestBoth"}, "login-seeding", "", "", "unit")

	if recs := readRecords(t, current); len(recs) != 1 {
		t.Errorf("documented variable should have received the record, got %d", len(recs))
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Errorf("legacy file must not also be written: %v", err)
	}
}
