// Package spec parses the --snapshot and --repo directory-sharing specs and
// resolves per-repo git identity. Pure parsing + filesystem validation, so it
// is fully unit-testable.
package spec

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/codesweep-ai/sandbox/internal/run"
)

// US is the ASCII Unit Separator used in the repos manifest instead of TAB: a
// TAB is whitespace that the guest init's field splitting would collapse for
// empty fields (e.g. a repo with no @REF), shifting columns. The Unit Separator
// keeps the manifest byte-compatible with the guest init.
const US = "\x1f"

var shareNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// Snapshot is a resolved --snapshot spec: a read-only, point-in-time copy of a
// host directory landing at ~/<Name>.
type Snapshot struct {
	HostPath string // resolved absolute path
	Name     string // mount/home dir name
}

// RepoClone is a resolved --repo spec.
type RepoClone struct {
	HostPath string // resolved absolute path (a git repo)
	Name     string // home dir name
	BaseRef  string // @REF base commit; "" = source HEAD
}

// Options controls path validation (macOS requires shared paths under $HOME).
type Options struct {
	IsMacOS bool
	Home    string
}

// ResolveSnapshots parses --snapshot PATH[:NAME] specs.
func ResolveSnapshots(specs []string, opt Options) ([]Snapshot, error) {
	seen := map[string]bool{}
	var out []Snapshot
	for _, s := range specs {
		path, name := splitName(s)
		rp, err := validateDir(path, "--snapshot", opt)
		if err != nil {
			return nil, err
		}
		if name == "" {
			name = filepath.Base(rp)
		}
		if err := validateShareName(name); err != nil {
			return nil, fmt.Errorf("--snapshot: %w", err)
		}
		if seen[name] {
			return nil, fmt.Errorf("duplicate mount name %q (use --snapshot PATH:NAME to disambiguate)", name)
		}
		seen[name] = true
		out = append(out, Snapshot{HostPath: rp, Name: name})
	}
	return out, nil
}

// ResolveRepoClones parses --repo PATH[@REF][:NAME] specs.
func ResolveRepoClones(specs []string, opt Options) ([]RepoClone, error) {
	seen := map[string]bool{}
	var out []RepoClone
	for _, s := range specs {
		var name, ref string
		// :NAME (slash-free, non-empty tail) is stripped first, then @REF.
		if i := strings.LastIndexByte(s, ':'); i >= 0 {
			tail := s[i+1:]
			if tail != "" && !strings.Contains(tail, "/") {
				name = tail
				s = s[:i]
			}
		}
		if i := strings.LastIndexByte(s, '@'); i >= 0 {
			ref = s[i+1:]
			s = s[:i]
			if ref == "" {
				return nil, fmt.Errorf("--repo: empty @REF")
			}
			if strings.ContainsAny(ref, "\r\n"+US) {
				return nil, fmt.Errorf("--repo: invalid @REF %q: control characters are not allowed", ref)
			}
		}
		rp, err := resolveAbs(s)
		if err != nil {
			return nil, fmt.Errorf("--repo path not found: %s", s)
		}
		if name == "" {
			name = filepath.Base(rp)
		}
		if err := validateShareName(name); err != nil {
			return nil, fmt.Errorf("--repo: %w", err)
		}
		if !isGitRepo(rp) {
			return nil, fmt.Errorf("--repo %q is not a git repository", rp)
		}
		if opt.IsMacOS && !underHome(rp, opt.Home) {
			return nil, fmt.Errorf("on macOS, --repo %q must be under $HOME (%s)", rp, opt.Home)
		}
		if seen[name] {
			return nil, fmt.Errorf("duplicate repo name %q (use --repo PATH:NAME to disambiguate)", name)
		}
		seen[name] = true
		out = append(out, RepoClone{HostPath: rp, Name: name, BaseRef: ref})
	}
	return out, nil
}

// GitIdentity returns the effective "name<US>email" a host repo commits as
// (git -C <repo> config user.*, which resolves a local override, includeIf, or
// the global).
func GitIdentity(ctx context.Context, r run.Runner, repoPath string) string {
	name := run.Output(ctx, r, "git", "-C", repoPath, "config", "user.name")
	email := run.Output(ctx, r, "git", "-C", repoPath, "config", "user.email")
	return cleanManifestField(name) + US + cleanManifestField(email)
}

func validateShareName(name string) error {
	if name == "." || name == ".." || len(name) > 255 || !shareNameRe.MatchString(name) {
		return fmt.Errorf("invalid destination name %q: use letters, digits, dots, underscores, and dashes", name)
	}
	return nil
}

func cleanManifestField(value string) string {
	value = strings.Map(func(r rune) rune {
		switch r {
		case '\r', '\n', '\x1f':
			return ' '
		default:
			return r
		}
	}, value)
	return strings.TrimSpace(value)
}

// splitName applies the PATH:NAME grammar (NAME must contain no slash and be
// non-empty), returning (path, name).
func splitName(s string) (path, name string) {
	if i := strings.LastIndexByte(s, ':'); i >= 0 {
		tail := s[i+1:]
		if tail != "" && !strings.Contains(tail, "/") {
			return s[:i], tail
		}
	}
	return s, ""
}

func validateDir(path, flag string, opt Options) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("%s: path not found: %s", flag, path)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("%s: not a directory: %s", flag, path)
	}
	rp, err := resolveAbs(path)
	if err != nil {
		return "", fmt.Errorf("%s: cannot resolve: %s", flag, path)
	}
	if opt.IsMacOS && !underHome(rp, opt.Home) {
		return "", fmt.Errorf("on macOS, %s %q must be under $HOME (%s) to be visible to the podman machine", flag, rp, opt.Home)
	}
	return rp, nil
}

func resolveAbs(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(abs); err != nil {
		return "", err
	}
	// EvalSymlinks resolves the real (symlink-free) path.
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real, nil
	}
	return abs, nil
}

func isGitRepo(path string) bool {
	if _, err := os.Stat(filepath.Join(path, ".git")); err == nil {
		return true
	}
	if fi, err := os.Stat(filepath.Join(path, "objects")); err == nil && fi.IsDir() {
		return true // bare repo
	}
	return false
}

func underHome(path, home string) bool {
	if home == "" {
		return false
	}
	rel, err := filepath.Rel(home, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
