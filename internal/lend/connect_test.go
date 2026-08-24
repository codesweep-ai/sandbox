package lend

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// connectTo sends one CONNECT to the lender and returns the status code and,
// when the tunnel opened, the connection to keep talking on.
func connectTo(t *testing.T, proxyAddr, target string) (int, net.Conn) {
	t.Helper()
	c, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("dial the lender: %v", err)
	}
	if _, err := fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	br := bufio.NewReader(c)
	res, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("read the CONNECT reply: %v", err)
	}
	if res.StatusCode != http.StatusOK {
		defer c.Close()
		return res.StatusCode, nil
	}
	return res.StatusCode, c
}

func lenderListener(t *testing.T, home string, loans Loans) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: New(Config{Home: home, KeysDir: KeysDir(home), Loans: loans})}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return l.Addr().String()
}

// The hosts the lender is the front for are exactly the hosts it will not
// tunnel. A side call to one of them is a call that went around the swap.
func TestConnectRefusesTheHostsTheLenderFronts(t *testing.T) {
	addr := lenderListener(t, hostProfile(t), fixedLoans{})
	for _, host := range []string{
		"api.anthropic.com:443",
		"chatgpt.com:443",
		"api.openai.com:443",
		"ab.chatgpt.com:443",
		"API.Anthropic.com.:443", // the same host, spelled to dodge a naive check
	} {
		code, conn := connectTo(t, addr, host)
		if conn != nil {
			conn.Close()
		}
		if code != http.StatusForbidden {
			t.Errorf("CONNECT %s = %d, want 403", host, code)
		}
	}
}

// Everything else is tunnelled, because an agent's tools share its environment
// and none of them have anything to do with the credential.
func TestConnectTunnelsEverythingElse(t *testing.T) {
	// A stand-in for "some host the agent's tools reach".
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		c, err := echo.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = io.Copy(c, c)
	}()

	addr := lenderListener(t, hostProfile(t), fixedLoans{})
	code, conn := connectTo(t, addr, echo.Addr().String())
	if code != http.StatusOK {
		t.Fatalf("CONNECT to an unlisted host = %d, want 200", code)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, "ping\n"); err != nil {
		t.Fatal(err)
	}
	got, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read back through the tunnel: %v", err)
	}
	if strings.TrimSpace(got) != "ping" {
		t.Errorf("through the tunnel: %q, want %q", got, "ping")
	}
}

// The port is non-loopback so a sandbox can reach it, which puts it on the
// host's network. The peer check is what keeps that from mattering.
func TestOnlyThisHostIsServed(t *testing.T) {
	home := hostProfile(t)
	s := New(Config{Home: home, KeysDir: KeysDir(home), Loans: fixedLoans{}, LocalOnly: true})

	for _, c := range []struct {
		remote string
		want   int
	}{
		{"192.0.2.55:41000", http.StatusForbidden}, // a machine on the network
		{"127.0.0.1:41000", http.StatusUnauthorized},
	} {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", http.NoBody)
		req.RemoteAddr = c.remote
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("a request from %s = %d, want %d", c.remote, rec.Code, c.want)
		}
	}
	if got := s.Snapshot().NotLocal; got != 1 {
		t.Errorf("not_local counter = %d, want 1", got)
	}
}

// doctor and create ask a port whether a lender is behind it, and no loan
// exists at the moment they ask.
func TestHealthzNeedsNoLoan(t *testing.T) {
	srv := newTestServer(t, hostProfile(t), fixedLoans{})
	res, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("/healthz = %d, want 200", res.StatusCode)
	}
	var doc map[string]any
	if err := json.NewDecoder(res.Body).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	if doc["service"] != "cs-sandbox lend" {
		t.Errorf("/healthz said %v, want it to identify the lender", doc)
	}
}
