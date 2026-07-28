// Package hostenv captures the host user's identity and SSH material — the "H"
// key set from the trust model. It is injectable: the real
// environment is discovered by Detect(), but tests construct a Host over a
// temporary HOME so the security-critical seed logic can be exercised without a
// real user.
package hostenv

import (
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/codesweep-ai/sandbox/internal/paths"
)

// Host is the host-side environment a sandbox is created from.
type Host struct {
	User    string   // id -un
	UID     int      // id -u
	GID     int      // id -g
	Group   string   // id -gn (empty if the gid has no name)
	Home    string   // $HOME
	Names   []string // hostname, hostname -s (deduped) — mapped to the host IP in the guest
	IsMacOS bool
}

// Detect reads the real host environment.
func Detect() (Host, error) {
	u, err := user.Current()
	if err != nil {
		return Host{}, err
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	home := u.HomeDir
	if h := os.Getenv("HOME"); h != "" {
		home = h
	}
	var group string
	if g, err := user.LookupGroupId(u.Gid); err == nil {
		group = g.Name
	}
	return Host{
		User:    u.Username,
		UID:     uid,
		GID:     gid,
		Group:   group,
		Home:    home,
		Names:   hostNames(),
		IsMacOS: runtime.GOOS == "darwin",
	}, nil
}

func hostNames() []string {
	set := map[string]struct{}{}
	if h, err := os.Hostname(); err == nil && h != "" {
		set[h] = struct{}{}
		if i := strings.IndexByte(h, '.'); i > 0 {
			set[h[:i]] = struct{}{} // short name (hostname -s)
		}
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// SSHDir is $HOME/.ssh.
func (h Host) SSHDir() string { return filepath.Join(h.Home, ".ssh") }

// SSHConfigDir is $HOME/.ssh/config.d.
func (h Host) SSHConfigDir() string { return filepath.Join(h.SSHDir(), "config.d") }

// SSHConfigFile is the managed include under $HOME/.ssh/config.d describing the
// instances root at instDir (see paths.SSHConfigFragment).
func (h Host) SSHConfigFile(instDir string) string {
	return filepath.Join(h.SSHConfigDir(), paths.SSHConfigFragment(instDir))
}

// pubFiles returns every non-empty ~/.ssh/*.pub, sorted, for determinism.
func (h Host) pubFiles() []string {
	matches, _ := filepath.Glob(filepath.Join(h.SSHDir(), "*.pub"))
	sort.Strings(matches)
	var out []string
	for _, f := range matches {
		if fi, err := os.Stat(f); err == nil && fi.Size() > 0 {
			out = append(out, f)
		}
	}
	return out
}

// PubKeys concatenates all host public keys (the "H" identity set), each on its
// own line. Returns empty (and ok=false) if none exist.
func (h Host) PubKeys() (keys string, ok bool) {
	var b strings.Builder
	for _, f := range h.pubFiles() {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		b.Write(data)
		if len(data) > 0 && data[len(data)-1] != '\n' {
			b.WriteByte('\n')
		}
		ok = true
	}
	return b.String(), ok
}

// QuoteConfigArg renders s as an ssh_config argument, double-quoting it when it
// contains whitespace — macOS puts our keys under "~/Library/Application
// Support/…", and an unquoted space makes ssh reject the whole file with
// "keyword identityfile extra arguments at end of line". A leading `~` still
// expands inside quotes (ssh expands after parsing).
func QuoteConfigArg(s string) string {
	if !strings.ContainsAny(s, " \t\r\n\"\\") {
		return s
	}
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\r", `\r`, "\n", `\n`)
	return `"` + replacer.Replace(s) + `"`
}

// IdentityLines emits an indented `IdentityFile ~/.ssh/<name>` line for every
// host public key that has a matching private key.
func (h Host) IdentityLines() string {
	var b strings.Builder
	for _, pub := range h.pubFiles() {
		priv := strings.TrimSuffix(pub, ".pub")
		if fi, err := os.Stat(priv); err != nil || fi.IsDir() {
			continue
		}
		b.WriteString("    IdentityFile ")
		b.WriteString(QuoteConfigArg("~/.ssh/" + filepath.Base(priv)))
		b.WriteByte('\n')
	}
	return b.String()
}

// IdentityBlock is IdentityLines() plus `IdentitiesOnly yes`, or "" if there is
// no usable host key. No trailing newline.
func (h Host) IdentityBlock() string {
	lines := h.IdentityLines()
	if lines == "" {
		return ""
	}
	return lines + "    IdentitiesOnly yes"
}
