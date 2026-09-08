package lend

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// Config is everything the lender needs. Every path arrives from the caller, so
// this package looks nowhere it was not pointed.
type Config struct {
	Home    string // the host home holding the ~/.cs-<agent> profiles
	KeysDir string // where the lendable API keys are, one file per provider
	Loans   Loans  // resolves a loan token to the sandbox and slot it stands for
	Log     *slog.Logger

	// Callers says which peers may be answered. The zero value is the strict
	// one, CallersHost, so a Config that forgets the field is not open.
	Callers Callers

	// Origins points a slot at a different upstream, by slot id. It is how a
	// host puts something of its own in front of a provider — a gateway, or a
	// recorder — and it is the operator's choice rather than the caller's: no
	// part of a request can reach it.
	Origins map[string]string
}

// Stats is the accounting a session prints on the way out: enough to tell "the
// sandbox never called" from "it called and was refused".
type Stats struct {
	Requests  int `json:"requests"`
	Lent      int `json:"lent"`
	Refused   int `json:"refused"`
	Tunnels   int `json:"tunnels"`
	Blocked   int `json:"blocked"`
	NotLocal  int `json:"not_local"`
	Upstream5 int `json:"upstream_errors"`
}

// Server is the lender's HTTP handler: an origin-mode proxy for model calls,
// and a CONNECT proxy for everything else the sandbox reaches.
type Server struct {
	cfg Config

	mu    sync.Mutex
	stats Stats

	localMu   sync.Mutex
	localNets []*net.IPNet
	localAt   time.Time
}

// New returns a lender for this configuration.
func New(cfg Config) *Server {
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.KeysDir == "" {
		cfg.KeysDir = KeysDir(cfg.Home)
	}
	return &Server{cfg: cfg}
}

// Snapshot returns the counters so far.
func (s *Server) Snapshot() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}

func (s *Server) count(f func(*Stats)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(&s.stats)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The host's own addresses are the only ones served, and that is the whole
	// answer to the port being non-loopback.
	//
	// It has to be non-loopback: a sandbox reaches the host at 169.254.1.2,
	// which arrives on the host's ordinary side, where a server bound to
	// 127.0.0.1 refuses the connection (R52). Binding wider than loopback puts
	// the port on the host's network — so the peer is checked instead. Traffic
	// from a sandbox is translated to the host's own address on the way through
	// the rootless network, and a machine on the same network cannot claim that
	// address without being on the path.
	if !s.peerAllowed(r.RemoteAddr) {
		s.count(func(st *Stats) { st.NotLocal++ })
		s.cfg.Log.Warn("refused a caller from outside the boundary",
			slog.String("remote", r.RemoteAddr), slog.String("callers", string(s.callers())))
		writeError(w, http.StatusForbidden, "not_local",
			"cs-sandbox lends credentials to the sandboxes inside its own boundary, and this connection came from outside it")
		return
	}

	// CONNECT first: it carries an authority rather than a path, so every
	// decision below would read it wrongly. See tunnelRefusal for why a lender
	// answers CONNECT at all.
	if r.Method == http.MethodConnect {
		s.serveConnect(w, r)
		return
	}

	// A health check needs no loan: it is how `create` and `doctor` tell a
	// listening lender from a port that merely answers.
	if r.URL.Path == "/healthz" {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "service": "cs-sandbox lend"})
		return
	}

	s.count(func(st *Stats) { st.Requests++ })

	presented := credentialValues(r.Header)
	if len(presented) == 0 {
		s.count(func(st *Stats) { st.Refused++ })
		s.cfg.Log.Warn("refused: no credential", slog.String("path", r.URL.Path))
		writeError(w, http.StatusUnauthorized, "no_loan",
			"this request carried no credential: a sandbox reaches the lender with the loan cs-sandbox seeded into it")
		return
	}
	var loan Loan
	var ok bool
	for _, v := range presented {
		if loan, ok = s.cfg.Loans.Lookup(v); ok {
			break
		}
	}
	if !ok {
		s.count(func(st *Stats) { st.Refused++ })
		// The value is never logged or echoed. What arrives here is usually one
		// of ours, but a client that was configured by hand could present a real
		// credential, and neither a log file nor an error body is a place to put
		// one.
		s.cfg.Log.Warn("refused: no loan matches the credential presented",
			slog.String("path", r.URL.Path), slog.String("remote", r.RemoteAddr))
		// Never forwarded anywhere. An unrecognized credential resolves to no
		// slot and therefore to no upstream, and passing it to a default would
		// make this an open relay for anything that can reach the port.
		writeError(w, http.StatusUnauthorized, "unknown_loan",
			"no loan matches the credential this request carried: it was revoked with its sandbox, or it was never minted here")
		return
	}
	slot, ok := SlotByID(loan.Slot)
	if !ok {
		s.count(func(st *Stats) { st.Refused++ })
		writeError(w, http.StatusInternalServerError, "unknown_slot",
			fmt.Sprintf("this loan names a slot this build does not have (%q)", loan.Slot))
		return
	}

	// Most specific first: what this sandbox was created with, then what this
	// host runs its lender with, then the slot's own. None of the three is
	// reachable from a request, which is what R147 turns on.
	switch {
	case loan.Origin != "":
		slot.Origin = loan.Origin
	case s.cfg.Origins[slot.ID] != "":
		slot.Origin = s.cfg.Origins[slot.ID]
	}
	secret, extra, err := slot.read(s.cfg.Home, s.cfg.KeysDir)
	if err != nil {
		s.count(func(st *Stats) { st.Refused++ })
		s.cfg.Log.Error("cannot read what this loan lends",
			slog.String("slot", slot.ID), slog.String("sandbox", loan.Name), slog.Any("err", err))
		writeError(w, http.StatusUnauthorized, "no_credential", err.Error())
		return
	}

	s.count(func(st *Stats) { st.Lent++ })
	s.cfg.Log.Info("lending",
		slog.String("sandbox", loan.Name), slog.String("slot", slot.ID),
		slog.String("loan", loan.Label),
		slog.String("method", r.Method), slog.String("path", r.URL.Path))
	s.forward(w, r, slot, secret, extra)
}

