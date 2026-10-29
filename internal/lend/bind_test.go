package lend

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// bridge is the network a fabric lender under test sits on, at 10.89.1.54.
func bridge(t *testing.T) {
	t.Helper()
	_, n, _ := net.ParseCIDR("10.89.1.54/24")
	n.IP = net.ParseIP("10.89.1.54")
	restore := ownNets
	ownNets = func() []*net.IPNet { return []*net.IPNet{n} }
	t.Cleanup(func() { ownNets = restore })
}

// members is a group's DNS whose answers the test chooses.
type members struct {
	mu   sync.Mutex
	at   map[string]string
	asks int
}

func (m *members) move(name, ip string) { m.mu.Lock(); m.at[name] = ip; m.mu.Unlock() }

func (m *members) lookup(_ context.Context, name string) ([]net.IP, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.asks++
	if ip, ok := m.at[name]; ok {
		return []net.IP{net.ParseIP(ip)}, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

// boundLender is a fabric lender for group "grp", holding one key loan for its
// member "box". It counts the credential reads, so a refusal can be shown to
// have read nothing.
type boundLender struct {
	*Server
	up    *upstream
	dns   *members
	log   *lockedBuffer
	reads *int
	clock *time.Time
}

func newBoundLender(t *testing.T, callers Callers) boundLender {
	t.Helper()
	bridge(t)
	up := newUpstream(t)
	reads := 0
	withSlots(t, Slot{ID: "openai", Kind: Key, Origin: up.URL, Header: "authorization", Prefix: "Bearer ",
		AuthEnvs: []string{"OPENAI_API_KEY"}, BaseEnv: "OPENAI_BASE_URL",
		read:  func(_, _ string) (string, map[string]string, error) { reads++; return "the-hosts-real-key", nil, nil },
		where: keyPath("openai")})
	dns := &members{at: map[string]string{"box": "10.89.1.7", "peer": "10.89.1.8"}}
	clock := time.Unix(1_700_000_000, 0)
	log := &lockedBuffer{}
	s := New(Config{Home: t.TempDir(), Callers: callers, Group: "grp", Addrs: dns.lookup,
		Now: func() time.Time { return clock },
		Loans: fixedLoans{
			lentToken: {Token: lentToken, Slot: "openai", Kind: Key, Group: "grp", Name: "box"},
			// Another group's member of the same name: its loan resolves to
			// grp's box on this network, which is not the sandbox it was minted for.
			"loan_other": {Token: "loan_other", Slot: "openai", Kind: Key, Group: "elsewhere", Name: "box"},
		},
		Log: slog.New(slog.NewTextHandler(log, nil))})
	return boundLender{Server: s, up: up, dns: dns, log: log, reads: &reads, clock: &clock}
}

// spend presents a loan from an address and returns the status and the
// error type the lender answered with.
func (b boundLender) spend(t *testing.T, token, from string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://cs-lender:2500/v1/responses", strings.NewReader(`{}`))
	req.RemoteAddr = from
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	b.ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Result().Body)
	var eb errorBody
	_ = json.Unmarshal(body, &eb)
	return rec.Code, eb.Error.Type
}

// R152a: a loan copied out of its sandbox is worth nothing to the peer that
// copied it. The refusal comes before the credential is read or the upstream
// is dialled, and the log names the loan and the caller without the token.
func TestALoanIsSpentOnlyFromItsOwnSandbox(t *testing.T) {
	cases := []struct {
		name   string
		token  string
		from   string
		status int
	}{
		{"its own sandbox", lentToken, "10.89.1.7:40000", http.StatusOK},
		{"a peer holding a copy", lentToken, "10.89.1.8:40000", http.StatusForbidden},
		{"an address no member holds", lentToken, "10.89.1.99:40000", http.StatusForbidden},
		{"loopback, inside the lender's own container", lentToken, "127.0.0.1:40000", http.StatusForbidden},
		{"another group's loan, from the member here of the same name", "loan_other", "10.89.1.7:40000", http.StatusForbidden},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := newBoundLender(t, CallersNetwork)
			status, kind := b.spend(t, c.token, c.from)
			if status != c.status {
				t.Fatalf("status %d (%s), want %d", status, kind, c.status)
			}
			if c.status == http.StatusOK {
				if b.up.calls != 1 || *b.reads != 1 {
					t.Errorf("upstream calls %d, credential reads %d: want one of each", b.up.calls, *b.reads)
				}
				return
			}
			if kind != "wrong_sandbox" {
				t.Errorf("error type %q, want wrong_sandbox", kind)
			}
			if b.up.calls != 0 || *b.reads != 0 {
				t.Errorf("a refused loan reached the upstream %d times and read the credential %d times, want 0",
					b.up.calls, *b.reads)
			}
			if got := b.Snapshot(); got.Refused != 1 || got.Lent != 0 {
				t.Errorf("refused %d, lent %d: want 1 and 0", got.Refused, got.Lent)
			}
			log := b.log.String()
			if strings.Contains(log, c.token) {
				t.Errorf("the log carries the token:\n%s", log)
			}
			if !strings.Contains(log, "sandbox=box") || !strings.Contains(log, "remote="+c.from) {
				t.Errorf("the log does not name the loan's sandbox and the caller:\n%s", log)
			}
		})
	}
}

