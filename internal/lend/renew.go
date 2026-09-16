package lend

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Describing how a lent login is renewed, and which of its values a refresh
// actually changes.
//
// The renewal itself runs in a container built from this project's own sandbox
// image — see internal/renew for why — so what a slot has to say about it is
// where the client lives in that image, how to invoke it, and which fields to
// carry back out. All data: internal/lend still execs nothing and still reads no
// environment.

// wrapperDir is where the sandbox image carries this project's own launch
// wrappers. The image is built with `COPY . /sandbox`, so the repository's
// image/rootfs tree lands under here.
//
// The wrappers rather than the bare clients, and in the image rather than on the
// host, which took a while to arrive at:
//
//   - They are the entry point that created the credential in the first place.
//     The copy in the image is byte-identical to the one on the host, so this
//     renews a login the same way it was signed in.
//   - Their side effects are free here. On the host the objection was that they
//     write the profile — .claude.json trust and theme, config.toml trust — but
//     the profile they write is the staged one, which is deleted afterwards.
//   - Their one real hazard is structurally absent. Each sources
//     ~/.cs-<agent>/env, where an ANTHROPIC_API_KEY or a Bedrock/Vertex target
//     would take the client off the OAuth path this exists to refresh. The staged
//     home has no such file, and the host's is never read.
//   - They answer the first-run dialogs — theme, directory trust, the custom
//     API-key prompt — which is exactly what a freshly staged profile hits, and
//     what would otherwise block a non-interactive run on some client version.
//   - Their --yolo branches are deterministic here: both are gated on a marker in
//     the profile directory or an environment variable, and neither exists in a
//     staged home, so the safe branch is always taken. On the host a stray .yolo
//     file would have flipped it.
const wrapperDir = "/sandbox/home/.local/bin"

// RenewSpec is everything needed to renew one slot's login in a container.
type RenewSpec struct {
	// Bin is the client's absolute path inside the image, not a name to look up.
	// The image installs these at fixed, checksum-verified locations, which is
	// the whole reason the renewal runs in there: on a host the same client may
	// sit under a version manager's directory that no service-manager PATH
	// reaches.
	Bin  string
	Args []string

	// ProfileDir is the profile directory's name under the staged HOME, and File
	// is the credential inside it. The renewer stages a copy of the host's
	// credential there and reads the result back.
	//
	// A directory under HOME rather than a variable, because the wrapper decides
	// the profile from $HOME and overrides any CLAUDE_CONFIG_DIR / CODEX_HOME it
	// is handed. Staging a small home that looks like the host's is what makes
	// the wrapper work unchanged — and it is the same layout the seed uses inside
	// a sandbox (seed.LentCredential.Agent is "~/.cs-<agent>").
	ProfileDir string
	File       string

	// Fields are the values a refresh rotates, as paths into the credential
	// document. They are the ONLY values copied back to the host.
	//
	// A field list rather than replacing the file wholesale, because the client
	// in the image is a pinned version and the host's is not. Replacing would let
	// an older client drop a field the newer one relies on — rateLimitTier, say,
	// which the client reads to decide what to say about limits — and a refresh
	// would quietly degrade a credential that was working. Copying only what a
	// refresh is supposed to change makes that impossible by construction.
	Fields [][]string
}

// RenewSpec returns how this slot is renewed, or ok=false for a slot nothing
// renews.
func (s Slot) RenewSpec() (RenewSpec, bool) {
	if s.renew == nil {
		return RenewSpec{}, false
	}
	return *s.renew, true
}

// MergeRefreshed returns the host's credential document with the rotated values
// taken from a refreshed one, and everything else left exactly as it was.
//
// Both documents are parsed, not patched textually: a credential file is written
// by its client and its formatting is not ours to preserve.
//
// A field the refreshed document does not carry is left at the host's value
// rather than deleted. The client decides what a refresh returns, and absence is
// not an instruction to remove something.
func (s Slot) MergeRefreshed(host, refreshed []byte) ([]byte, error) {
	spec, ok := s.RenewSpec()
	if !ok {
		return nil, fmt.Errorf("nothing renews the %s slot", s.ID)
	}
	var into, from map[string]any
	if err := json.Unmarshal(host, &into); err != nil {
		return nil, fmt.Errorf("the host's %s credential is not readable as JSON: %w", s.ID, err)
	}
	if err := json.Unmarshal(refreshed, &from); err != nil {
		return nil, fmt.Errorf("the renewed %s credential is not readable as JSON: %w", s.ID, err)
	}

	moved := 0
	for _, path := range spec.Fields {
		v, ok := lookupPath(from, path)
		if !ok {
			continue
		}
		if err := setPath(into, path, v); err != nil {
			return nil, err
		}
		moved++
	}
	if moved == 0 {
		return nil, fmt.Errorf("the renewed %s credential carried none of the values a refresh changes, "+
			"so there is nothing to copy back", s.ID)
	}
	return json.Marshal(into)
}

