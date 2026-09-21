package renew

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/codesweep-ai/sandbox/internal/lend"
)

func TestContainerArgvIsolatesTheRenewal(t *testing.T) {
	s, _ := lend.SlotByID("claude")
	spec, _ := s.RenewSpec()
	argv := containerArgv("localhost/img:1", spec, "/stage")
	joined := strings.Join(argv, " ")

	for _, want := range []string{
		"--rm", // one turn, holding a real credential
		"--entrypoint /sandbox/home/.local/bin/cs-claude", // this project's own wrapper
		"-v /stage:/stage:rw",                             // the only mount
		"-e HOME=/stage",                                  // the wrappers derive the profile from HOME
		"localhost/img:1",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv is missing %q:\n%s", want, joined)
		}
	}
	// A renewal talks to the provider and nothing talks to it, so it must not join
	// a sandbox network or publish anything. Read from podman's own options only:
	// what follows the image belongs to the client, whose -p means a prompt.
	podmanOpts := strings.Join(argv[:slices.Index(argv, "localhost/img:1")], " ")
	for _, unwanted := range []string{"--network", "-p ", "--publish"} {
		if strings.Contains(podmanOpts, unwanted) {
			t.Errorf("podman's options should not contain %q:\n%s", unwanted, joined)
		}
	}
	// Only the host's agent home may be mounted, and only via the stage.
	if strings.Count(joined, "-v ") != 1 {
		t.Errorf("more than one mount:\n%s", joined)
	}
}

// The turn is the renewal. An entrypoint run with no arguments is a client with
// no prompt: it exits before it authenticates, so nothing is refreshed, and the
// log line that names the turn reads as though it ran.
func TestContainerArgvHandsTheClientItsTurn(t *testing.T) {
	for _, id := range []string{"claude", "codex"} {
		s, _ := lend.SlotByID(id)
		spec, ok := s.RenewSpec()
		if !ok || len(spec.Args) == 0 {
			t.Fatalf("%s: no renewing turn to check", id)
		}
		argv := containerArgv("localhost/img:1", spec, "/stage")
		at := slices.Index(argv, "localhost/img:1")
		if at < 0 {
			t.Fatalf("%s: the image is not in argv:\n%s", id, strings.Join(argv, " "))
		}
		// Everything after the image is handed to the entrypoint, in order.
		if got := argv[at+1:]; !slices.Equal(got, spec.Args) {
			t.Errorf("%s: the client is handed %q, want its turn %q", id, got, spec.Args)
		}
	}
}

// The safety property of the whole container approach: a renewal that ran cleanly
// but rotated nothing must leave the host's credential exactly as it was.
func TestAHostCredentialIsNotTouchedByARenewalThatRotatedNothing(t *testing.T) {
	f := newFixture(t)
	f.lend("claude")
	f.setClaudeExpiry(2 * time.Minute)
	src := filepath.Join(f.home, ".cs-claude", ".credentials.json")
	before, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	// A client that writes the staged file back unchanged: same tokens, same expiry.
	f.onRun = func(f *fixture, stage string) error {
		spec, _ := mustSlot(t, "claude").RenewSpec()
		staged := filepath.Join(stage, spec.ProfileDir, spec.File)
		b, err := os.ReadFile(staged)
		if err != nil {
			return err
		}
		return os.WriteFile(staged, b, 0o600)
	}

	f.k.tick(context.Background())

	after, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("the host's credential was rewritten by a renewal that rotated nothing")
	}
	if st := f.status(); st.Slots[0].Err == "" {
		t.Error("a renewal that rotated nothing was recorded as success")
	}
}

