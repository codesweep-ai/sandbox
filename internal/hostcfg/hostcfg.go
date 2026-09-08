// Package hostcfg generates the host-side SSH material: the reusable ssh option
// set for reaching a sandbox (keyed by HostKeyAlias, so two sandboxes of the
// same name in different groups key two entries), and the managed
// ~/.ssh/config.d include that makes `ssh <name>` work.
package hostcfg

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/codesweep-ai/sandbox/internal/hostenv"
	"github.com/codesweep-ai/sandbox/internal/state"
)

// KnownHostsFile is the dedicated known-hosts file (keyed by HostKeyAlias) so
// managed connections never collide with the user's main known_hosts.
func KnownHostsFile(h hostenv.Host) string {
	return filepath.Join(h.SSHDir(), "known_hosts.cs-sandbox")
}

// Route is how the host reaches one sandbox's sshd.
//
// A sandbox binds no host port. It used to bind one each, from a 100-port range
// per engine (R42) that every sandbox on the machine drew from — so two people,
// or two runs, competed for the same hundred numbers, and allocation could only
// see the ports of whoever was asking. What replaces it is a ProxyCommand: ssh
// gets its byte stream from something that already exists, and nothing is bound
// on the host at all.
//
// The stream differs by engine, and neither is new machinery. A container is
// entered with `podman exec`, which is how `exec` already reaches one. A microVM
// answers on the unix socket the fabric's forwarder already publishes for it,
// which used to have a host-side socat bolted onto it purely to turn it back
// into a TCP port.
//
// It narrows the reach as well as the pollution. A loopback port is open to
// every account on the machine; `podman exec` is refused to anyone but the
// owner, because another user's rootless podman does not have this container,
// and the microVM's socket is protected by the 0700 instance directory it sits
// in.
//
// Port is the opt-in: CS_SANDBOX_SSH_BIND publishes one for a caller that cannot
// run a ProxyCommand — another machine, or a tool that takes a host and a port.
type Route struct {
	Proxy string // the ProxyCommand ssh runs, or "" when Port is dialled instead
	Port  int    // a published host port, or 0 for none
}

// Published reports whether this route dials a host port.
func (r Route) Published() bool { return r.Proxy == "" && r.Port != 0 }

// RouteTo is how to reach this instance, published port first.
//
// instDir is where a microVM's forwarder socket lives, beside the instance whose
// stream it carries.
func RouteTo(instDir string, in *state.Instance) Route {
	if in.Port != 0 {
		return Route{Port: in.Port}
	}
	group := in.Group
	if group == "" {
		group = state.DefaultGroup
	}
	if in.Engine == state.Firecracker {
		return Route{Proxy: proxyUnix(filepath.Join(state.Dir(instDir, group, in.Name), state.SockFwd))}
	}
	return Route{Proxy: proxyExec(state.ObjectName(group, in.Name))}
}

// proxyExec reaches a container's sshd through the engine, with the socat the
// image is guaranteed to carry. `podman exec` needs no network of its own, so
// this works for a sandbox on any group's bridge and for one on none.
func proxyExec(obj string) string {
	return shellJoin("podman", "exec", "-i", obj, "socat", "-",
		fmt.Sprintf("TCP:127.0.0.1:%d", state.InternalSSHPort))
}

// proxyUnix reaches a microVM through the socket its forwarder already listens
// on inside the rootless namespace. The socket is a file in the instance
// directory, so what may open it is decided by the mode of that directory
// rather than by who can reach a loopback port.
func proxyUnix(sock string) string {
	return shellJoin("socat", "-", "UNIX-CONNECT:"+sock)
}

// SSHOptions returns the -o/-i option list for reaching sandbox `name` by this
// route with the user-tier key, keyed by HostKeyAlias.
func SSHOptions(h hostenv.Host, tierDir, name string, r Route) []string {
	opts := []string{
		"-i", filepath.Join(tierDir, "id_cs-sandbox_user"),
		"-o", "HostKeyAlias=" + name,
		"-o", "UserKnownHostsFile=" + KnownHostsFile(h),
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "IdentitiesOnly=yes",
	}
	if r.Proxy != "" {
		return append(opts, "-o", "ProxyCommand="+r.Proxy)
	}
	return append(opts, "-p", strconv.Itoa(r.Port))
}

