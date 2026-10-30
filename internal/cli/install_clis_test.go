package cli

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"

	assets "github.com/codesweep-ai/sandbox"
)

// The versions the fake releases serve and the fixture Containerfile pins.
const (
	fakeClaude   = "2.9.1"
	fakeCodex    = "0.9.2"
	fakeOpencode = "9.0.3"
)

// fakeCLI is a CLI that answers --version the way the real one words it.
func fakeCLI(answer string) []byte {
	return []byte("#!/bin/sh\necho '" + answer + "'\n")
}

func tarGz(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func zipOf(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	h := &zip.FileHeader{Name: name, Method: zip.Deflate}
	h.SetMode(0o755)
	w, err := zw.CreateHeader(h)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// releases serves the three vendors' release layouts for this host, and counts
// the builds it hands out. tamper, when set, replaces a served build's bytes
// after its checksum was published.
type releases struct {
	mu      sync.Mutex
	files   map[string][]byte
	builds  int
	tampers map[string]bool
	// digests are the OpenCode digests a Containerfile.agents records for
	// these builds, as ARG name -> sha256: OpenCode publishes none.
	digests map[string]string
}

func (r *releases) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	body, ok := r.files[req.URL.Path]
	if !ok {
		http.NotFound(w, req)
		return
	}
	if !strings.HasSuffix(req.URL.Path, "SUMS") && !strings.HasSuffix(req.URL.Path, ".json") {
		r.builds++
	}
	if r.tampers[req.URL.Path] {
		body = append([]byte("tampered "), body...)
	}
	_, _ = w.Write(body)
}

func serveReleases(t *testing.T) *releases {
	t.Helper()
	goos, goarch := runtime.GOOS, runtime.GOARCH
	r := &releases{files: map[string][]byte{}, tampers: map[string]bool{}, digests: map[string]string{}}

	plat := goos + "-" + map[string]string{"amd64": "x64", "arm64": "arm64"}[goarch]
	claude := fakeCLI(fakeClaude + " (Claude Code)")
	manifest, _ := json.Marshal(map[string]any{"platforms": map[string]any{plat: map[string]string{"checksum": sum(claude)}}})
	r.files["/claude/"+fakeClaude+"/manifest.json"] = manifest
	r.files["/claude/"+fakeClaude+"/"+plat+"/claude"] = claude

	tgt := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[goarch] +
		map[string]string{"linux": "-unknown-linux-musl", "darwin": "-apple-darwin"}[goos]
	codexPkg := tarGz(t, map[string][]byte{
		"bin/codex":     fakeCLI("codex-cli " + fakeCodex),
		"codex-path/rg": fakeCLI("ripgrep 15.0.0"),
	})
	r.files["/codex/rust-v"+fakeCodex+"/codex-package-"+tgt+".tar.gz"] = codexPkg
	r.files["/codex/rust-v"+fakeCodex+"/codex-package_SHA256SUMS"] =
		[]byte(sum(codexPkg) + "  codex-package-" + tgt + ".tar.gz\n")

	ocArch := map[string]string{"amd64": "x64", "arm64": "arm64"}[goarch]
	oc := fakeCLI("opencode v" + fakeOpencode)
	ocTar := tarGz(t, map[string][]byte{"opencode": oc})
	ocZip := zipOf(t, "opencode", oc)
	r.files["/opencode/v"+fakeOpencode+"/opencode-linux-"+ocArch+".tar.gz"] = ocTar
	r.files["/opencode/v"+fakeOpencode+"/opencode-darwin-"+ocArch+".zip"] = ocZip
	r.digests["OPENCODE_SHA256_"+strings.ToUpper(ocArch)] = sum(ocTar)
	r.digests["OPENCODE_SHA256_DARWIN_"+strings.ToUpper(ocArch)] = sum(ocZip)

	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	saved := []string{claudeReleases, codexReleases, opencodeReleases}
	claudeReleases, codexReleases, opencodeReleases = srv.URL+"/claude", srv.URL+"/codex", srv.URL+"/opencode"
	t.Cleanup(func() { claudeReleases, codexReleases, opencodeReleases = saved[0], saved[1], saved[2] })
	return r
}

// pinnedCheckout is an asset dir whose Containerfile.agents pins the fake
// versions and records r's OpenCode digests. The root command takes its asset
// dir from CS_SANDBOX_ASSETS_DIR, so this sets that too.
func pinnedCheckout(t *testing.T, r *releases) string {
	t.Helper()
	dir := t.TempDir()
	img := filepath.Join(dir, "image")
	if err := os.MkdirAll(img, 0o755); err != nil {
		t.Fatal(err)
	}
	var agents strings.Builder
	fmt.Fprintf(&agents, "ARG CLAUDE_CODE_VERSION=%s\nARG CODEX_VERSION=%s\nARG OPENCODE_VERSION=%s\n",
		fakeClaude, fakeCodex, fakeOpencode)
	for k, v := range r.digests {
		fmt.Fprintf(&agents, "ARG %s=%s\n", k, v)
	}
	for name, body := range map[string]string{"Containerfile": "FROM scratch\n", "Containerfile.agents": agents.String()} {
		if err := os.WriteFile(filepath.Join(img, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("CS_SANDBOX_ASSETS_DIR", dir)
	return dir
}

func pinnedApp(t *testing.T, r *releases) *App {
	t.Helper()
	pinnedCheckout(t, r)
	return &App{}
}

func skipUnlessUnixHost(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("the fake CLIs are shell scripts")
	}
}

// R90b: each CLI lands at the version Containerfile.agents pins, and says so
// when asked. Codex keeps its helpers beside it, so dir/codex links into the
// package rather than standing alone.
func TestInstallAgentCLIsInstallsThePins(t *testing.T) {
	skipUnlessUnixHost(t)
	r := serveReleases(t)
	dest := t.TempDir()
	if _, err := runRoot(t, pinnedApp(t, r), "install-agent-clis", dest); err != nil {
		t.Fatalf("install-agent-clis: %v", err)
	}
	for name, want := range map[string]string{"claude": fakeClaude, "codex": fakeCodex, "opencode": fakeOpencode} {
		if got := cliVersion(filepath.Join(dest, name)); got != want {
			t.Errorf("%s reports %q, want %s", name, got, want)
		}
	}
	link, err := os.Readlink(filepath.Join(dest, "codex"))
	if err != nil || !strings.HasPrefix(link, filepath.Join(".agent-clis", "codex-"+fakeCodex)) {
		t.Errorf("codex is %q (%v), want a link into .agent-clis/codex-%s", link, err, fakeCodex)
	}
	if _, err := os.Stat(filepath.Join(dest, ".agent-clis", "codex-"+fakeCodex, "codex-path", "rg")); err != nil {
		t.Errorf("codex's helpers were not kept beside it: %v", err)
	}
}

// R90b: a download that does not match its published checksum replaces
// nothing. The CLI already there keeps working.
func TestInstallAgentCLIsRefusesABadChecksum(t *testing.T) {
	skipUnlessUnixHost(t)
	r := serveReleases(t)
	ocArch := map[string]string{"amd64": "x64", "arm64": "arm64"}[runtime.GOARCH]
	ext := map[string]string{"linux": ".tar.gz", "darwin": ".zip"}[runtime.GOOS]
	r.tampers["/opencode/v"+fakeOpencode+"/opencode-"+runtime.GOOS+"-"+ocArch+ext] = true
	dest := t.TempDir()
	before := fakeCLI("opencode v8.0.0")
	if err := os.WriteFile(filepath.Join(dest, "opencode"), before, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := runRoot(t, pinnedApp(t, r), "install-agent-clis", dest)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("a tampered download installed: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dest, "opencode")); !bytes.Equal(got, before) {
		t.Errorf("the existing opencode was replaced by a download that did not verify")
	}
	if leftovers, _ := filepath.Glob(filepath.Join(dest, ".agent-cli-*")); len(leftovers) > 0 {
		t.Errorf("the rejected download was left behind: %v", leftovers)
	}
}

// A CLI already at its pin is left alone: a second run downloads nothing.
func TestInstallAgentCLIsSkipsWhatIsAlreadyPinned(t *testing.T) {
	skipUnlessUnixHost(t)
	r := serveReleases(t)
	dest := t.TempDir()
	if _, err := runRoot(t, pinnedApp(t, r), "install-agent-clis", dest); err != nil {
		t.Fatalf("first run: %v", err)
	}
	first := r.builds
	if _, err := runRoot(t, &App{}, "install-agent-clis", dest); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if r.builds != first {
		t.Errorf("the second run downloaded %d build(s), want none", r.builds-first)
	}
}

// OpenCode ships a zip on macOS where Linux gets a tarball, and the binary is
// taken out of either.
func TestOpencodeOnMacOSComesAsAZip(t *testing.T) {
	skipUnlessUnixHost(t)
	r := serveReleases(t)
	dest := t.TempDir()
	arch := runtime.GOARCH
	if err := installOpencode(context.Background(), http.DefaultClient, dest, fakeOpencode, r.digests, "darwin", arch); err != nil {
		t.Fatalf("installOpencode for darwin: %v", err)
	}
	if got := cliVersion(filepath.Join(dest, "opencode")); got != fakeOpencode {
		t.Errorf("the zip's opencode reports %q, want %s", got, fakeOpencode)
	}
}

// R90b: OpenCode publishes no checksums, so a platform the agents tier
// records no digest for gets nothing installed, rather than an unchecked build.
func TestOpencodeWithNoRecordedDigestInstallsNothing(t *testing.T) {
	skipUnlessUnixHost(t)
	r := serveReleases(t)
	dest := t.TempDir()
	err := installOpencode(context.Background(), http.DefaultClient, dest, fakeOpencode, map[string]string{}, runtime.GOOS, runtime.GOARCH)
	if err == nil || !strings.Contains(err.Error(), "records no OPENCODE_SHA256_") {
		t.Fatalf("an OpenCode build with no recorded digest installed: %v", err)
	}
	if r.builds != 0 {
		t.Errorf("downloaded %d build(s) with nothing to check them against", r.builds)
	}
	if _, err := os.Stat(filepath.Join(dest, "opencode")); !os.IsNotExist(err) {
		t.Errorf("opencode was installed: %v", err)
	}
}

// The pins come from the agents tier's own Containerfile, so the embedded copy
// has to carry all three, and a digest for each OpenCode build a host can take.
func TestAgentPinsReadsTheAgentsTier(t *testing.T) {
	pins, err := assets.AgentPins("")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range agentCLIs {
		if !semverOf.MatchString(pins[a.pin]) {
			t.Errorf("Containerfile.agents pins %s = %q, want a version", a.pin, pins[a.pin])
		}
	}
	sha := regexp.MustCompile(`^[0-9a-f]{64}$`)
	for _, k := range []string{"OPENCODE_SHA256_X64", "OPENCODE_SHA256_ARM64", "OPENCODE_SHA256_DARWIN_X64", "OPENCODE_SHA256_DARWIN_ARM64"} {
		if !sha.MatchString(pins[k]) {
			t.Errorf("Containerfile.agents records %s = %q, want a sha256", k, pins[k])
		}
	}
}
