// Package state is the typed per-instance record: a sandbox's engine, type,
// port, VM address, and shares. It is the single source of truth the CLI and
// both engines read, and persists as instances/<name>/state.json.
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
)

// nameRe restricts a sandbox name to a single DNS label: it becomes the guest
// hostname, an `ssh <name>` alias in the managed host config, a dnsmasq hosts
// entry, and a directory under the instances dir. Dots are rejected on purpose —
// the in-guest client config treats a dotless name as a fabric peer ("Host *
// !*.*"), so a dotted sandbox name would not resolve as a peer.
var nameRe = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?$`)

// ValidName reports whether a sandbox name is acceptable. Besides keeping names
// resolvable, this is the gate that keeps a name from escaping the instances dir
// (filepath.Join resolves "..") or injecting into the generated ssh config.
func ValidName(name string) error {
	if len(name) > 63 {
		return fmt.Errorf("sandbox name %q is too long (max 63 characters)", name)
	}
	if !nameRe.MatchString(name) {
		return fmt.Errorf("invalid sandbox name %q: use letters, digits and dashes only (must start and end alphanumeric)", name)
	}
	return nil
}

// Engine identifies the sandbox engine.
type Engine string

const (
	Podman      Engine = "podman"
	Firecracker Engine = "firecracker"
)

// RepoClone records one --repo share: which host repo was shared, where it
// landed in the sandbox, and the branch fetch/push move commits on.
type RepoClone struct {
	Source string `json:"source"` // host repo path
	Dir    string `json:"dir"`    // ~/<dir> in the sandbox
	Branch string `json:"branch"` // cs-sandbox/<name>
}

// Instance is the canonical typed state of one sandbox.
type Instance struct {
	Name       string      `json:"name"`
	Type       string      `json:"type"` // user | agent
	Engine     Engine      `json:"engine"`
	Port       int         `json:"port"`
	FCIP       string      `json:"fcip,omitempty"` // firecracker VM address
	CPUs       int         `json:"cpus,omitempty"` // firecracker
	MemMiB     int         `json:"mem,omitempty"`  // firecracker
	Created    string      `json:"created"`        // RFC3339 UTC
	Yolo       bool        `json:"yolo,omitempty"`
	Solo       bool        `json:"solo,omitempty"`
	Shared     []string    `json:"shared,omitempty"`    // image-store names
	Snapshots  []string    `json:"snapshots,omitempty"` // "hostpath:name"
	RepoClones []RepoClone `json:"repoclones,omitempty"`
}

// Dir returns the on-disk instance directory for name under instDir.
func Dir(instDir, name string) string { return filepath.Join(instDir, name) }

func statePath(instDir, name string) string { return filepath.Join(Dir(instDir, name), "state.json") }

// Save writes the canonical state.json.
func Save(instDir string, in *Instance) error {
	if in == nil {
		return fmt.Errorf("cannot save nil instance")
	}
	if err := ValidName(in.Name); err != nil {
		return err
	}
	if err := os.MkdirAll(Dir(instDir, in.Name), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(in, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := statePath(instDir, in.Name) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, statePath(instDir, in.Name))
}

// Load reads an instance's state.json. Returns os.ErrNotExist if it is absent.
func Load(instDir, name string) (*Instance, error) {
	if err := ValidName(name); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(statePath(instDir, name))
	if err != nil {
		return nil, err
	}
	var in Instance
	if err := json.Unmarshal(data, &in); err != nil {
		return nil, fmt.Errorf("state %s: %w", name, err)
	}
	if err := ValidName(in.Name); err != nil {
		return nil, fmt.Errorf("state %s: %w", name, err)
	}
	if in.Name != name {
		return nil, fmt.Errorf("state %s: record name is %q", name, in.Name)
	}
	return &in, nil
}

// List returns every instance found under instDir. A corrupt record is reported
// as an error, but the instances that did load are still returned alongside it:
// callers that allocate ports and VM addresses treat this as best-effort
// (`insts, _ := List(...)`), and must see every live claim or they hand out one
// that is already taken. Directories that cannot be instances at all — an
// invalid name, or data retained by `rm` with no state.json — are skipped
// silently.
func List(instDir string) ([]*Instance, error) {
	entries, err := os.ReadDir(instDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []*Instance
	var firstErr error
	for _, e := range entries {
		if !e.IsDir() || ValidName(e.Name()) != nil {
			continue
		}
		in, err := Load(instDir, e.Name())
		if err != nil {
			if os.IsNotExist(err) {
				continue // retained data from `rm`, not an active instance
			}
			if firstErr == nil {
				firstErr = fmt.Errorf("load instance %s: %w", e.Name(), err)
			}
			continue
		}
		out = append(out, in)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, firstErr
}
