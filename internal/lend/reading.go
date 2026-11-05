package lend

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// SpentStatus is what a hard budget refuses a call with. Measured on
// 2026-11-04 against this lender, Claude Code 2.1.312, Codex 0.165.0 and
// OpenCode 2.2.5 each end a turn on a 400 at once, with no retry of their own:
// on a fresh session, partway through one, and inside a Claude Code sub-agent.
// A 429 with x-should-retry: false ended each turn at once too, but Codex
// reported only "exceeded retry limit, last status: 429" and dropped the body,
// so its driver could not see the marker and read a throttle. A 402 was retried
// by Codex for 227 s in 2026-09.
const SpentStatus = http.StatusBadRequest

// observe puts the meter on a model call's answer. A 101 is left alone, because
// the reverse proxy needs its body to be the raw connection: nothing in it can
// be read, so the call is estimated at once.
func (s *Server) observe(c call, resp *http.Response) {
	m := s.cfg.Meter
	m.begin(c)
	status := resp.StatusCode
	if status == http.StatusSwitchingProtocols || resp.Body == nil || resp.Body == http.NoBody {
		s.settled(c, reading{}, status, true)
		return
	}
	resp.Body = &meteredBody{ReadCloser: resp.Body, scan: newScan(c.surface),
		settle: func(r reading, complete bool) { s.settled(c, r, status, complete) }}
}

// unanswered charges a call that went out whole and got no answer: the client
// gave up first, or the upstream dropped it. The provider may have spent tokens
// on it, so it is charged as any unread call is (R153e). It has no status.
func (s *Server) unanswered(c call) {
	s.cfg.Meter.begin(c)
	s.settled(c, reading{}, 0, false)
}

// settled charges a call and says so: one line a call, beside the line its
// answer already has (R153a).
func (s *Server) settled(c call, r reading, status int, complete bool) {
	t, estimated, model := s.cfg.Meter.settle(c, r, status, complete)
	s.count(func(st *Stats) {
		st.Metered++
		if estimated {
			st.Estimated++
		}
		if st.Tokens == nil {
			st.Tokens = map[string]Tokens{}
		}
		sum := st.Tokens[c.slot]
		sum.Add(t)
		st.Tokens[c.slot] = sum
	})
	s.cfg.Log.Info("metered",
		slog.String("sandbox", c.loan.Name), slog.String("slot", c.slot), slog.String("model", model),
		slog.Int64("input", t.Of(TypeInput)), slog.Int64("cached", t.Of(TypeCached)),
		slog.Int64("output", t.Of(TypeOutput)), slog.Bool("estimated", estimated))
}

// serveMeter answers the lender's own namespace. Only a meter loan reads it,
// only on MeterPath, and only about its own sandbox, whose address the loan
// was already checked against. A meter loan anywhere else lends nothing.
func (s *Server) serveMeter(w http.ResponseWriter, r *http.Request, loan Loan) {
	switch {
	case loan.Kind != MeterLoan:
		s.count(func(st *Stats) { st.Refused++ })
		writeError(w, http.StatusForbidden, "not_a_meter_loan",
			"the lender's own paths are read with the sandbox's meter loan, and are never forwarded")
		return
	case r.URL.Path != MeterPath || (r.Method != http.MethodGet && r.Method != http.MethodHead):
		s.count(func(st *Stats) { st.Refused++ })
		s.cfg.Log.Warn("refused: a meter loan lends nothing",
			slog.String("sandbox", loan.Name), slog.String("method", r.Method), slog.String("path", r.URL.Path))
		writeError(w, http.StatusForbidden, "meter_loan",
			"this loan reads the sandbox's token count at "+MeterPath+", and lends nothing")
		return
	case s.cfg.Meter == nil:
		writeError(w, http.StatusServiceUnavailable, "no_meter", "this lender keeps no token count")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.cfg.Meter.Read(loan))
}