// SSHDest is the destination that goes with SSHOptions.
//
// A proxied connection never resolves this name: ssh hands the stream to the
// ProxyCommand instead. It is the alias rather than a placeholder so that a log
// line, a prompt and an error all say which sandbox this was.
func SSHDest(h hostenv.Host, name string, r Route) string {
	if r.Proxy != "" {
		return h.User + "@" + name
	}
	return h.User + "@127.0.0.1"
}

// shellJoin renders a command line for somewhere a shell will read it: an ssh
// ProxyCommand, or GIT_SSH_COMMAND.
func shellJoin(argv ...string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = shellQuote(a)
	}
	return strings.Join(parts, " ")
}

// SSHCommandString renders SSHOptions as a single `ssh …` string for
// GIT_SSH_COMMAND / core.sshCommand, so git transport reaches a sandbox without
// depending on the user's ssh config.
func SSHCommandString(h hostenv.Host, tierDir, name string, r Route) string {
	parts := append([]string{"ssh"}, SSHOptions(h, tierDir, name, r)...)
	// Git runs this through a shell, so quote every argument containing shell
	// metacharacters. In particular, macOS keeps our keys under
	// "~/Library/Application Support/…".
	for i, p := range parts {
		parts[i] = shellQuote(p)
	}
	return strings.Join(parts, " ")
}

func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' ||
			r >= 'A' && r <= 'Z' ||
			r >= '0' && r <= '9' ||
			strings.ContainsRune("%+,-./:=@_", r))
	}) == -1 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// Ref is a sandbox's canonical, always-unambiguous ssh alias: the host-global
// object name, so the alias create purges cannot drift from the one emitted here.
func Ref(in *state.Instance) string {
	return state.ObjectName(in.Group, in.Name)
}

// GroupKeysDir maps the base key directory to one group's key directory. It
// mirrors paths.GroupKeys, kept here so hostcfg does not import paths.
func GroupKeysDir(tierBase, group string) string {
	return filepath.Join(tierBase, "groups", group)
}

// SyncSSHConfig regenerates this instances root's ~/.ssh/config.d fragment with
// one Host block per instance and ensures ~/.ssh/config includes it. It works
// off the typed state records, so containers and microVMs are written the same
// way. Only this root's fragment is touched: ~/.ssh is shared by every root on
// the host, so rewriting a single shared file would drop the Host blocks of
// sandboxes this root cannot see.
func SyncSSHConfig(h hostenv.Host, tierDir, instDir string, insts []*state.Instance, groups []*state.Group) error {
	if err := os.MkdirAll(h.SSHConfigDir(), 0o700); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("# Managed by cs-sandbox — do not edit. Regenerated by 'cs-sandbox sync-ssh-config'.\n")
	kh := KnownHostsFile(h)
	idLines := h.IdentityLines() // host keys (H), if any
	// A bare `ssh <name>` alias belongs to the default group and to nothing else,
	// matching how the CLI resolves a bare reference — so ssh and cs-sandbox can
	// never disagree about which sandbox a name denotes. Emitting it for whichever
	// group held the name uniquely made that meaning depend on the rest of the
	// host, and ssh_config takes the FIRST match for a keyword, so a later
	// collision would silently connect to whichever block was written first.
	blocks := 0
	// One gateway alias per group. The per-sandbox blocks below are the direct
	// path and keep working when a gateway is down; the gateway is what reaches
	// a name on the group's network that is not a member — the credential
	// lender, a recorder — without having to pick a member to go through.
	for _, g := range groups {
		gwKey := filepath.Join(GroupKeysDir(tierDir, g.Name), "id_cs-sandbox_user")
		blocks++
		fmt.Fprintf(&b, "\nHost %s-gw\n", g.Name)
		// The same choice a sandbox gets: a published port where one was asked
		// for, and otherwise the engine's own channel into the container.
		if g.GWPort != 0 {
			fmt.Fprintf(&b, "    HostName 127.0.0.1\n")
			fmt.Fprintf(&b, "    Port %d\n", g.GWPort)
		} else {
			fmt.Fprintf(&b, "    ProxyCommand %s\n",
				proxyExec(state.KeepaliveFor(state.NetworkName(g.Name))))
		}
		fmt.Fprintf(&b, "    User %s\n", h.User)
		fmt.Fprintf(&b, "    HostKeyAlias %s-gw\n", g.Name)
		// Only the group key, deliberately: the gateway authorizes nothing else,
		// and offering the host's own identities first would exhaust sshd's
		// MaxAuthTries before this one was ever tried.
		fmt.Fprintf(&b, "    IdentityFile %s\n", hostenv.QuoteConfigArg(gwKey))
		b.WriteString("    IdentitiesOnly yes\n")
		b.WriteString("    StrictHostKeyChecking accept-new\n")
		fmt.Fprintf(&b, "    UserKnownHostsFile %s\n", hostenv.QuoteConfigArg(kh))
	}
	for _, in := range insts {
		blocks++
		// Trust material is per group: the key that opens this sandbox opens
		// nothing in any other group.
		group := in.Group
		if group == "" {
			group = state.DefaultGroup
		}
		aliases := Ref(in)
		if group == state.DefaultGroup {
			aliases += " " + in.Name
		}
		userKey := filepath.Join(GroupKeysDir(tierDir, group), "id_cs-sandbox_user")
		fmt.Fprintf(&b, "\nHost %s\n", aliases)
		// One or the other, never both: a published port is the opt-in, and a
		// ProxyCommand is what a sandbox has when nothing is published for it.
		if r := RouteTo(instDir, in); r.Published() {
			fmt.Fprintf(&b, "    HostName 127.0.0.1\n")
			fmt.Fprintf(&b, "    Port %d\n", r.Port)
		} else {
			fmt.Fprintf(&b, "    ProxyCommand %s\n", r.Proxy)
		}
		fmt.Fprintf(&b, "    User %s\n", h.User)
		fmt.Fprintf(&b, "    HostKeyAlias %s\n", Ref(in))
		// The host reaches sandboxes with its own keys (H) if it has any, plus the
		// group's user-tier key U as the fallback authorized in its members.
		b.WriteString(idLines)
		fmt.Fprintf(&b, "    IdentityFile %s\n", hostenv.QuoteConfigArg(userKey))
		b.WriteString("    IdentitiesOnly yes\n")
		b.WriteString("    StrictHostKeyChecking accept-new\n")
		fmt.Fprintf(&b, "    UserKnownHostsFile %s\n", hostenv.QuoteConfigArg(kh))
	}
	if blocks == 0 {
		// A root with nothing to describe leaves no fragment behind, so a
		// throwaway root does not litter ~/.ssh/config.d permanently.
		if err := os.Remove(h.SSHConfigFile(instDir)); err != nil && !os.IsNotExist(err) {
			return err
		}
		return ensureInclude(h)
	}
	if err := writeFileAtomic(h.SSHConfigFile(instDir), []byte(b.String()), 0o600); err != nil {
		return err
	}
	return ensureInclude(h)
}

