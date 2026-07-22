// Package seed builds the per-instance seed — the security-critical interface
// between cs-sandbox and the guest init. The trust model (H/U/G keys, --solo
// withholding, the ssh_config Match-exec guard) is expressed as pure functions
// returning content, so it is exhaustively unit-testable.
package seed

import (
	"fmt"
	"regexp"
	"strings"
)

// Type is the sandbox type, which governs the trust model.
type Type string

const (
	User  Type = "user"
	Agent Type = "agent"
)

// Tier names — the generated per-tier keys (the "U" and "G" identities).
const (
	TierUserKey  = "id_cs-sandbox_user"
	TierAgentKey = "id_cs-sandbox_agent"
)

var ipv4Re = regexp.MustCompile(`^[0-9]{1,3}(\.[0-9]{1,3}){3}$`)

// TierKey selects the private tier key an instance holds (the only credential
// it can ssh OUT with). The single rule that produces the whole trust matrix:
//   - a --solo agent gets NO tier key (no outbound auth anywhere);
//   - a user sandbox holds U;
//   - an agent sandbox holds G.
func TierKey(typ Type, solo bool) string {
	switch {
	case solo:
		return ""
	case typ == User:
		return TierUserKey
	default:
		return TierAgentKey
	}
}

// AuthorizedKeys assembles a sandbox's authorized_keys: H (+ U) (+ G for agent).
// It is IDENTICAL for solo and non-solo (peers and the host can still ssh INTO a
// --solo sandbox; solo is an OUTBOUND restriction only). userTierPub/agentTierPub
// are the contents of the two tier .pub files.
//
// The single rule that blocks "agent → user": G (agentTierPub) is only appended
// for an agent sandbox, so it is never authorized in a user sandbox.
func AuthorizedKeys(hostPubs, userTierPub, agentTierPub string, typ Type) []byte {
	var b strings.Builder
	writeLines(&b, hostPubs)
	writeLines(&b, userTierPub)
	if typ == Agent {
		writeLines(&b, agentTierPub)
	}
	return []byte(b.String())
}

// SSHClientConfig generates the in-instance ~/.ssh/config.d/cs-sandbox client
// config — the trickiest piece.
//
//   - "Host * !*.*" matches dotless names (peer sandboxes); dotted hosts (github.com,
//     FQDNs) keep ssh defaults.
//   - Both non-solo types pin their tier key (IdentityFile) for peer reach.
//   - A `Match exec` restricts IdentitiesOnly to real peers — names resolving onto
//     the fabric subnet — so `ssh -A` cannot substitute a forwarded host key for
//     the tier key on peer connections, while still letting a forwarded agent reach
//     external hosts.
//   - Agents additionally pin PreferredAuthentications=publickey so a rejected key
//     fails fast instead of hanging on a host password prompt.
//
// fabricGW is the podman network gateway (e.g. 10.89.0.1). For any non-solo
// sandbox (tierKey != "") a valid IPv4 gateway is REQUIRED — otherwise we return
// an error rather than emit a config that silently omits the guard and would let
// `ssh -A` leak the host key to peers (a hard tripwire).
func SSHClientConfig(typ Type, tierKey, fabricGW string) (string, error) {
	var b strings.Builder
	b.WriteString("# Managed by cs-sandbox — sandbox→peer auth uses the tier key; other hosts may use a forwarded agent.\n")

	if tierKey != "" {
		if !ipv4Re.MatchString(fabricGW) {
			return "", fmt.Errorf("could not determine the fabric subnet from the podman network (gateway=%q); the network must be up first", fabricGW)
		}
		fabricRe := fabricRegexp(fabricGW)
		// A dotless name resolving onto the fabric subnet is a real peer -> restrict
		// it to the tier key so `ssh -A` cannot substitute a forwarded host key.
		fmt.Fprintf(&b, "Match exec \"case %%h in *.*) exit 1 ;; esac; getent ahostsv4 %%h 2>/dev/null | grep -q '%s'\"\n", fabricRe)
		b.WriteString("    IdentitiesOnly yes\n")
	}
	b.WriteString("Host * !*.*\n")
	if tierKey != "" {
		fmt.Fprintf(&b, "    IdentityFile ~/.ssh/%s\n", tierKey)
	}
	if typ == Agent {
		b.WriteString("    PreferredAuthentications publickey\n")
	}
	b.WriteString("    StrictHostKeyChecking accept-new\n")
	return b.String(), nil
}

// fabricRegexp derives the peer-subnet anchor, e.g. 10.89.0.1 -> ^10[.]89[.]0[.]
// (the gateway with its last octet stripped, dots escaped for the regex).
func fabricRegexp(gw string) string {
	prefix := gw[:strings.LastIndexByte(gw, '.')] + "." // strip last octet, keep trailing dot
	return "^" + strings.ReplaceAll(prefix, ".", "[.]")
}

// GitIdentity is the host's global git user.name/email carried into the
// instance's ~/.gitconfig. Serialized as the seed git_identity file
// ("name\t<name>\nemail\t<email>\n").
type GitIdentity struct {
	Name  string
	Email string
}

// File renders the git_identity seed file, or "" (write nothing) if both empty.
func (g GitIdentity) File() string {
	name := cleanGitIdentity(g.Name)
	email := cleanGitIdentity(g.Email)
	if name == "" && email == "" {
		return ""
	}
	return fmt.Sprintf("name\t%s\nemail\t%s\n", name, email)
}

func cleanGitIdentity(value string) string {
	value = strings.Map(func(r rune) rune {
		switch r {
		case '\r', '\n', '\t':
			return ' '
		default:
			return r
		}
	}, value)
	return strings.TrimSpace(value)
}

// writeLines appends s, guaranteeing a trailing newline if non-empty.
func writeLines(b *strings.Builder, s string) {
	if s == "" {
		return
	}
	b.WriteString(s)
	if s[len(s)-1] != '\n' {
		b.WriteByte('\n')
	}
}
