package lend

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"
)

// CONNECT tunnelling: the half of an agent's traffic a base URL does not
// govern.
//
// Pointing an agent at the lender aims its MODEL calls here and nothing else.
// These clients also talk to hosts of their own choosing — Claude Code checks
// its session against api.anthropic.com, Codex reaches chatgpt.com for its
// subscription transport and ab.chatgpt.com for experiment assignment — and
// they do it whatever base URL they were given. A sandbox holding a loan token
// has no credential those hosts accept, so each such call fails, and an agent
// whose session check just failed reports itself signed out while its model
// calls are working perfectly.
//
// So the sandbox's HTTPS_PROXY points here too, and this refuses exactly the
// hosts the lender is itself the front for, plus the ones these agents phone
// home to. A side call to a fronted host is by definition a call that went
// around the swap, and blocking it is what makes "this sandbox reaches the
// model only through the lender" a property rather than a hope.
//
// Everything else is tunnelled rather than blocked, because an agent's TOOLS
// share its environment: a blanket refusal takes away git, curl and every
// package manager the agent might shell out to, and none of those have anything
// to do with the credential.
//
// No certificate is involved. A CONNECT proxy pipes bytes; TLS stays end to end
// between the client and the host it dialled, and the hostname decided on here
// is the one in the CONNECT line, in the clear, before any of that starts.

// phoneHome is what these agents contact on their own, beyond the hosts the
// slot table already names. Short on purpose: github.com is deliberately absent
// — an agent's tools reach it constantly and nothing it answers has to do with
// a credential this host lends.
var phoneHome = []string{"ab.chatgpt.com"}

// blockedHosts is the refusal list: every upstream this build fronts, plus the
// phone-home hosts. Deriving most of it from the slot table is what keeps the
// two in step — a provider added there is refused here on the same commit.
func blockedHosts() []string {
	out := append([]string{}, Origins()...)
	return append(out, phoneHome...)
}

// tunnelRefusal reports why a host may not be tunnelled, or "" to allow it.
func tunnelRefusal(host string) string {
	if !slices.Contains(blockedHosts(), host) {
		return ""
	}
	return "cs-sandbox does not tunnel " + host +
		": a sandbox reaches it through the lender, which is what puts the host's real credential on the request"
}

// hostOnly drops a port and a trailing dot, so "API.Example.com.:443" and
// "api.example.com" are the same host.
func hostOnly(hostport string) string {
	h := hostport
	if x, _, err := net.SplitHostPort(hostport); err == nil {
		h = x
	}
	return strings.ToLower(strings.TrimSuffix(h, "."))
}

// serveConnect answers a CONNECT: refuse the fronted hosts, tunnel the rest.
func (s *Server) serveConnect(w http.ResponseWriter, r *http.Request) {
	target := r.Host // CONNECT carries authority-form: host:port
	host := hostOnly(target)
	if why := tunnelRefusal(host); why != "" {
		s.count(func(st *Stats) { st.Blocked++ })
		s.cfg.Log.Info("tunnel refused", slog.String("host", host))
		writeError(w, http.StatusForbidden, "tunnel_blocked", why)
		return
	}

	upstream, err := net.DialTimeout("tcp", target, 10*time.Second)
	if err != nil {
		s.cfg.Log.Warn("tunnel could not be opened", slog.String("host", host), slog.Any("err", err))
		writeError(w, http.StatusBadGateway, "tunnel_failed", err.Error())
		return
	}
	defer upstream.Close()

	hj, ok := w.(http.Hijacker)
	if !ok {
		writeError(w, http.StatusInternalServerError, "tunnel_unsupported", "this server cannot hijack a connection")
		return
	}
	client, buf, err := hj.Hijack()
	if err != nil {
		return
	}
	defer client.Close()

	if _, err := client.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n")); err != nil {
		return
	}
	s.count(func(st *Stats) { st.Tunnels++ })

	// Both directions, and the first to end finishes the tunnel: a peer that
	// closed has nothing more to say, and the deferred closes free the other.
	// buf rather than client for the read side — the client may already have
	// sent bytes that landed in the server's buffer.
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, buf); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, upstream); done <- struct{}{} }()
	<-done
}