// includeDirective is the one line cs-sandbox maintains in ~/.ssh/config. The
// pattern is a glob so a single directive covers every instances root's
// fragment; ssh reads glob matches in sorted order, so the default root (the
// plain name) wins if two roots use the same sandbox name.
const includeDirective = "Include ~/.ssh/config.d/cs-sandbox*"

// ensureInclude points ~/.ssh/config at the managed fragments: an Include of a
// cs-sandbox fragment is updated in place (keeping the user's ordering), and one
// is prepended if there is none.
func ensureInclude(h hostenv.Host) error {
	cfg := filepath.Join(h.SSHDir(), "config")
	existing, err := os.ReadFile(cfg)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	lines := strings.Split(string(existing), "\n")
	for i, line := range lines {
		if !isManagedInclude(line) {
			continue
		}
		if strings.TrimSpace(line) == includeDirective {
			return nil
		}
		lines[i] = includeDirective
		return writeFileAtomic(cfg, []byte(strings.Join(lines, "\n")), 0o600)
	}
	return writeFileAtomic(cfg, []byte(includeDirective+"\n\n"+string(existing)), 0o600)
}

// isManagedInclude reports whether a ~/.ssh/config line is the Include of a
// cs-sandbox fragment — the line this package owns and rewrites.
func isManagedInclude(line string) bool {
	f := strings.Fields(strings.TrimSpace(line))
	return len(f) == 2 && strings.EqualFold(f[0], "Include") &&
		strings.Contains(f[1], "config.d/cs-sandbox")
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) (err error) {
	// Preserve symlinked dotfile setups. Rename would otherwise replace the
	// symlink itself; resolving it lets us update the target atomically.
	if fi, lerr := os.Lstat(path); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
		resolved, rerr := filepath.EvalSymlinks(path)
		if rerr != nil {
			// Match os.WriteFile's behavior for a dangling symlink: follow it
			// and create the target when its parent exists.
			return os.WriteFile(path, data, mode)
		}
		path = resolved
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		_ = f.Close()
		_ = os.Remove(tmp)
	}()
	if err := f.Chmod(mode); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
