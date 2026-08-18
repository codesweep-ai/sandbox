package run

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
)

// Fake is a recording/scripted Runner for unit tests. It records every
// invocation's argv and returns canned Results keyed by a prefix match on the
// rendered command line, so tests can assert what would be executed without
// touching the host.
type Fake struct {
	mu    sync.Mutex
	Calls [][]string // every argv, in order

	// Responses maps a substring of the rendered "argv joined by space" to a
	// canned result. The first matching entry (in insertion order) wins.
	responses []fakeResponse
	// Default is returned when no response matches (zero Result, nil error).
	Default Result
}

type fakeResponse struct {
	match  string
	result Result
	err    error
}

// NewFake returns an empty Fake.
func NewFake() *Fake { return &Fake{} }

// On registers a canned response for any command whose rendered argv contains
// match. Returns the Fake for chaining.
func (f *Fake) On(match string, res Result, err error) *Fake {
	f.responses = append(f.responses, fakeResponse{match: match, result: res, err: err})
	return f
}

// OnStdout is a convenience for On(match, Result{Stdout: out}, nil).
func (f *Fake) OnStdout(match, out string) *Fake {
	return f.On(match, Result{Stdout: out}, nil)
}

// Run implements Runner.
func (f *Fake) Run(_ context.Context, opts Opts, argv ...string) (Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := append([]string(nil), argv...)
	f.Calls = append(f.Calls, cp)
	line := strings.Join(argv, " ")
	res, err := f.Default, error(nil)
	for _, r := range f.responses {
		if strings.Contains(line, r.match) {
			res, err = r.result, r.err
			break
		}
	}
	// Honour StdoutFile as Exec does, or a caller that streams its output to a
	// file is untestable here: it would see the canned bytes as Result.Stdout,
	// which is exactly what the real runner does not do.
	if opts.StdoutFile != "" && err == nil {
		if werr := os.WriteFile(opts.StdoutFile, []byte(res.Stdout), 0o600); werr != nil {
			return Result{}, werr
		}
		res.Stdout = ""
	}
	return res, err
}

// Rendered returns every recorded call as a joined command line, for golden
// assertions.
func (f *Fake) Rendered() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.Calls))
	for i, c := range f.Calls {
		out[i] = strings.Join(c, " ")
	}
	return out
}

// Contains reports whether any recorded call's rendered line contains sub.
func (f *Fake) Contains(sub string) bool {
	for _, l := range f.Rendered() {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

// String renders all calls for debugging.
func (f *Fake) String() string {
	return fmt.Sprintf("Fake{%d calls: %s}", len(f.Calls), strings.Join(f.Rendered(), " | "))
}
