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

// sunPathMax is the AF_UNIX sun_path limit, terminator included. Not a Linux
// tunable — it is the size of a struct field in the kernel ABI.
const sunPathMax = 108

// The Unix sockets a running sandbox creates in its instance directory. Their
// names live here, and not at the two places that create them, because
// ValidInstancePath has to budget for the longest: a name that drifted from that
// budget would bring back the silent truncation the check exists to prevent.
const (
	SockFwd   = "fwd.sock" // the host→VM ssh forwarder's, by internal/fcnet
	SockVsock = "vm.vsock" // Firecracker's vsock UDS, by internal/engine
)

var instanceSockets = []string{SockFwd, SockVsock}

// longestSocketName is the longest basename among instanceSockets — 8
// characters, both of them being that long.
//
// No room is reserved for the _<port> suffix Firecracker appends. That path
// serves a guest-initiated connection, and for one the host has to be listening
// on <uds>_<port> first; nothing here ever is. The guest only listens (socat
// VSOCK-LISTEN:22) and the host only dials the base path. Add a guest→host
// channel and this has to grow with it — until then the reserve would only
// spend six bytes of a tight budget rejecting names that work.
var longestSocketName = longestOf(instanceSockets)

func longestOf(names []string) string {
	longest := ""
	for _, n := range names {
		if len(n) > len(longest) {
			longest = n
		}
	}
	return longest
}

// ValidInstancePath rejects a (group, name) whose instance directory could not
// hold a Unix socket. ValidName and ValidGroup each bound one label at 63
// characters and cannot see the other, but both spend the SAME 108-byte budget:
// sockets live at <instances>/<group>/<name>/, so two individually legal names
// compose into an illegal path.
//
// This has to fail before anything is provisioned, because the failure it
// replaces is invisible. socat truncates an over-long UNIX-LISTEN path, binds a
// socket under the shortened name and exits 0; the forwarder then waits for a
// socket at the full path that will never appear, and the operator sees a
// readiness timeout naming a serial.log that was never written — several
// minutes and one indirection away from the cause.
//
// Only Firecracker puts sockets in the instance directory, so only Firecracker
// is constrained; a podman sandbox has no such path and its caller must not
// apply this.
func ValidInstancePath(instDir, group, name string) error {
	if group == "" {
		group = DefaultGroup
	}
	path := filepath.Join(Dir(instDir, group, name), longestSocketName)
	if len(path) < sunPathMax {
		return nil
	}
	over := len(path) - sunPathMax + 1
	return fmt.Errorf("sandbox %q in group %q needs a %d-byte socket path, %d over the %d-byte AF_UNIX limit (%s);\n"+
		"  shorten the name or the group by %d characters, or set CS_SANDBOX_HOME to a shorter directory",
		name, group, len(path), over, sunPathMax, path, over)
}

// DefaultGroup is the group a sandbox joins when none is named. It is an
// ordinary group in every respect — its own network, keys and gateway — so
// there is no unnamed special case anywhere below.
const DefaultGroup = "default"

// ValidGroup applies the sandbox-name rules to a group name: it becomes a
// Podman network, a key directory, an ssh alias suffix and a directory under
// the instances dir, so the same single-DNS-label restriction applies.
func ValidGroup(name string) error {
	if len(name) > 63 {
		return fmt.Errorf("group name %q is too long (max 63 characters)", name)
	}
	if !nameRe.MatchString(name) {
		return fmt.Errorf("invalid group name %q: use letters, digits and dashes only (must start and end alphanumeric)", name)
	}
	return nil
}

// ObjectName is a sandbox's host-global name: its Podman container and volumes,
// and the canonical reference the CLI accepts. Identity is (group, name) and
// these namespaces are host-global, so they carry the group — including the
// default group, which is an ordinary group here as everywhere else. The guest
// hostname and the in-network DNS alias stay bare, so members of a group keep
// reaching each other as plain <name>.
//
// It lives here because every layer that addresses a sandbox from outside must
// spell it the same way: an engine asking Podman for a bare name gets "no such
// object", and a caller that guessed differently gets a silent no-op.
func ObjectName(group, name string) string {
	if group == "" {
		group = DefaultGroup
	}
	return name + "." + group
}

// BranchName is the branch a --repo clone commits to, and the ref `fetch`
// writes into the HOST source repository. That repository sits outside every
// group, so unlike the guest-side name this one has to carry the group: two
// groups running the same fixture — the case groups exist for — would otherwise
// both target refs/heads/cs-sandbox/<name>, and the second fetch would be
// rejected as a non-fast-forward.
//
// The group is appended as <name>.<group>, the same spelling the CLI accepts,
// rather than nested as <group>/<name>. Nesting would put a directory where a
// ref may already be: a default-group sandbox `api` owns refs/heads/cs-sandbox/api,
// so a group named `api` could not then create refs/heads/cs-sandbox/api/<member>
// ("cannot lock ref"). Appended, the two are siblings and coexist.
//
// The default group keeps the bare form. It is what every example in README.md
// and docs/repo-sharing.md shows, it is what a host repo's existing branches are
// already called, and with one group there is nothing to disambiguate.
func BranchName(group, name string) string {
	if group == "" || group == DefaultGroup {
		return "cs-sandbox/" + name
	}
	return "cs-sandbox/" + ObjectName(group, name)
}

// NetworkName is the Podman network backing a group. The default group keeps
// the historical fabric name so existing docs, the host route and the fabric
// helpers keep referring to the same bridge.
func NetworkName(group string) string {
	if group == DefaultGroup {
		return "cs-sandbox-net"
	}
	return "cs-sandbox-" + group
}

