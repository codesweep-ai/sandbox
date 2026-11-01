package cli

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	assets "github.com/codesweep-ai/sandbox"
	"github.com/spf13/cobra"
)

// Where each vendor publishes its releases. Vars so a test can serve them.
var (
	claudeReleases   = "https://downloads.claude.ai/claude-code-releases"
	codexReleases    = "https://github.com/openai/codex/releases/download"
	opencodeReleases = "https://github.com/anomalyco/opencode/releases/download"
)

// agentCLI is one agent CLI the agents tier installs, and how a host gets the
// same build of it.
type agentCLI struct {
	name string // the command, as it appears on PATH
	pin  string // the Containerfile.agents ARG holding its version
	// install fetches the build for goos/goarch, checks it against the
	// checksum its vendor publishes, and puts it at dir/name. Nothing at
	// dir/name changes unless the download verified.
	install func(ctx context.Context, c *http.Client, dir, version, goos, goarch string) error
}

var agentCLIs = []agentCLI{
	{name: "claude", pin: "CLAUDE_CODE_VERSION", install: installClaude},
	{name: "codex", pin: "CODEX_VERSION", install: installCodex},
	{name: "opencode", pin: "OPENCODE_VERSION", install: installOpencode},
}

// newInstallAgentCLIsCmd installs on the host the agent CLIs its sandboxes run.
//
// The versions come from image/Containerfile.agents, the one place they are
// pinned, and each build is verified the way the agents tier verifies it. A
// host whose CLIs drift from its sandboxes' records sessions its sandboxes
// cannot replay, and drift is what an updating installer produces by default.
func newInstallAgentCLIsCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "install-agent-clis [dir]",
		Short: "Install the agent CLIs (claude, codex, opencode) at the versions the image pins (default: ~/.local/bin)",
		Long: "Install claude, codex and opencode on this host at the versions image/Containerfile.agents\n" +
			"pins, so the host runs the same builds as its sandboxes. Each download is checked against the\n" +
			"checksum its vendor publishes before it replaces anything. A CLI already at its pin is left as it is.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dest := filepath.Join(app.Host.Home, ".local", "bin")
			if len(args) == 1 {
				dest = args[0]
			}
			pins, err := assets.AgentPins(app.AssetDir)
			if err != nil {
				return fmt.Errorf("read the agent pins: %w", err)
			}
			if err := os.MkdirAll(dest, 0o755); err != nil {
				return fmt.Errorf("cannot create %s: %w", dest, err)
			}
			out := cmd.OutOrStdout()
			listed := onPath(dest)
			client := &http.Client{Timeout: 15 * time.Minute}
			for _, a := range agentCLIs {
				want := pins[a.pin]
				if want == "" {
					return fmt.Errorf("image/Containerfile.agents pins no %s", a.pin)
				}
				target := filepath.Join(dest, a.name)
				if cliVersion(target) == want {
					fmt.Fprintf(out, "%s %s is already in %s\n", a.name, want, dest)
				} else {
					if err := a.install(cmd.Context(), client, dest, want, runtime.GOOS, runtime.GOARCH); err != nil {
						return fmt.Errorf("install %s %s: %w", a.name, want, err)
					}
					if got := cliVersion(target); got != want {
						return fmt.Errorf("%s reports version %q after installing, want %s", target, got, want)
					}
					fmt.Fprintf(out, "installed %s %s into %s\n", a.name, want, dest)
				}
				// Only where dest is on PATH: a copy that comes earlier is the one
				// that runs, and the install changed nothing a caller will see.
				if found, err := exec.LookPath(a.name); listed && err == nil && !sameFile(found, target) {
					fmt.Fprintf(out, "note: %s on your PATH is %s, which comes before this one\n", a.name, found)
				}
			}
			if !listed {
				fmt.Fprintf(out, "note: %s is not on your PATH yet — add it:\n", dest)
				fmt.Fprintf(out, "    echo 'export PATH=\"%s:$PATH\"' >> ~/.bashrc && source ~/.bashrc\n", dest)
			}
			return nil
		},
	}
}

