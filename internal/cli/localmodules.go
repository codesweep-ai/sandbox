package cli

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/codesweep-ai/sandbox/internal/paths"
	"golang.org/x/mod/modfile"
)

// The image installs cs-sandbox and each sibling cs- tool by version. A version
// CI built is on proxy.golang.org. A version only this machine built is not, so
// the build finds it where it is (SPEC R167):
//
//   - in the local build store, where a clean `make ci` recorded it
//     (codesweep-ai/dashboards SPEC.md, "The local build store");
//   - for cs-sandbox's own version, when the store lacks it, in the checkout,
//     packed on the fly (localproxy.go).
//
// Each is a file:// proxy bind-mounted into the build, ahead of proxy.golang.org.
// The checksum database has seen none of the modules served from them, so
// GONOSUMDB names those and nothing else.

// guestProxyDir is where the build store's module proxy is mounted inside the
// build, and guestSelfProxyDir where the packed commit is. Under /tmp so nothing
// survives into the image.
const (
	guestProxyDir     = "/tmp/cs-goproxy"
	guestSelfProxyDir = "/tmp/cs-goproxy-self"
)

// sandboxModule is the module image/Containerfile installs cs-sandbox from.
const sandboxModule = "github.com/codesweep-ai/sandbox"

// localModulesNone is the --local-modules value that reads no store and packs
// nothing, so the image installs from published modules alone, as CI's does.
const localModulesNone = "none"

// moduleSources is where the image build takes the modules it installs from,
// beyond proxy.golang.org.
type moduleSources struct {
	store   string   // the store's goproxy/ directory, when it holds a pinned version
	self    string   // a one-module proxy packed from the checkout, when one was
	private []string // the modules either serves, which skip the checksum database
	from    []string // one line per module taken from either, for the phase output
	cleanup func()
}

// buildArgs are the build arguments and podman flags that point the install at
// the sources, or nothing when every module is published.
func (s moduleSources) buildArgs() (args, extra []string) {
	var proxies []string
	for _, p := range []struct{ host, guest string }{{s.store, guestProxyDir}, {s.self, guestSelfProxyDir}} {
		if p.host == "" {
			continue
		}
		proxies = append(proxies, "file://"+p.guest)
		extra = append(extra, "-v", p.host+":"+p.guest+":ro")
	}
	if len(proxies) == 0 {
		return nil, nil
	}
	// The proxy is a directory the invoking user owns, and on an SELinux host
	// the build's RUN steps are denied every read of it: `go install` fails on
	// the .info with "permission denied" and the mount looks empty rather than
	// forbidden. Confinement off rather than a :z relabel, the same choice the
	// run paths make — a relabel also has to be undone, and it fails outright
	// on the virtiofs mounts a macOS podman machine serves host directories from.
	extra = append(extra, "--security-opt", "label=disable")
	args = []string{
		// The real proxy still serves everything else, the Go toolchain
		// included: a file:// proxy 404s for those, and Go moves down the list.
		"CS_GOPROXY=" + strings.Join(proxies, ",") + ",https://proxy.golang.org,direct",
		"CS_GONOSUMDB=" + strings.Join(s.private, ","),
	}
	return args, extra
}

