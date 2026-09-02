//go:build integration || smoke || live_agents || agents_replay

package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/codesweep-ai/sandbox/internal/run"
)

// TestProbeReport holds the post-mortem to the one thing it is for: saying how
// a probe ended. The reporting it replaced returned "" for every failure and
// the caller called all of them "not responding", so a lost ssh connection and
// a probe that answered on a non-zero exit both read as a wedged guest.
func TestProbeReport(t *testing.T) {
	exit := func(code int, stderr string) error {
		return &run.ExitError{Argv: []string{"ssh"}, ExitCode: code, Stderr: stderr}
	}
	for _, tc := range []struct {
		name     string
		res      run.Result
		err      error
		timedOut bool
		want     []string // substrings the report must carry
		notWant  []string
	}{{
		name: "answer",
		res:  run.Result{Stdout: "CONTAINER ID  IMAGE\n"},
		want: []string{"CONTAINER ID"},
		// A probe that answered says nothing about how it ended.
		notWant: []string{"("},
	}, {
		name:     "deadline",
		timedOut: true,
		err:      errors.New("signal: killed"),
		want:     []string{"no answer within 20s", "not merely busy"},
	}, {
		// The fault this could not previously be told apart from a deadline.
		name:    "ssh failed fast",
		res:     run.Result{Stderr: "ssh: connect to host port 22: Connection refused", ExitCode: 255},
		err:     exit(255, "ssh: connect to host port 22: Connection refused"),
		want:    []string{"exit 255", "the connection, not the command", "Connection refused"},
		notWant: []string{"not merely busy"},
	}, {
		// 2>&1 already put the answer on stdout; the old code threw it away.
		name:    "probe exited non-zero with output",
		res:     run.Result{Stdout: "Error: unable to connect to Podman socket", ExitCode: 125},
		err:     exit(125, ""),
		want:    []string{"probe exited 125", "unable to connect to Podman socket"},
		notWant: []string{"not merely busy"},
	}, {
		name:    "succeeded with no output",
		want:    []string{"exit 0, no output"},
		notWant: []string{"not merely busy"},
	}, {
		name:    "never started",
		err:     errors.New("run ssh: exec: \"ssh\": executable file not found in $PATH"),
		want:    []string{"probe did not run", "executable file not found"},
		notWant: []string{"not merely busy"},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			got := probeReport(tc.res, tc.err, tc.timedOut)
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("report = %q, want it to contain %q", got, want)
				}
			}
			for _, not := range tc.notWant {
				if strings.Contains(got, not) {
					t.Errorf("report = %q, want it NOT to contain %q", got, not)
				}
			}
		})
	}
}
