package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"

	"golang.org/x/mod/module"
	"golang.org/x/mod/zip"
)

// A module the proxy has never seen cannot be installed by version, which is
// how the image installs cs-sandbox. That is the right default — an image
// should name a published revision — but it stops `cs-sandbox build` dead on a
// commit that has not been pushed yet.
//
// --local-sandbox answers that without giving up the version: it writes the
// module zip the proxy WOULD serve, from this repository's git tree, into a
// throwaway file:// proxy the image build reads. The zip comes from
// golang.org/x/mod/zip, the same package the real proxy uses, so `go install
// <module>@<version>` inside the build behaves exactly as it would against
// proxy.golang.org and the binary still reports its own pseudo-version.
//
// It builds from a REVISION, not from the working tree: an unpushed commit can
// be zipped, an unsaved edit cannot.

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

// modulePath reads the module line from a checkout's go.mod, so a renamed
// module does not quietly publish itself under the old name.
func modulePath(repoDir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(repoDir, "go.mod"))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return strings.TrimSpace(rest), nil
		}
	}
	return "", fmt.Errorf("no module line in %s/go.mod", repoDir)
}

// localModuleProxy lays out a one-module file:// proxy for repoDir at revision,
// published under version. It returns the directory to serve and a cleanup.
func localModuleProxy(repoDir, version, revision string) (dir string, cleanup func(), err error) {
	path, err := modulePath(repoDir)
	if err != nil {
		return "", func() {}, err
	}
	tmp, err := os.MkdirTemp("", "cs-sandbox-goproxy-")
	if err != nil {
		return "", func() {}, err
	}
	cleanup = func() { _ = os.RemoveAll(tmp) }

	at := filepath.Join(tmp, filepath.FromSlash(path), "@v")
	if err := os.MkdirAll(at, 0o755); err != nil {
		cleanup()
		return "", func() {}, err
	}
	m := module.Version{Path: path, Version: version}

	f, err := os.Create(filepath.Join(at, version+".zip"))
	if err != nil {
		cleanup()
		return "", func() {}, err
	}
	if err := zip.CreateFromVCS(f, m, repoDir, revision, ""); err != nil {
		f.Close()
		cleanup()
		return "", func() {}, fmt.Errorf("zip %s at %s: %w", path, revision, err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", func() {}, err
	}

	// .mod and .info complete what a proxy serves for one version. The manifest
	// is the revision's, not the working tree's, so it matches the zip.
	gomod, err := os.ReadFile(filepath.Join(repoDir, "go.mod"))
	if err != nil {
		cleanup()
		return "", func() {}, err
	}
	if err := os.WriteFile(filepath.Join(at, version+".mod"), gomod, 0o644); err != nil {
		cleanup()
		return "", func() {}, err
	}
	info, err := json.Marshal(struct{ Version string }{version})
	if err != nil {
		cleanup()
		return "", func() {}, err
	}
	if err := os.WriteFile(filepath.Join(at, version+".info"), info, 0o644); err != nil {
		cleanup()
		return "", func() {}, err
	}
	return tmp, cleanup, nil
}
