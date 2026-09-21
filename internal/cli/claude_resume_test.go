package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	resumeLiveID = "11111111-1111-4111-8111-111111111111"
	resumeAPIID  = "22222222-2222-4222-8222-222222222222"
	resumeWebID  = "33333333-3333-4333-8333-333333333333"
	resumeGoneID = "44444444-4444-4444-8444-444444444444"
)

// resumeHome builds a fake $HOME holding four cs-claude transcripts in the shape Claude
// Code writes them, newest first: one still open in a live process, one renamed, one
// with only a generated title, and one whose directory has since been deleted. The
// cs-claude stub reports where it was started and with what, instead of starting.
func resumeHome(t *testing.T) (home, bin string) {
	t.Helper()
	jq, err := exec.LookPath("jq")
	if err != nil {
		t.Skip("cs-claude-resume needs jq")
	}
	home = t.TempDir()
	bin = filepath.Join(home, "bin")
	for _, d := range []string{bin, filepath.Join(home, "work", "api"), filepath.Join(home, "work", "web"),
		filepath.Join(home, ".cs-claude", "sessions")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(jq, filepath.Join(bin, "jq")); err != nil {
		t.Fatal(err)
	}
	writeStub(t, bin, "cs-claude", "#!/bin/sh\necho \"ran in $(pwd -P): $*\"\n")

	transcript := func(cwd, id string, age time.Duration, meta ...string) {
		dir := filepath.Join(home, ".cs-claude", "projects", regexp.MustCompile(`[^a-zA-Z0-9]`).ReplaceAllString(cwd, "-"))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		lines := append([]string{`{"type":"user","cwd":"` + cwd + `","sessionId":"` + id + `","message":{"role":"user","content":"hi"}}`}, meta...)
		path := filepath.Join(dir, id+".jsonl")
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		when := time.Now().Add(-age)
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatal(err)
		}
	}
	transcript(filepath.Join(home, "work", "web"), resumeLiveID, 10*time.Minute, `{"type":"ai-title","aiTitle":"Live one"}`)
	transcript(filepath.Join(home, "work", "api"), resumeAPIID, time.Hour,
		`{"type":"ai-title","aiTitle":"Generated api title"}`, `{"type":"custom-title","customTitle":"Renamed api work"}`)
	transcript(filepath.Join(home, "work", "web"), resumeWebID, 2*time.Hour, `{"type":"ai-title","aiTitle":"Web build"}`)
	transcript(filepath.Join(home, "gone"), resumeGoneID, 3*time.Hour, `{"type":"last-prompt","lastPrompt":"fix it"}`)

	// The test process stands in for the Claude Code holding the live session open.
	live := `{"pid":` + strconv.Itoa(os.Getpid()) + `,"sessionId":"` + resumeLiveID + `","kind":"interactive"}`
	if err := os.WriteFile(filepath.Join(home, ".cs-claude", "sessions", strconv.Itoa(os.Getpid())+".json"), []byte(live), 0o600); err != nil {
		t.Fatal(err)
	}
	return home, bin
}

// TestClaudeResumeListsEveryDirectory: `-l` lists sessions from every project newest first,
// each under the directory it was started in, titled the way the picker titles it, and
// terms narrow the list.
func TestClaudeResumeListsEveryDirectory(t *testing.T) {
	home, bin := resumeHome(t)
	out, exit := runScript(t, home, bin, "cs-claude-resume", "-l")
	if exit != 0 {
		t.Fatalf("-l exit %d: %s", exit, out)
	}
	rows := strings.Split(strings.TrimRight(out, "\n"), "\n")
	for i, want := range []string{
		"~/work/web  " + resumeLiveID + "  Live one  [running]",
		"~/work/api  " + resumeAPIID + "  Renamed api work",
		"~/work/web  " + resumeWebID + "  Web build",
		"~/gone      " + resumeGoneID + "  fix it",
	} {
		if i >= len(rows) || !strings.HasSuffix(rows[i], want) {
			t.Fatalf("row %d should end %q:\n%s", i+1, want, out)
		}
	}

	out, _ = runScript(t, home, bin, "cs-claude-resume", "-l", "WEB", "build")
	if !strings.Contains(out, resumeWebID) || strings.Count(out, "\n") != 1 {
		t.Errorf("terms should keep only the session matching all of them:\n%s", out)
	}
}

// TestClaudeResumeStartsInTheSessionsDirectory: the pick runs cs-claude --resume in the
// session's own directory, whatever directory the tool ran from, and passes on what
// follows `--`.
func TestClaudeResumeStartsInTheSessionsDirectory(t *testing.T) {
	home, bin := resumeHome(t)
	out, exit := runScriptStdin(t, home, bin, nil, "2\n", "cs-claude-resume", "--", "--verbose")
	api, _ := filepath.EvalSymlinks(filepath.Join(home, "work", "api"))
	if want := "ran in " + api + ": --resume " + resumeAPIID + " --verbose"; exit != 0 || !strings.Contains(out, want) {
		t.Fatalf("exit %d, want %q in:\n%s", exit, want, out)
	}
}

// TestClaudeResumeRefusesWhatCannotResume: a session still open in another Claude Code is
// refused unless the pick forks it, and so is one whose directory is gone.
func TestClaudeResumeRefusesWhatCannotResume(t *testing.T) {
	home, bin := resumeHome(t)
	if out, exit := runScriptStdin(t, home, bin, nil, "1\n", "cs-claude-resume"); exit != 1 || !strings.Contains(out, "still open") {
		t.Errorf("an open session should be refused, exit %d:\n%s", exit, out)
	}
	if out, exit := runScriptStdin(t, home, bin, nil, "1\n", "cs-claude-resume", "--", "--fork-session"); exit != 0 ||
		!strings.Contains(out, "--resume "+resumeLiveID+" --fork-session") {
		t.Errorf("forking an open session should go ahead, exit %d:\n%s", exit, out)
	}
	if out, exit := runScriptStdin(t, home, bin, nil, "4\n", "cs-claude-resume"); exit != 1 || !strings.Contains(out, "no longer exists") {
		t.Errorf("a session whose directory is gone should be refused, exit %d:\n%s", exit, out)
	}
}
