package lend

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
)

// The vendor's own backend: the part of a login's traffic that is neither a
// model call nor something a tunnel should carry.
//
// Codex 0.156 and later will not start on a ChatGPT login until a workspace
// routing discovery succeeds. It asks GET {chatgpt_base_url}/wham/accounts/check
// with the token it holds, and looks for its own account in the answer. A
// loan's token is refused by chatgpt.com, and the tunnel refuses chatgpt.com
// before that (R153), so a lent Codex exited at startup (SBX-084).
//
// So the lender answers that call, and nothing else of that backend (R146a). The
// answer names the loan's own account, the one its seeded token claims, so the
// sandbox still never learns whose subscription it spends. The credential is
// not read and no provider is called: a loan needs nothing from the account
// that the provider would have to be asked for.
//
// The answer names a concrete HTTPS origin, and it has to. For an account with
// no residency rule the real answer is NO_CONSTRAINT, which Codex resolves to
// the origin of chatgpt_base_url itself and then requires to be HTTPS. Here
// that origin is the lender, on plain HTTP. A concrete origin is checked in its
// place, and https://chatgpt.com is what NO_CONSTRAINT resolves to for the
// same account on the host. Codex applies it only to a model provider on the
// backend's own origin, which a lent Codex's is not: see BackendName.
//
// Every other call under the prefix is refused. Codex makes several more there
// (usage, settings, the config bundle). Each went to the tunnel before and was
// refused there, and Codex works without them. Forwarding them would put the
// real credential on paths nobody reviewed.

// backendPrefix is where a client's calls to its vendor's backend arrive. A
// model call never starts with it.
const backendPrefix = "/backend-api"

// discoveryPath is the one backend call a lent Codex needs to succeed.
const discoveryPath = backendPrefix + "/wham/accounts/check"

// discoveredOrigin is the workspace origin the answer names.
const discoveredOrigin = "https://chatgpt.com"

// accountsCheck is the discovery answer, in the fields Codex reads.
type accountsCheck struct {
	Accounts        []accountEntry `json:"accounts"`
	AccountOrdering []string       `json:"account_ordering"`
	DefaultAccount  string         `json:"default_account_id"`
}

type accountEntry struct {
	ID        string `json:"id"`
	Origin    string `json:"workspace_backend_origin"`
	Override  string `json:"account_routing_override"`
	Structure string `json:"structure"`
}

// codexBackend answers Codex's workspace discovery for a loan. It returns nil
// for any other call, which the caller refuses.
func codexBackend(loan Loan, r *http.Request) (any, error) {
	if r.Method != http.MethodGet || r.URL.Path != discoveryPath {
		return nil, nil
	}
	account, err := loanAccount(loan.Token)
	if err != nil {
		return nil, err
	}
	return accountsCheck{
		Accounts: []accountEntry{{
			ID: account, Origin: discoveredOrigin, Override: "NO_CONSTRAINT", Structure: "personal",
		}},
		AccountOrdering: []string{account},
		DefaultAccount:  account,
	}, nil
}

// underBackend reports whether a path is a call to a vendor's backend.
func underBackend(path string) bool {
	return path == backendPrefix || strings.HasPrefix(path, backendPrefix+"/")
}

// serveBackend answers a backend call from the slot's own table, or refuses
// it. Nothing here reads the credential or dials anything.
func (s *Server) serveBackend(w http.ResponseWriter, r *http.Request, loan Loan, slot Slot) {
	attrs := []any{slog.String("sandbox", loan.Name), slog.String("slot", slot.ID), slog.String("path", r.URL.Path)}
	answer, err := slot.backend(loan, r)
	switch {
	case err != nil:
		s.count(func(st *Stats) { st.Refused++ })
		s.cfg.Log.Error("backend call could not be answered", append(attrs, slog.Any("err", err))...)
		writeError(w, http.StatusInternalServerError, "backend_unanswered", err.Error())
	case answer == nil:
		s.count(func(st *Stats) { st.Blocked++ })
		s.cfg.Log.Info("backend call refused", attrs...)
		writeError(w, http.StatusForbidden, "backend_blocked",
			"cs-sandbox answers "+slot.ID+"'s startup call to its vendor's backend for a loan, and lends nothing else of that backend")
	default:
		s.count(func(st *Stats) { st.Answered++ })
		s.cfg.Log.Info("backend call answered", attrs...)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(answer)
	}
}

// loanAccount reads back the account a lent Codex was seeded with. Its loan
// token is the access token forged for it, which names the account in the
// same claim the client reads.
func loanAccount(token string) (string, error) {
	payload, err := jwtPayload(token)
	if err != nil {
		return "", err
	}
	var claims struct {
		Auth struct {
			Account string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", errors.New("the loan token's payload is not JSON")
	}
	if claims.Auth.Account == "" {
		return "", errors.New("the loan token names no ChatGPT account")
	}
	return claims.Auth.Account, nil
}
