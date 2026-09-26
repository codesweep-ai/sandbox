package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
	"golang.org/x/mod/zip"
)

// A module the proxy has never seen cannot be installed by version, which is
// how the image installs cs-sandbox. The build store answers that for a commit
// a clean `make ci` recorded (localmodules.go). For the commit this binary was
// built from, when nothing recorded it, the build packs it here instead.
//
// localModuleProxy writes the module zip the proxy WOULD serve, from this
// repository's git tree, into a throwaway file:// proxy the image build reads.
// The zip comes from golang.org/x/mod/zip, the same package the real proxy
// uses, so `go install <module>@<version>` inside the build behaves exactly as
// it would against proxy.golang.org and the binary still reports its own
// pseudo-version.
//
// It builds from a REVISION, not from the working tree: an unpushed commit can
// be zipped, an unsaved edit cannot. So the .mod beside the zip is the
// revision's go.mod too, and the .info carries the revision's commit time.

// sandboxRevision is the commit this binary was built from, as the toolchain
// recorded it. Empty when there is no build info to read, which is the same
// condition that leaves sandboxPin empty — and the condition a test binary is
// always in, which is why this is a var the tests can stand in for.
var sandboxRevision = func() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" {
			return s.Value
		}
	}
	return ""
}

// gitRaw runs a read-only git command in dir and returns what it printed, byte
// for byte, or git's own complaint.
func gitRaw(dir string, args ...string) ([]byte, error) {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if ee, ok := errors.AsType[*exec.ExitError](err); ok && len(ee.Stderr) > 0 {
		err = errors.New(strings.TrimSpace(string(ee.Stderr)))
	}
	if err != nil {
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

// gitAt is gitRaw's answer as one trimmed line.
func gitAt(dir string, args ...string) (string, error) {
	out, err := gitRaw(dir, args...)
	return strings.TrimSpace(string(out)), err
}

// zipRoot is the directory the module zip is read from: the checkout whose .git
// directory holds the repository. For a worktree made with `git worktree add`
// that is the main checkout, since the worktree's .git is only a file and Go's
// zip code wants a directory (SBX-050). Both share one object store, so any
// commit the worktree holds, the main checkout holds too.
func zipRoot(repoDir string) (string, error) {
	common, err := gitAt(repoDir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	if filepath.Base(common) != ".git" {
		return "", fmt.Errorf("%s is a bare repository, with no checkout to zip a module from", common)
	}
	return filepath.Dir(common), nil
}

// localModuleProxy lays out a one-module file:// proxy for repoDir at revision,
// published under version. It returns the directory to serve and a cleanup.
func localModuleProxy(repoDir, version, revision string) (dir string, cleanup func(), err error) {
	root, err := zipRoot(repoDir)
	if err != nil {
		return "", func() {}, err
	}
	// The manifest and the time are the revision's, so the three files agree
	// with one another and with what the real proxy serves once it is pushed.
	// Byte for byte, since the checksum Go records for a go.mod is of its bytes.
	gomod, err := gitRaw(root, "show", revision+":go.mod")
	if err != nil {
		return "", func() {}, err
	}
	path := modfile.ModulePath(gomod)
	if path == "" {
		return "", func() {}, fmt.Errorf("the go.mod at %s names no module", revision)
	}
	committed, err := gitAt(root, "show", "-s", "--format=%cI", revision)
	if err != nil {
		return "", func() {}, err
	}
	when, err := time.Parse(time.RFC3339, committed)
	if err != nil {
		return "", func() {}, fmt.Errorf("the commit time of %s: %w", revision, err)
	}

	tmp, err := os.MkdirTemp("", "cs-sandbox-goproxy-")
	if err != nil {
		return "", func() {}, err
	}
	cleanup = func() { _ = os.RemoveAll(tmp) }
	fail := func(err error) (string, func(), error) {
		cleanup()
		return "", func() {}, err
	}

	at := filepath.Join(tmp, filepath.FromSlash(path), "@v")
	if err := os.MkdirAll(at, 0o755); err != nil {
		return fail(err)
	}
	m := module.Version{Path: path, Version: version}

	f, err := os.Create(filepath.Join(at, version+".zip"))
	if err != nil {
		return fail(err)
	}
	if err := zip.CreateFromVCS(f, m, root, revision, ""); err != nil {
		f.Close()
		return fail(fmt.Errorf("zip %s at %s: %w", path, revision, err))
	}
	if err := f.Close(); err != nil {
		return fail(err)
	}
	if err := os.WriteFile(filepath.Join(at, version+".mod"), gomod, 0o644); err != nil {
		return fail(err)
	}
	info, err := json.Marshal(struct {
		Version string
		Time    time.Time
	}{version, when.UTC()})
	if err != nil {
		return fail(err)
	}
	if err := os.WriteFile(filepath.Join(at, version+".info"), info, 0o644); err != nil {
		return fail(err)
	}
	return tmp, cleanup, nil
}