// And the converse: a real rotation reaches the host file, carrying only the
// rotated fields.
func TestARotationReachesTheHostFileAndKeepsTheRest(t *testing.T) {
	f := newFixture(t)
	f.lend("claude")
	// A host document with a field the pinned client will not write back.
	dir := filepath.Join(f.home, ".cs-claude")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, ".credentials.json")
	host := `{"claudeAiOauth":{"accessToken":"old","refreshToken":"old","expiresAt":` +
		strconv.FormatInt(f.now.Add(2*time.Minute).UnixMilli(), 10) + `,"refreshTokenExpiresAt":` +
		strconv.FormatInt(f.now.Add(400*time.Hour).UnixMilli(), 10) + `,"rateLimitTier":"default_claude_max_20x"}}`
	if err := os.WriteFile(src, []byte(host), 0o600); err != nil {
		t.Fatal(err)
	}
	f.onRun = func(f *fixture, stage string) error { return stageClaudeExpiry(t, stage, f.now) }

	f.k.tick(context.Background())

	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		OAuth map[string]any `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("the host credential is no longer JSON: %v", err)
	}
	if got.OAuth["accessToken"] != "rotated" {
		t.Errorf("the rotated token did not reach the host file: %v", got.OAuth["accessToken"])
	}
	if got.OAuth["rateLimitTier"] != "default_claude_max_20x" {
		t.Errorf("a field the client did not write was lost: %v", got.OAuth["rateLimitTier"])
	}
	if fi, err := os.Stat(src); err == nil && fi.Mode().Perm() != 0o600 {
		t.Errorf("the renewed credential is mode %o, want 600", fi.Mode().Perm())
	}
}

// The stage holds the host's real credential, so it must not outlive the renewal.
func TestTheStageIsRemovedAfterwards(t *testing.T) {
	f := newFixture(t)
	f.lend("claude")
	f.setClaudeExpiry(2 * time.Minute)
	f.onRun = func(f *fixture, stage string) error { return stageClaudeExpiry(t, stage, f.now) }

	f.k.tick(context.Background())

	if len(f.stages) != 1 {
		t.Fatalf("stages = %d", len(f.stages))
	}
	if _, err := os.Stat(f.stages[0]); !os.IsNotExist(err) {
		t.Errorf("the staging directory survived the renewal: %s", f.stages[0])
	}
}

func TestNoImageIsReportedBeforeAnythingIsAttempted(t *testing.T) {
	claude, _ := lend.SlotByID("claude")
	key, _ := lend.SlotByID("anthropic")

	// No image, and no container is started to find that out.
	got := Possible(context.Background(), Config{}, []lend.Slot{claude, key})
	err, ok := got["claude"]
	if !ok {
		t.Fatal("a host with no image reported nothing for a renewable slot")
	}
	for _, want := range []string{"no sandbox image", "cs-sandbox build"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error is missing %q: %v", want, err)
		}
	}
	// A key needs no renewal and so needs no image.
	if _, blamed := got["anthropic"]; blamed {
		t.Errorf("a key was blamed for a missing image: %v", got["anthropic"])
	}
}

// Nothing renewable means nothing to ask, and in particular no container start.
func TestPossibleAsksNothingWhenNoSlotIsRenewable(t *testing.T) {
	key, _ := lend.SlotByID("anthropic")
	// An image that does not exist: if this tried to probe it, it would fail.
	got := Possible(context.Background(), Config{Image: "localhost/does-not-exist:none"},
		[]lend.Slot{key})
	if len(got) != 0 {
		t.Errorf("reported something for a slot nothing renews: %v", got)
	}
}

func TestWriteHostCredentialReplacesAtomicallyAndLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".credentials.json")
	if err := os.WriteFile(p, []byte(`{"old":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeHostCredential(p, []byte(`{"new":true}`)); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"new"`) {
		t.Errorf("content = %q", b)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 {
		t.Errorf("a temporary file was left behind: %v", ents)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 600", fi.Mode().Perm())
	}
}

func mustSlot(t *testing.T, id string) lend.Slot {
	t.Helper()
	s, ok := lend.SlotByID(id)
	if !ok {
		t.Fatalf("no %s slot", id)
	}
	return s
}