// forward swaps the loan token for the real credential and proxies to the
// slot's own upstream.
//
// The body is never read, parsed or rewritten: the swap is one header, so a
// stream passes through byte for byte and a request shape this build has never
// seen still works.
func (s *Server) forward(w http.ResponseWriter, r *http.Request, slot Slot, secret string, extra map[string]string) {
	base, err := url.Parse(slot.Origin)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "bad_origin", "this slot's upstream is not a URL")
		return
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(base)
			// SetURL keeps the inbound Host, which upstream TLS vhosting
			// rejects; the request must claim the host it is going to.
			pr.Out.Host = base.Host
			// The version segment, exactly once, whichever of the two
			// conventions the client follows. Everything else about the path
			// is the client's and is passed through untouched.
			pr.Out.URL.Path = joinPath(base.Path, ensureVersion(slot.Version, pr.In.URL.Path))
			pr.Out.URL.RawPath = ""

			// Every credential the caller sent goes, not just the one the loan
			// arrived in: a sandbox holding a stale variable from an earlier
			// create must not have it reach a provider beside the real one.
			for _, h := range credentialHeaders {
				pr.Out.Header.Del(h)
			}
			pr.Out.Header.Set(slot.Header, slot.Prefix+secret)
			for k, v := range extra {
				if mergeHeaders[strings.ToLower(k)] {
					addExtra(pr.Out.Header, k, v)
					continue
				}
				// Everything else replaces what arrived. A seeded credential
				// makes the client send an identity of its own — a fabricated
				// account id — and the real one has to take its place.
				pr.Out.Header.Set(k, v)
			}
			// The sandbox's own view of the network is not the upstream's
			// business, and a forwarding header would hand it over.
			for _, h := range []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "Forwarded"} {
				pr.Out.Header.Del(h)
			}
		},
		// -1 flushes each write straight through, which is what a token stream
		// needs: the default buffering turns a token-by-token response into one
		// that arrives in lumps.
		FlushInterval: -1,
		ErrorLog:      slog.NewLogLogger(s.cfg.Log.Handler(), slog.LevelError),
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			s.count(func(st *Stats) { st.Upstream5++ })
			s.cfg.Log.Error("upstream failed", slog.String("origin", slot.Origin), slog.Any("err", err))
			writeError(w, http.StatusBadGateway, "upstream_error", "the upstream request failed: "+err.Error())
		},
	}
	rp.ServeHTTP(w, r)
}

// ensureVersion returns p with the provider's version segment present exactly
// once. An empty version leaves the path alone.
func ensureVersion(version, p string) string {
	if version == "" || p == version || strings.HasPrefix(p, version+"/") {
		return p
	}
	return version + p
}

// joinPath glues two path segments with exactly one slash between them.
func joinPath(a, b string) string {
	switch {
	case a == "" || a == "/":
		return b
	case strings.HasSuffix(a, "/") && strings.HasPrefix(b, "/"):
		return a + b[1:]
	case !strings.HasSuffix(a, "/") && !strings.HasPrefix(b, "/"):
		return a + "/" + b
	}
	return a + b
}

// credentialHeaders are every header these providers carry a credential in.
// Deleted as a set before the real one is attached.
var credentialHeaders = []string{
	"Authorization",
	"X-Api-Key",
	"Api-Key",
	"Proxy-Authorization",
}

// mergeHeaders are the headers whose value is a list the client contributes to
// as well. Ours is added to what arrived; every other header the credential
// travels with replaces it.
var mergeHeaders = map[string]bool{"anthropic-beta": true}