// A container comes back on another address after a restart, and the lender
// finds it there on its first call. The address it left is not the sandbox's
// any more, and stops working once the lender has looked again.
func TestARestartedSandboxIsFoundAtItsNewAddress(t *testing.T) {
	b := newBoundLender(t, CallersNetwork)
	if status, kind := b.spend(t, lentToken, "10.89.1.7:40000"); status != http.StatusOK {
		t.Fatalf("before the restart: %d (%s)", status, kind)
	}
	b.dns.move("box", "10.89.1.12")
	*b.clock = b.clock.Add(5 * time.Second) // a restart is seconds, not milliseconds
	if status, kind := b.spend(t, lentToken, "10.89.1.12:40000"); status != http.StatusOK {
		t.Fatalf("after the restart, from the new address: %d (%s)", status, kind)
	}
	if status, _ := b.spend(t, lentToken, "10.89.1.7:40000"); status != http.StatusForbidden {
		t.Errorf("the address the sandbox left still spends its loan: %d", status)
	}
}

// A peer replaying a stolen token gets one lookup per interval, not one per
// request, and a sandbox that matched is not looked up again on every call.
func TestRefusalsDoNotLookUpPerRequest(t *testing.T) {
	b := newBoundLender(t, CallersNetwork)
	for range 20 {
		b.spend(t, lentToken, "10.89.1.8:40000")
	}
	for range 20 {
		b.spend(t, lentToken, "10.89.1.7:40000")
	}
	if b.dns.asks != 1 {
		t.Errorf("40 calls inside one interval asked the DNS %d times, want 1", b.dns.asks)
	}
}

// A lender on the host sees every sandbox at the host's own address, so there is
// nothing to bind to, and a lender under CallersAny is a test's. Neither refuses
// on this ground.
func TestOnlyAFabricLenderBinds(t *testing.T) {
	b := newBoundLender(t, CallersAny)
	if status, kind := b.spend(t, lentToken, "10.89.1.8:40000"); status != http.StatusOK {
		t.Errorf("a CallersAny lender refused: %d (%s)", status, kind)
	}
}

// The fabric's dnsmasq is <prefix>.53 on the lender's bridge, and only an
// answer on that bridge is a member: aardvark hands the host's resolvers a name
// it does not hold, and what they say about it is somewhere else.
func TestFabricLookupStaysOnTheBridge(t *testing.T) {
	bridge(t)
	nets := bridgeNets()
	if got := fabricDNS(nets); got != "10.89.1.53" {
		t.Errorf("fabricDNS = %q, want 10.89.1.53", got)
	}
	got := onNets([]net.IP{net.ParseIP("192.168.1.5"), net.ParseIP("10.89.1.7")}, nets)
	if len(got) != 1 || !got[0].Equal(net.ParseIP("10.89.1.7")) {
		t.Errorf("onNets kept %v, want only 10.89.1.7", got)
	}
}

// fakeFabricDNS is the fabric's dnsmasq as a lookup of a member's bare name
// meets it: the A record from its hostsdir at once, and the AAAA forwarded to
// aardvark and never answered. It returns the address to dial.
func fakeFabricDNS(t *testing.T, ip string) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			q := buf[:n]
			// The question is a name, then its type and class.
			end := 12
			for end < n && q[end] != 0 {
				end += int(q[end]) + 1
			}
			if end+5 > n || binary.BigEndian.Uint16(q[end+1:end+3]) != 1 {
				continue
			}
			resp := append([]byte{q[0], q[1], 0x81, 0x80, 0, 1, 0, 1, 0, 0, 0, 0}, q[12:end+5]...)
			resp = append(resp, 0xC0, 12, 0, 1, 0, 1, 0, 0, 0, 0, 0, 4)
			resp = append(resp, net.ParseIP(ip).To4()...)
			_, _ = pc.WriteTo(resp, from)
		}
	}()
	return pc.LocalAddr().String()
}

// R152a: a member is found by its A record alone. The fabric's dnsmasq forwards
// the AAAA for a bare name to aardvark, which often never answers, and a lookup
// that asked for both waited out its deadline and returned neither: the member
// was refused its own loan, at random.
func TestAMemberIsFoundWithoutItsAAAA(t *testing.T) {
	bridge(t)
	addr := fakeFabricDNS(t, "10.89.1.7")
	restore := fabricDNSAddr
	fabricDNSAddr = func([]*net.IPNet) string { return addr }
	t.Cleanup(func() { fabricDNSAddr = restore })

	ctx, cancel := context.WithTimeout(context.Background(), bindLookup)
	defer cancel()
	start := time.Now()
	ips, err := fabricAddrs(ctx, "box")
	if len(ips) != 1 || !ips[0].Equal(net.ParseIP("10.89.1.7")) {
		t.Fatalf("fabricAddrs = %v, %v; want the member's 10.89.1.7", ips, err)
	}
	if took := time.Since(start); took > bindLookup/2 {
		t.Errorf("finding the member took %v, which is waiting on an AAAA nobody answers", took)
	}
}