// installClaude installs Claude Code's native binary, checked against the
// release manifest, as the agents tier does.
func installClaude(ctx context.Context, c *http.Client, dir, version, goos, goarch string) error {
	arch, ok := map[string]string{"amd64": "x64", "arm64": "arm64"}[goarch]
	if !ok || (goos != "linux" && goos != "darwin") {
		return fmt.Errorf("no Claude Code build for %s/%s", goos, goarch)
	}
	plat := goos + "-" + arch
	base := claudeReleases + "/" + version
	var manifest struct {
		Platforms map[string]struct {
			Checksum string `json:"checksum"`
		} `json:"platforms"`
	}
	if err := getJSON(ctx, c, base+"/manifest.json", &manifest); err != nil {
		return err
	}
	want := manifest.Platforms[plat].Checksum
	if want == "" {
		return fmt.Errorf("the %s manifest has no checksum for %s", version, plat)
	}
	tmp, err := fetchVerified(ctx, c, base+"/"+plat+"/claude", dir, want)
	if err != nil {
		return err
	}
	return replaceExec(tmp, filepath.Join(dir, "claude"))
}

// installCodex installs Codex's release package: the CLI with the helpers it
// ships, kept together under dir/.agent-clis, and dir/codex linked into it.
func installCodex(ctx context.Context, c *http.Client, dir, version, goos, goarch string) error {
	arch, ok := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[goarch]
	target := map[string]string{"linux": "-unknown-linux-musl", "darwin": "-apple-darwin"}[goos]
	if !ok || target == "" {
		return fmt.Errorf("no Codex build for %s/%s", goos, goarch)
	}
	asset := "codex-package-" + arch + target + ".tar.gz"
	base := codexReleases + "/rust-v" + version
	want, err := publishedSum(ctx, c, base+"/codex-package_SHA256SUMS", asset)
	if err != nil {
		return err
	}
	tmp, err := fetchVerified(ctx, c, base+"/"+asset, dir, want)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)

	pkgs := filepath.Join(dir, ".agent-clis")
	pkg := filepath.Join(pkgs, "codex-"+version)
	staging := pkg + ".new"
	_ = os.RemoveAll(staging)
	if err := extractTarGz(tmp, staging); err != nil {
		_ = os.RemoveAll(staging)
		return err
	}
	if fi, err := os.Stat(filepath.Join(staging, "bin", "codex")); err != nil || fi.IsDir() {
		_ = os.RemoveAll(staging)
		return fmt.Errorf("%s holds no bin/codex", asset)
	}
	_ = os.RemoveAll(pkg)
	if err := os.Rename(staging, pkg); err != nil {
		return err
	}
	link := filepath.Join(dir, "codex")
	if err := os.Remove(link + ".new"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Symlink(filepath.Join(".agent-clis", "codex-"+version, "bin", "codex"), link+".new"); err != nil {
		return err
	}
	if err := os.Rename(link+".new", link); err != nil {
		return err
	}
	// The packages of earlier versions are no longer reachable from dir/codex.
	old, _ := filepath.Glob(filepath.Join(pkgs, "codex-*"))
	for _, p := range old {
		if p != pkg {
			_ = os.RemoveAll(p)
		}
	}
	return nil
}

// installOpencode installs OpenCode's single binary from its release archive,
// checked against the release's SHA256SUMS.
func installOpencode(ctx context.Context, c *http.Client, dir, version, goos, goarch string) error {
	arch, ok := map[string]string{"amd64": "x64", "arm64": "arm64"}[goarch]
	ext := map[string]string{"linux": ".tar.gz", "darwin": ".zip"}[goos]
	if !ok || ext == "" {
		return fmt.Errorf("no OpenCode build for %s/%s", goos, goarch)
	}
	asset := "opencode-" + goos + "-" + arch + ext
	base := opencodeReleases + "/v" + version
	want, err := publishedSum(ctx, c, base+"/SHA256SUMS", asset)
	if err != nil {
		return err
	}
	tmp, err := fetchVerified(ctx, c, base+"/"+asset, dir, want)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	bin := filepath.Join(dir, ".opencode.new")
	if ext == ".zip" {
		err = extractOneZip(tmp, "opencode", bin)
	} else {
		err = extractOneTarGz(tmp, "opencode", bin)
	}
	if err != nil {
		_ = os.Remove(bin)
		return fmt.Errorf("%s: %w", asset, err)
	}
	return replaceExec(bin, filepath.Join(dir, "opencode"))
}