// lookupPath walks a dotted path through a decoded document.
func lookupPath(doc map[string]any, path []string) (any, bool) {
	cur := doc
	for i, key := range path {
		v, ok := cur[key]
		if !ok {
			return nil, false
		}
		if i == len(path)-1 {
			return v, true
		}
		next, ok := v.(map[string]any)
		if !ok {
			return nil, false
		}
		cur = next
	}
	return nil, false
}

// setPath writes a value at a dotted path, creating intermediate objects.
func setPath(doc map[string]any, path []string, val any) error {
	cur := doc
	for i, key := range path {
		if i == len(path)-1 {
			cur[key] = val
			return nil
		}
		switch next := cur[key].(type) {
		case map[string]any:
			cur = next
		case nil:
			created := map[string]any{}
			cur[key] = created
			cur = created
		default:
			return fmt.Errorf("cannot set %s: %s is not an object", joinPathKeys(path), key)
		}
	}
	return nil
}

func joinPathKeys(path []string) string { return strings.Join(path, ".") }

// claudeRenewSpec is the turn that makes Claude Code refresh its own OAuth token.
//
// A real turn rather than a cheaper probe, because it is the only invocation that
// must traverse the auth path: the client has to present a valid token or fail, so
// it refreshes first.
//
// The flags are subtractions, and two matter more than they look:
//
//   - never --bare. It looks like the way to make this cheap, but in bare mode the
//     client's own help says Anthropic auth is strictly ANTHROPIC_API_KEY or
//     apiKeyHelper and "OAuth and keychain are never read" — precisely the mode
//     that cannot refresh the credential in question.
//   - --model haiku, because an OAuth login bills the subscription rather than API
//     credits but still lands on its rolling usage window.
//   - --strict-mcp-config, so renewing a token does not start any connectors.
var claudeRenewSpec = &RenewSpec{
	Bin: wrapperDir + "/cs-claude",
	// --strict-mcp-config and --permission-mode are the wrapper's own, so only
	// the two flags it does not supply are passed here. --model haiku because the
	// cheapest turn that still authenticates is the right one: an OAuth login
	// bills the subscription rather than API credits, but still lands on its
	// rolling usage window.
	Args:       []string{"-p", "ping", "--model", "haiku"},
	ProfileDir: ".cs-claude",
	File:       ".credentials.json",
	Fields: [][]string{
		{"claudeAiOauth", "accessToken"},
		{"claudeAiOauth", "refreshToken"},
		{"claudeAiOauth", "expiresAt"},
		{"claudeAiOauth", "refreshTokenExpiresAt"},
	},
}

// codexRenewSpec is the same for Codex.
//
// --skip-git-repo-check because the renewal runs in a directory of its own that is
// not a git repository, and `codex exec` otherwise refuses with "Not inside a
// trusted directory". Marking the directory trusted would work too and is the
// thing not to do: trust lives in the profile's own config.toml, and writing it
// there would make renewal a writer of something it should only read.
//
// account_id is deliberately absent from Fields. It identifies the account rather
// than the session, a refresh does not change it, and copying it back would let a
// renewal rewrite which account the host's login belongs to.
var codexRenewSpec = &RenewSpec{
	Bin:        wrapperDir + "/cs-codex",
	Args:       []string{"exec", "--skip-git-repo-check", "ping"},
	ProfileDir: ".cs-codex",
	File:       "auth.json",
	Fields: [][]string{
		{"tokens", "access_token"},
		{"tokens", "refresh_token"},
		{"tokens", "id_token"},
		{"last_refresh"},
	},
}

// renewFieldNames is for tests and errors: the paths a slot copies back, spelled.
func (s Slot) renewFieldNames() []string {
	spec, ok := s.RenewSpec()
	if !ok {
		return nil
	}
	out := make([]string, 0, len(spec.Fields))
	for _, p := range spec.Fields {
		out = append(out, joinPathKeys(p))
	}
	slices.Sort(out)
	return out
}