// resolveModuleSources decides, for each module@version the image installs,
// whether it comes from the build store, the checkout, or proxy.golang.org.
// flag is --local-modules: empty for the owner's own store, a store directory,
// or "none".
func (a *App) resolveModuleSources(flag, sandboxVersion string, pins map[string]string) (moduleSources, error) {
	src := moduleSources{cleanup: func() {}}
	if flag == localModulesNone {
		return src, nil
	}
	storeDir := flag
	if storeDir == "" {
		storeDir = paths.BuildStore(imageOwner)
	}
	proxy := filepath.Join(storeDir, "goproxy")
	if _, err := os.Stat(proxy); err != nil {
		if flag != "" {
			return src, fmt.Errorf("--local-modules %s: it holds no goproxy/ directory, so it is no build store", flag)
		}
		proxy = ""
	}

	want := map[string]string{sandboxModule: sandboxVersion}
	maps.Copy(want, pins)
	private := map[string]bool{}
	held := func(dir, m, v string) bool {
		if dir == "" {
			return false
		}
		_, err := os.Stat(filepath.Join(dir, filepath.FromSlash(m), "@v", v+".info"))
		return err == nil
	}
	for _, m := range slices.Sorted(maps.Keys(want)) {
		if held(proxy, m, want[m]) {
			src.store = proxy
			private[m] = true
			src.from = append(src.from, fmt.Sprintf("%s %s from the local build store", m, want[m]))
		}
	}

	// cs-sandbox's own version, when no clean `make ci` recorded it, is packed
	// from the checkout holding its revision. A binary with no checkout behind
	// it is a published one, so it has nothing to pack and needs nothing.
	if !private[sandboxModule] {
		if rev := sandboxRevision(); rev != "" && a.AssetDir != "" {
			dir, cleanup, err := localModuleProxy(a.AssetDir, sandboxVersion, rev)
			if err != nil {
				a.phase(fmt.Sprintf("could not pack cs-sandbox %s from %s, so it comes from the module proxy: %v",
					sandboxVersion, a.AssetDir, err))
			} else {
				src.self, src.cleanup = dir, cleanup
				private[sandboxModule] = true
				src.from = append(src.from, fmt.Sprintf("%s %s packed from this checkout", sandboxModule, sandboxVersion))
			}
		}
	}

	// A local build can require other local builds, and Go checks each module
	// in the graph against the checksum database unless GONOSUMDB names it. So
	// every requirement the store serves, and so on down, is named too.
	var todo []string
	for _, m := range slices.Sorted(maps.Keys(private)) {
		todo = append(todo, m+"@"+want[m])
	}
	seen := map[string]bool{}
	for len(todo) > 0 {
		mv := todo[0]
		todo = todo[1:]
		if seen[mv] {
			continue
		}
		seen[mv] = true
		m, v, _ := strings.Cut(mv, "@")
		var data []byte
		for _, dir := range []string{proxy, src.self} {
			if dir == "" {
				continue
			}
			if b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(m), "@v", v+".mod")); err == nil {
				data = b
				break
			}
		}
		if data == nil {
			continue
		}
		f, err := modfile.ParseLax(mv+"/go.mod", data, nil)
		if err != nil {
			src.cleanup()
			return moduleSources{}, fmt.Errorf("reading the go.mod of %s: %w", mv, err)
		}
		for _, r := range f.Require {
			if held(proxy, r.Mod.Path, r.Mod.Version) {
				private[r.Mod.Path] = true
				src.store = proxy
				todo = append(todo, r.Mod.String())
			}
		}
	}
	src.private = slices.Sorted(maps.Keys(private))
	return src, nil
}

// recordImage tells the build store that this machine has made an image of a
// commit a clean `make ci` recorded, so a sibling's `make repin` can take that
// build (codesweep-ai/dashboards SPEC.md, "The local build store"). The entry
// for sandbox lists the images it awaits, and a repin waits until each has a
// file under images/, as CI's status file waits for them to be published.
//
// The file names the image by its local tag, not by its bytes, so a second
// build of the same image writes what the first did and nothing is rewritten.
// A dirty binary, or a commit no clean gate recorded, is not a build a sibling
// may pin, and gets nothing.
func (a *App) recordImage(slim bool) {
	rev := sandboxRevision()
	if rev == "" || strings.HasSuffix(buildVersion(), "+dirty") || a.dryRun() {
		return
	}
	name := path.Base(sandboxModule)
	image := path.Base(localImageRepo)
	if slim {
		image = path.Base(localSlimImageRepo)
	}
	store := paths.BuildStore(imageOwner)
	if _, err := os.Stat(filepath.Join(store, "status", name, rev+".json")); err != nil {
		a.phase(fmt.Sprintf("not recorded in the build store, as no clean `make ci` recorded %.7s", rev))
		return
	}
	file := filepath.Join(store, "images", name, rev, image+".json")
	if _, err := os.Stat(file); err == nil {
		return
	}
	data, err := json.MarshalIndent(struct {
		Schema int    `json:"schema"`
		Name   string `json:"name"`
		Commit string `json:"commit"`
		Image  string `json:"image"`
		Ref    string `json:"ref"`
	}{1, name, rev, image, a.Image}, "", " ")
	if err == nil {
		err = os.MkdirAll(filepath.Dir(file), 0o755)
	}
	if err == nil {
		tmp := fmt.Sprintf("%s.tmp.%d", file, os.Getpid())
		if err = os.WriteFile(tmp, append(data, '\n'), 0o644); err == nil {
			err = os.Rename(tmp, file)
		}
	}
	if err != nil {
		a.phase("could not record the image in the build store: " + err.Error())
		return
	}
	a.phase(fmt.Sprintf("recorded %s of %.7s in the build store, for a sibling's repin", image, rev))
	// A store that is a repository, as a campaign's is, keeps each record as a
	// commit, which the orchestrator carries to the members.
	if prefix, err := gitAt(store, "rev-parse", "--show-prefix"); err == nil && prefix == "" {
		rel, _ := filepath.Rel(store, file)
		_, err := gitAt(store, "add", "--", rel)
		if err == nil {
			_, err = gitAt(store, "commit", "-q", "-m", fmt.Sprintf("Record image %s of %s %.7s", image, name, rev), "--", rel)
		}
		if err != nil {
			a.phase("could not commit the image record in " + store + ": " + err.Error())
		}
	}
}