// fetchVerified downloads url into a temporary file in dir and returns its
// path once its sha256 is want. A download that does not verify is removed.
func fetchVerified(ctx context.Context, c *http.Client, url, dir, want string) (string, error) {
	body, err := get(ctx, c, url)
	if err != nil {
		return "", err
	}
	defer body.Close()
	f, err := os.CreateTemp(dir, ".agent-cli-*.download")
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("download %s: %w", url, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, want) {
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("checksum mismatch for %s: want %s, got %s", url, want, got)
	}
	return f.Name(), nil
}

// publishedSum reads the checksum a SHA256SUMS-style file publishes for asset.
func publishedSum(ctx context.Context, c *http.Client, url, asset string) (string, error) {
	body, err := get(ctx, c, url)
	if err != nil {
		return "", err
	}
	defer body.Close()
	sc := bufio.NewScanner(body)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == asset {
			return fields[0], nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("%s publishes no checksum for %s", url, asset)
}

func get(ctx context.Context, c *http.Client, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return resp.Body, nil
}

func getJSON(ctx context.Context, c *http.Client, url string, v any) error {
	body, err := get(ctx, c, url)
	if err != nil {
		return err
	}
	defer body.Close()
	if err := json.NewDecoder(body).Decode(v); err != nil {
		return fmt.Errorf("read %s: %w", url, err)
	}
	return nil
}

// replaceExec makes src executable and renames it over dst, so a reader of dst
// sees the old file or the new one and never half of either.
func replaceExec(src, dst string) error {
	if err := os.Chmod(src, 0o755); err != nil {
		_ = os.Remove(src)
		return err
	}
	if err := os.Rename(src, dst); err != nil {
		_ = os.Remove(src)
		return err
	}
	return nil
}

// extractTarGz unpacks a gzipped tarball into dst, refusing any entry that
// would land outside it.
func extractTarGz(src, dst string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := path.Clean(strings.TrimPrefix(h.Name, "./"))
		if name == "." {
			continue
		}
		if !filepath.IsLocal(name) {
			return fmt.Errorf("refusing archive entry %q", h.Name)
		}
		target := filepath.Join(dst, filepath.FromSlash(name))
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := writeFrom(tr, target, os.FileMode(h.Mode)&0o755|0o644); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if !filepath.IsLocal(filepath.Join(filepath.Dir(name), h.Linkname)) {
				return fmt.Errorf("refusing archive link %q -> %q", h.Name, h.Linkname)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(h.Linkname, target); err != nil {
				return err
			}
		}
	}
}

// extractOneTarGz writes the archive's regular file whose base name is name to dst.
func extractOneTarGz(src, name, dst string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("holds no %s", name)
		}
		if err != nil {
			return err
		}
		if h.Typeflag == tar.TypeReg && path.Base(h.Name) == name {
			return writeFrom(tr, dst, 0o755)
		}
	}
}

// extractOneZip writes the zip's regular file whose base name is name to dst.
func extractOneZip(src, name, dst string) error {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, f := range zr.File {
		if !f.FileInfo().Mode().IsRegular() || path.Base(f.Name) != name {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		err = writeFrom(rc, dst, 0o755)
		if cerr := rc.Close(); err == nil {
			err = cerr
		}
		return err
	}
	return fmt.Errorf("holds no %s", name)
}

func writeFrom(r io.Reader, dst string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// semverOf finds the version in an agent's --version answer, which each of
// them words differently: `2.1.312 (Claude Code)`, `codex-cli 0.165.0`,
// `opencode v2.2.2`.
var semverOf = regexp.MustCompile(`[0-9]+\.[0-9]+\.[0-9]+`)

// cliVersion is what the CLI at path says its version is, or "" when there is
// none there or it will not say.
func cliVersion(path string) string {
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return ""
	}
	return semverOf.FindString(string(out))
}

// sameFile reports whether two paths name the same file on disk.
func sameFile(a, b string) bool {
	fa, err := os.Stat(a)
	if err != nil {
		return false
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(fa, fb)
}