// Group is an isolation boundary: one isolated network, one SSH key pair, one
// gateway. Members of a group reach each other; members of different groups do
// not, and cannot authenticate to one another even if they could.
type Group struct {
	Name    string `json:"name"`
	Created string `json:"created"` // RFC3339 UTC
	// TapPrefix is allocated once and recorded, never derived from a hash:
	// Linux interface names are host-global, so two groups that collided on a
	// hash would produce an interface-name clash far from its cause.
	TapPrefix string `json:"tapprefix"`
	// GWPort is the host port publishing this group's gateway (the keepalive
	// container, which also serves as the ssh jump host into the group).
	GWPort int `json:"gwport,omitempty"`
}

// GroupDir is the on-disk directory holding a group's record and its members.
func GroupDir(instDir, group string) string { return filepath.Join(instDir, group) }

func groupPath(instDir, group string) string {
	return filepath.Join(GroupDir(instDir, group), "group.json")
}

// SaveGroup writes the canonical group.json.
func SaveGroup(instDir string, g *Group) error {
	if g == nil {
		return fmt.Errorf("cannot save nil group")
	}
	if err := ValidGroup(g.Name); err != nil {
		return err
	}
	if err := os.MkdirAll(GroupDir(instDir, g.Name), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := groupPath(instDir, g.Name) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, groupPath(instDir, g.Name))
}

// LoadGroup reads a group record. Returns os.ErrNotExist if the group is absent.
func LoadGroup(instDir, group string) (*Group, error) {
	if err := ValidGroup(group); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(groupPath(instDir, group))
	if err != nil {
		return nil, err
	}
	var g Group
	if err := json.Unmarshal(data, &g); err != nil {
		return nil, fmt.Errorf("group %s: %w", group, err)
	}
	if g.Name != group {
		return nil, fmt.Errorf("group %s: record name is %q", group, g.Name)
	}
	return &g, nil
}

// ListGroups returns every group under instDir, sorted by name.
func ListGroups(instDir string) ([]*Group, error) {
	entries, err := os.ReadDir(instDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []*Group
	for _, e := range entries {
		if !e.IsDir() || ValidGroup(e.Name()) != nil {
			continue
		}
		g, err := LoadGroup(instDir, e.Name())
		if err != nil {
			continue // a directory without a group.json is not a group
		}
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
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
	Name    string   `json:"name"`
	Group   string   `json:"group"` // the group whose network, keys and gateway it uses
	Type    string   `json:"type"`  // user | agent
	Engine  Engine   `json:"engine"`
	Port    int      `json:"port"`
	FCIP    string   `json:"fcip,omitempty"` // firecracker VM address
	CPUs    int      `json:"cpus,omitempty"` // firecracker
	MemMiB  int      `json:"mem,omitempty"`  // firecracker
	Created string   `json:"created"`        // RFC3339 UTC
	Yolo    bool     `json:"yolo,omitempty"`
	Solo    bool     `json:"solo,omitempty"`
	Shared  []string `json:"shared,omitempty"` // image-store names
	// AgentLogins are the agents whose host login was inherited at create
	// (--inherit-agent-login), so `ls` can show which sandboxes hold credentials.
	AgentLogins []string    `json:"agentlogins,omitempty"`
	Snapshots   []string    `json:"snapshots,omitempty"` // "hostpath:name"
	RepoClones  []RepoClone `json:"repoclones,omitempty"`
}

// Dir returns the on-disk instance directory. Identity is (group, name), so the
// same sandbox name may exist in several groups.
func Dir(instDir, group, name string) string { return filepath.Join(GroupDir(instDir, group), name) }

func statePath(instDir, group, name string) string {
	return filepath.Join(Dir(instDir, group, name), "state.json")
}

// Save writes the canonical state.json.
func Save(instDir string, in *Instance) error {
	if in == nil {
		return fmt.Errorf("cannot save nil instance")
	}
	if err := ValidName(in.Name); err != nil {
		return err
	}
	if in.Group == "" {
		in.Group = DefaultGroup
	}
	if err := ValidGroup(in.Group); err != nil {
		return err
	}
	if err := os.MkdirAll(Dir(instDir, in.Group, in.Name), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(in, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := statePath(instDir, in.Group, in.Name) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, statePath(instDir, in.Group, in.Name))
}

// Load reads an instance's state.json. Returns os.ErrNotExist if it is absent.
func Load(instDir, group, name string) (*Instance, error) {
	if err := ValidName(name); err != nil {
		return nil, err
	}
	if err := ValidGroup(group); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(statePath(instDir, group, name))
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
	if in.Group == "" {
		in.Group = DefaultGroup
	}
	if in.Group != group {
		return nil, fmt.Errorf("state %s: record group is %q, found under %q", name, in.Group, group)
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
	for _, ge := range entries {
		if !ge.IsDir() || ValidGroup(ge.Name()) != nil {
			continue
		}
		group := ge.Name()
		members, err := os.ReadDir(GroupDir(instDir, group))
		if err != nil {
			continue
		}
		for _, e := range members {
			if !e.IsDir() || ValidName(e.Name()) != nil {
				continue
			}
			in, err := Load(instDir, group, e.Name())
			if err != nil {
				if os.IsNotExist(err) {
					continue // retained data from `rm`, not an active instance
				}
				if firstErr == nil {
					firstErr = fmt.Errorf("load instance %s.%s: %w", e.Name(), group, err)
				}
				continue
			}
			out = append(out, in)
		}
	}
	// Sorted by group then name, so a group's members stay adjacent everywhere
	// they are listed.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Group != out[j].Group {
			return out[i].Group < out[j].Group
		}
		return out[i].Name < out[j].Name
	})
	return out, firstErr
}
