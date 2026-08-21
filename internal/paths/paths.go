// Package paths resolves where cs-sandbox keeps its files, keeping runtime
// state out of the source tree and separating two concerns:
//
//   - runtime STATE (mutable, per-user) -> XDG dirs, never the source tree:
//     instances + tier keys under $XDG_DATA_HOME/cs-sandbox (persistent data),
//     the firecracker artifact cache under $XDG_CACHE_HOME/cs-sandbox (regenerable).
//   - build ASSETS (Containerfile, guest rootfs skeleton, guest init) -> the
//     checkout when running from source, or embedded in the binary.
//
// Every location has an env override; CS_SANDBOX_HOME relocates all state under
// one root. macOS uses ~/Library/{Application Support,Caches}; Linux uses XDG.
package paths

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
)

const app = "cs-sandbox"

// Instances is where per-sandbox records live (persistent data).
func Instances() string {
	if d := os.Getenv("CS_SANDBOX_INSTANCES_DIR"); d != "" {
		return d
	}
	if h := os.Getenv("CS_SANDBOX_HOME"); h != "" {
		return filepath.Join(h, "instances")
	}
	return defaultInstances()
}

// SSHConfigFragment is the basename of the managed ~/.ssh/config.d file that
// describes the instances root at instDir. Roots share one ~/.ssh, so each gets
// its own fragment — otherwise a sync in one would erase another's Host blocks —
// and the Include in ~/.ssh/config is a glob that picks up all of them. The
// default root keeps the plain name, so the common case has a predictable file.
func SSHConfigFragment(instDir string) string {
	inst := instDir
	if abs, err := filepath.Abs(inst); err == nil {
		inst = abs
	}
	if inst == defaultInstances() {
		return app
	}
	sum := sha256.Sum256([]byte(inst))
	return app + "-" + hex.EncodeToString(sum[:])[:8]
}

// defaultInstances is Instances() with the env overrides ignored.
func defaultInstances() string {
	return filepath.Join(dataHome(), app, "instances")
}

// TierKeys is where the generated U/G tier keys live (persistent, secret).
func TierKeys() string {
	if d := os.Getenv("CS_SANDBOX_TIER_DIR"); d != "" {
		return d
	}
	if h := os.Getenv("CS_SANDBOX_HOME"); h != "" {
		return filepath.Join(h, "keys")
	}
	return filepath.Join(dataHome(), app, "keys")
}

// GroupKeys is where one group's SSH trust material lives. Keys are per group
// so a network-boundary failure cannot become an access failure: a member of
// one group holds no credential any other group's sandbox will accept.
func GroupKeys(group string) string {
	return filepath.Join(TierKeys(), "groups", group)
}

// FCCache is the firecracker kernel/rootfs/disk cache (regenerable).
func FCCache() string {
	if d := os.Getenv("CS_SANDBOX_FC_CACHE"); d != "" {
		return d
	}
	if h := os.Getenv("CS_SANDBOX_HOME"); h != "" {
		return filepath.Join(h, "cache")
	}
	return filepath.Join(cacheHome(), app)
}

// AgentLoginHome is the directory holding the .cs-<agent> profiles that
// --inherit-agent-login copies a login out of. The developer's own home, unless
// CS_SANDBOX_AGENT_HOME names another.
//
// It is separate from CS_SANDBOX_HOME, which moves this tool's state. This moves
// only where a login is READ from, and it exists for a caller that has to supply
// one it did not sign in for: a replay suite hands its members a fabricated
// login, and pointing HOME at a fake profile tree would move the instance
// directory and the caches with it.
//
// Nothing is written here. The seed is still written under the instance.
func AgentLoginHome(home string) string {
	if d := os.Getenv("CS_SANDBOX_AGENT_HOME"); d != "" {
		return d
	}
	return home
}

// FCNet is the fabric's working dir: the dnsmasq hostsdir + log and the
// host-route marker.
//
// Unlike the rest of the cache this is deliberately HOST-GLOBAL. There is
// exactly one rootless fabric per host, shared by every sandbox root, so its
// bookkeeping must not follow a per-root CS_SANDBOX_HOME / CS_SANDBOX_FC_CACHE:
// a second root that kept its own copy would see an empty hostsdir, conclude the
// fabric's DNS was down, and try to start a second dnsmasq on an address the
// first one already holds. CS_SANDBOX_FC_NET overrides it for isolated runs.
func FCNet() string {
	if d := os.Getenv("CS_SANDBOX_FC_NET"); d != "" {
		return d
	}
	return filepath.Join(cacheHome(), app, "net")
}

// FCNetFor is one group's fabric working directory. The default group keeps the
// historical path so an existing fabric's DNS bookkeeping is undisturbed;
// every other group gets its own beneath it.
func FCNetFor(group string) string {
	if group == "" || group == "default" {
		return FCNet()
	}
	return filepath.Join(FCNet(), group)
}

// AssetDir is where the build assets (Containerfile + image/ tree) live on disk:
// an explicit CS_SANDBOX_ASSETS_DIR, else the checkout root (bin/cs-sandbox next
// to go.mod), else the cwd. A downloaded binary with no checkout uses its
// embedded copy instead (the embed layer treats "" / a missing dir as "use embed").
func AssetDir() string {
	if d := os.Getenv("CS_SANDBOX_ASSETS_DIR"); d != "" {
		return d
	}
	if exe, err := os.Executable(); err == nil {
		root := filepath.Dir(filepath.Dir(exe)) // bin/cs-sandbox -> project root
		if fi, err := os.Stat(filepath.Join(root, "go.mod")); err == nil && !fi.IsDir() {
			return root
		}
	}
	wd, _ := os.Getwd()
	return wd
}

func dataHome() string {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return d
	}
	home := userHome()
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support")
	}
	return filepath.Join(home, ".local", "share")
}

func cacheHome() string {
	if d := os.Getenv("XDG_CACHE_HOME"); d != "" {
		return d
	}
	home := userHome()
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Caches")
	}
	return filepath.Join(home, ".cache")
}

func userHome() string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return h
	}
	return os.Getenv("HOME")
}
