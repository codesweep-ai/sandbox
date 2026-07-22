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