// addExtra sets a header the real credential travels with, without discarding
// what the client already sent. anthropic-beta is a comma-separated list a
// client uses for its own features, so ours is appended to it rather than
// written over it.
func addExtra(h http.Header, k, v string) {
	cur := h.Get(k)
	if cur == "" {
		h.Set(k, v)
		return
	}
	for part := range strings.SplitSeq(cur, ",") {
		if strings.EqualFold(strings.TrimSpace(part), v) {
			return
		}
	}
	h.Set(k, cur+","+v)
}

// credentialValues returns what the client presented, in the order to try.
//
// Which header carries it depends on the agent and on what it believes it is
// holding, and the lender does not need to care: it looks each candidate up in
// the loan table, and a value that is not there is refused. Matching on the
// table rather than on the shape of the value is what lets a loan be a whole
// forged token rather than something with a prefix to recognize.
func credentialValues(h http.Header) []string {
	var out []string
	if v := strings.TrimSpace(h.Get("Authorization")); v != "" {
		if after, ok := cutPrefixFold(v, "Bearer "); ok {
			out = append(out, strings.TrimSpace(after))
		} else {
			out = append(out, v)
		}
	}
	for _, k := range []string{"X-Api-Key", "Api-Key"} {
		if v := strings.TrimSpace(h.Get(k)); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
		return s[len(prefix):], true
	}
	return "", false
}

// Callers names the boundary a lender answers inside. It exists because the
// lender has two homes now, and "who may call me" is a different sentence in
// each.
type Callers string

const (
	// CallersHost is the lender as a host process. A sandbox reaches it at the
	// host's own address (SPEC R152), so the callers to accept are this host's
	// addresses and nothing else. This is the zero value: a Config that names
	// no policy gets the strict one.
	CallersHost Callers = ""

	// CallersNetwork is the lender ON the fabric, in a container attached to
	// one group's network. Its callers arrive from that network's subnet, and
	// the container's namespace is what keeps everyone else out — it has no
	// interface any other network can reach. So the check here is a second
	// fence rather than the boundary: accept the subnets this lender is
	// actually attached to, refuse everything else.
	CallersNetwork Callers = "network"

	// CallersAny turns the check off, for a test that dials from wherever the
	// test framework happened to bind.
	CallersAny Callers = "any"
)

// ParseCallers reads the flag value, so an unknown one is refused where it is
// typed rather than silently becoming the strict policy.
func ParseCallers(v string) (Callers, error) {
	switch Callers(v) {
	case CallersHost, "host":
		return CallersHost, nil
	case CallersNetwork:
		return CallersNetwork, nil
	case CallersAny:
		return CallersAny, nil
	}
	return CallersHost, fmt.Errorf("--callers %q: use host, network or any", v)
}

func (s *Server) callers() Callers {
	if s.cfg.Callers == CallersHost {
		return "host"
	}
	return s.cfg.Callers
}

// peerAllowed reports whether this caller is inside the boundary. Loopback is
// allowed under every policy: it is how a health probe reaches a lender, from
// the host when it is a host process and from `podman exec` when it is not.
func (s *Server) peerAllowed(remote string) bool {
	if s.cfg.Callers == CallersAny {
		return true
	}
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	s.localMu.Lock()
	defer s.localMu.Unlock()
	// Re-read periodically: a laptop changes networks while sandboxes keep
	// running, and a container is attached to a network after it starts.
	if s.localNets == nil || time.Since(s.localAt) > 10*time.Second {
		s.localNets = ownNets()
		s.localAt = time.Now()
	}
	for _, n := range s.localNets {
		if s.cfg.Callers == CallersNetwork {
			if n.Contains(ip) {
				return true
			}
			continue
		}
		if n.IP.Equal(ip) {
			return true
		}
	}
	return false
}

// ownNets is every network this process's interfaces sit on: the address for
// CallersHost, and the whole subnet for CallersNetwork.
//
// A var so a test can say what the interfaces are. The two policies differ only
// in how this list is read, and that difference is the boundary — worth testing
// without a machine that happens to have the right addresses.
var ownNets = func() []*net.IPNet {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	out := make([]*net.IPNet, 0, len(addrs))
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			out = append(out, n)
		}
	}
	return out
}

type errorBody struct {
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
		Source  string `json:"source"`
	} `json:"error"`
}

func writeError(w http.ResponseWriter, status int, kind, msg string) {
	var b errorBody
	b.Error.Type = kind
	b.Error.Message = msg
	b.Error.Source = "cs-sandbox lend"
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(b)
}

// Summary is the line a stopping lender prints, which is what a reader sees
// after a run that did not work.
func (st Stats) Summary() string {
	return fmt.Sprintf("requests %d · lent %d · refused %d · tunnels %d · blocked %d",
		st.Requests, st.Lent, st.Refused, st.Tunnels, st.Blocked)
}

// Origins are the upstream hosts this build fronts, sorted, for reporting and
// for the tunnel refusal list.
func Origins() []string {
	seen := map[string]bool{}
	for _, s := range slots {
		if u, err := url.Parse(s.Origin); err == nil && u.Host != "" {
			seen[u.Host] = true
		}
	}
	out := make([]string, 0, len(seen))
	for h := range seen {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}
