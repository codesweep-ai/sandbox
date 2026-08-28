package cli

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/codesweep-ai/sandbox/internal/doctor"
	"github.com/codesweep-ai/sandbox/internal/engine"
	"github.com/codesweep-ai/sandbox/internal/lend"
	"github.com/codesweep-ai/sandbox/internal/paths"
	"github.com/codesweep-ai/sandbox/internal/seed"
	"github.com/codesweep-ai/sandbox/internal/state"
)

// Wiring a sandbox to the credential lender.
//
// Everything here happens at create and nowhere else. A loan is a fact written
// beside the instance, true from the moment create returns and gone when the
// instance directory is removed — so there is no registration to sequence, no
// second lifetime to keep in step with the sandbox's, and no command to revoke
// one. Destroying the sandbox is the revocation.

// loanPlan is what the lend flags resolved to: the loans to record, the
// environment the sandbox needs to spend them, and the lines create prints.
type loanPlan struct {
	loans []lend.Loan
	env   []string
	// seeded are the fabricated credential files a lent sandbox holds, in the
	// shape each agent's own sign-in leaves behind.
	seeded []seed.LentCredential
	notes  []string
	// origins are the upstreams the caller named for the slots being lent,
	// by slot id, and consumed the variables those values came out of.
	origins  map[string]string
	consumed []string
}

// resolveLoans validates the credential flags, checks the host holds what they
// name, starts the lender if it is not already up, and mints one token per
// loan.
//
// It fails before anything is provisioned. The alternative is a sandbox that
// comes up looking healthy and reports itself signed out at the first model
// call, which is the failure this whole feature exists to make impossible to
// hit by accident.
func (app *App) resolveLoans(f *createFlags, name, injected string) (*loanPlan, error) {
	plan := &loanPlan{origins: map[string]string{}}
	home := paths.AgentLoginHome(app.Host.Home)
	keysDir := lend.KeysDir(home)

	// Shared keys are copied in rather than lent, so they need no lender and no
	// token — but they are still a named credential rather than a stray
	// variable, which is why they are a flag of their own.
	for _, id := range f.inheritAPIKey {
		slot, err := keySlot(id, "--inherit-api-key")
		if err != nil {
			return nil, err
		}
		key, _, err := slot.Read(home, keysDir)
		if err != nil {
			return nil, fmt.Errorf("--inherit-api-key %s: %w", id, err)
		}
		// Every variable the slot names, because the clients that read this
		// provider disagree about which one to look at.
		for _, v := range slot.AuthEnvs {
			plan.env = append(plan.env, v+"="+key)
		}
		plan.notes = append(plan.notes,
			fmt.Sprintf("api key: %s (copied into the sandbox from %s)", id, slot.Source(home, keysDir)))
	}

	var lent []lend.Slot
	for _, id := range f.lendAgentLogin {
		slot, err := loginSlot(id)
		if err != nil {
			return nil, err
		}
		lent = append(lent, slot)
	}
	for _, id := range f.lendAPIKey {
		slot, err := keySlot(id, "--lend-api-key")
		if err != nil {
			return nil, err
		}
		lent = append(lent, slot)
	}
	if len(lent) == 0 {
		return plan, nil
	}

	// One agent cannot both hold a login and borrow one: the copied credential
	// would win, and the sandbox would be spending it directly while claiming
	// to be lent one.
	for _, a := range f.inheritAgentLogin {
		for _, s := range lent {
			if s.ID == a {
				return nil, fmt.Errorf("--inherit-agent-login %s and --lend-agent-login %s ask for opposite things: "+
					"inheriting copies the credential into the sandbox, lending keeps it on the host", a, a)
			}
		}
	}
	// Two slots that steer the same agent would fight over one base URL, and
	// the loser would be silently ignored.
	seen := map[string]string{}
	for _, s := range lent {
		if other, ok := seen[s.BaseEnv]; ok {
			return nil, fmt.Errorf("%s and %s both drive %s: lend one of them", other, s.ID, s.BaseEnv)
		}
		seen[s.BaseEnv] = s.ID
	}
	for _, id := range f.inheritAPIKey {
		if slot, ok := lend.SlotByID(id); ok {
			if other, ok := seen[slot.BaseEnv]; ok {
				return nil, fmt.Errorf("--inherit-api-key %s copies a credential in while %s is lent: pick one", id, other)
			}
		}
	}

	// What is lent has to exist before the sandbox is built around it.
	for _, s := range lent {
		if err := s.Available(home, keysDir); err != nil {
			return nil, err
		}
	}

	// A base URL the caller set for a slot they are lending says where the
	// lender should forward it: a recorder, or a gateway, in front of the
	// provider. The variable is read here and re-used below for the lender's own
	// address, which is the trade a lent credential already makes. What the host
	// holds is spent on the sandbox's behalf, and the sandbox is handed
	// something that only works through the lender.
	//
	// Read before anything is provisioned, so an address that is not one fails
	// here rather than as a 502 on the sandbox's first model call.
	for _, s := range lent {
		u := envValue(injected, s.BaseEnv)
		if u == "" {
			continue
		}
		if err := checkUpstream(s.BaseEnv, u); err != nil {
			return nil, err
		}
		plan.origins[s.ID] = u
		plan.consumed = append(plan.consumed, s.BaseEnv)
		plan.notes = append(plan.notes,
			fmt.Sprintf("upstream: %s goes to %s (from %s, which the sandbox does not keep)", s.ID, u, s.BaseEnv))
	}

	guestBase, err := app.ensureLender()
	if err != nil {
		return nil, err
	}
	// Where a sandbox sends one slot's model calls. It is a property of the
	// slot rather than of the run, because a cassette prefix names the provider
	// the traffic is for and a sandbox can borrow two credentials at once.
	baseFor := func(lend.Slot) string { return guestBase }
	proxyBase := guestBase
	if f.cassette != "" {
		vcr := f.vcr
		if vcr == "" {
			vcr = fmt.Sprintf("%s:%d", engine.HostReachableName, defaultVCRPort)
		}
		baseFor = func(s lend.Slot) string {
			return "http://" + vcr + "/c/" + vcrProvider(s) + "/" + f.cassette
		}
		proxyBase = "http://" + vcr
		plan.notes = append(plan.notes, cassetteNotes(f.cassette, vcr, lenderLoopback(guestBase), lent)...)
	}

	for _, s := range lent {
		g, err := s.MintGuest(name)
		if err != nil {
			return nil, err
		}
		plan.loans = append(plan.loans, lend.Loan{
			Token: g.Wire, Label: g.Label, Slot: s.ID, Kind: s.Kind, Origin: plan.origins[s.ID],
		})
		if g.File != "" {
			// A login is seeded as the agent's own credential file, so the
			// client stays on the code path it takes when it is signed in. Only
			// the base URL is set, because there is no gateway variable in play.
			plan.seeded = append(plan.seeded, seed.LentCredential{Agent: g.Agent, File: g.File, Doc: g.Doc})
			plan.env = append(plan.env, s.BaseEnv+"="+baseFor(s))
		} else {
			plan.env = append(plan.env, s.Env(g.Wire, baseFor(s))...)
		}
		what := "login"
		if s.Kind == lend.Key {
			what = "api key"
		}
		plan.notes = append(plan.notes, fmt.Sprintf("lent: %s %s (the credential stays on the host, in %s)",
			s.ID, what, s.Source(home, keysDir)))
	}

	// The half of an agent's traffic a base URL does not govern. Without this a
	// sandbox's agent reaches api.anthropic.com on its own, is refused there
	// because it holds no credential that host accepts, and reports itself
	// signed out while its model calls are working.
	if f.blockSideCalls {
		host := strings.TrimPrefix(proxyBase, "http://")
		for _, k := range []string{"HTTPS_PROXY", "https_proxy"} {
			plan.env = append(plan.env, k+"=http://"+host)
		}
		// The host itself, by both the name the sandbox reaches it under and
		// the address that name has where podman runs natively.
		noProxy := "localhost,127.0.0.1," + engine.HostReachableName + "," + engine.HostReachableIP
		for _, k := range []string{"NO_PROXY", "no_proxy"} {
			plan.env = append(plan.env, k+"="+noProxy)
		}
		plan.notes = append(plan.notes, "side calls: blocked to "+strings.Join(lend.Origins(), ", ")+
			" (--block-side-calls=false to allow them)")
	}
	return plan, nil
}

// envValue reads one variable out of an injected env block, or "" when it is
// not there. The block is the same KEY=VALUE lines the seed writes.
func envValue(block, key string) string {
	for line := range strings.SplitSeq(block, "\n") {
		k, v, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(k) == key {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// checkUpstream refuses an address the lender could not forward to, using the
// same parser the request path uses, so what passes here is what forwards.
func checkUpstream(name, raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("--env %s=%q is not a URL: %w", name, raw, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("--env %s=%q needs an http or https scheme and a host, "+
			"as in http://127.0.0.1:8080/c/anthropic/build", name, raw)
	}
	return nil
}

// defaultVCRPort is where cs-vcr listens unless it was told otherwise.
const defaultVCRPort = 8080

// cassetteNotes is the wiring cs-sandbox cannot do for you.
//
// The cassette lives in a cs-vcr's configuration, in another repository, with
// its own rules about unknown keys — so this prints the stanza rather than
// writing it. The values are this run's own, which is what keeps the printed
// line from going stale: the component that knows the address is the one
// printing it.
func cassetteNotes(cassette, vcrAddr, lenderURL string, lent []lend.Slot) []string {
	seen := map[string]bool{}
	var provs []string
	for _, s := range lent {
		p := vcrProvider(s)
		if p != "" && !seen[p] {
			seen[p] = true
			provs = append(provs, p)
		}
	}
	notes := []string{
		fmt.Sprintf("cassette: %s (model calls go to /c/<provider>/%s on the cs-vcr at %s)", cassette, cassette, vcrAddr),
		fmt.Sprintf("          that cs-vcr needs  listen: 0.0.0.0:%s  so a sandbox can reach it, and", portOf(vcrAddr)),
	}
	for _, p := range provs {
		notes = append(notes, fmt.Sprintf("          providers.%s.base_url: %s", p, lenderURL))
	}
	return notes
}

// vcrProvider is the entry a cs-vcr serves this slot's upstream under, which is
// the name the sandbox's base URL carries.
//
// Keyed on the slot rather than on its variable, because a provider with no
// base-URL variable of its own borrows a client's. One vendor can have two
// entries: a lent Codex login is a ChatGPT subscription, spent at the backend
// cs-vcr reaches under `chatgpt`, while an OpenAI key is spent at
// api.openai.com under `openai`.
func vcrProvider(s lend.Slot) string {
	switch s.ID {
	case "claude", "anthropic":
		return "anthropic"
	case "codex":
		return "chatgpt"
	}
	return s.ID
}

// lenderLoopback rewrites the sandbox's view of the lender into the host's own,
// because a cs-vcr forwarding to it runs on the host rather than in a sandbox.
func lenderLoopback(guestBase string) string {
	return "http://127.0.0.1:" + portOf(strings.TrimPrefix(guestBase, "http://"))
}

func portOf(hostport string) string {
	if _, port, err := net.SplitHostPort(hostport); err == nil {
		return port
	}
	return strconv.Itoa(lend.DefaultPort)
}

func loginSlot(id string) (lend.Slot, error) {
	s, ok := lend.SlotByID(id)
	if !ok || s.Kind != lend.Login {
		return lend.Slot{}, fmt.Errorf("--lend-agent-login: unknown agent %q: use one of %s",
			id, strings.Join(lend.SlotIDs(lend.Login), ", "))
	}
	return s, nil
}

func keySlot(id, flag string) (lend.Slot, error) {
	s, ok := lend.SlotByID(id)
	if !ok || s.Kind != lend.Key {
		return lend.Slot{}, fmt.Errorf("%s: unknown provider %q: use one of %s",
			flag, id, strings.Join(lend.SlotIDs(lend.Key), ", "))
	}
	return s, nil
}

// ensureLender returns the base URL a sandbox reaches the lender at, starting
// one if nothing is listening.
//
// Started the way a port forward starts its ssh child: on first use, so a
// sandbox created with a loan works without anyone having been told to run a
// daemon first.
func (app *App) ensureLender() (guestBase string, err error) {
	d := lend.Daemon{Dir: app.InstDir}
	bind := envOr("CS_SANDBOX_LEND_ADDR", lend.DefaultBind)
	if app.dryRun() {
		// A dry run starts nothing. It reports the address a real run would use,
		// so the environment it prints is the environment it would seed.
		fmt.Fprintf(app.stderr(), "+ cs-sandbox lender --addr %s\n", bind)
		return lend.GuestURL(engine.HostReachableName, bind), nil
	}
	if _, recorded, alive := d.Status(); alive && recorded != "" {
		if lend.Probe(lend.ProbeAddr(recorded)) == nil {
			return lend.GuestURL(engine.HostReachableName, recorded), nil
		}
	}
	// A lender this tool did not start still counts: a host that runs one under
	// a service manager should not have a second started underneath it, and the
	// port would refuse the attempt anyway.
	if lend.Probe(lend.ProbeAddr(bind)) == nil {
		return lend.GuestURL(engine.HostReachableName, bind), nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("find this executable to start the lender: %w", err)
	}
	if err := d.Start(exe, []string{"lender", "--addr", bind}, bind, lend.ProbeAddr(bind)); err != nil {
		return "", fmt.Errorf("start the credential lender: %w\n  its log is at %s", err, d.LogPath())
	}
	return lend.GuestURL(engine.HostReachableName, bind), nil
}

// mergeLoanEnv folds the loan variables into the injected block, refusing to
// overwrite one the caller set by hand.
//
// A --env that shadowed a loan would point the agent somewhere else while
// create reported a loan in place, which is the one failure this feature cannot
// afford: the sandbox would look lent-to and be holding nothing.
//
// consumed are the variables the loans took over rather than collided with:
// the base URLs whose values are now the loans' upstreams. They are dropped
// here so the value the caller wrote never reaches the sandbox, and the
// lender's own address takes the name.
func mergeLoanEnv(block string, loanEnv, consumed []string) (string, error) {
	drop := map[string]bool{}
	for _, k := range consumed {
		drop[k] = true
	}
	set := map[string]bool{}
	var kept strings.Builder
	for line := range strings.SplitSeq(block, "\n") {
		k, _, ok := strings.Cut(line, "=")
		if ok && drop[strings.TrimSpace(k)] {
			continue
		}
		if ok {
			set[strings.TrimSpace(k)] = true
		}
		if line != "" {
			kept.WriteString(line)
			kept.WriteByte('\n')
		}
	}
	block = kept.String()

	var b strings.Builder
	b.WriteString(block)
	for _, line := range loanEnv {
		k, _, _ := strings.Cut(line, "=")
		if set[k] {
			return "", fmt.Errorf("--env %s collides with a credential this sandbox is lent: drop it, or drop the lend flag", k)
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String(), nil
}

// stopLenderIfIdle ends the lender once no sandbox on this host holds a loan.
//
// It holds refreshed credentials in memory, so the fewer minutes it exists the
// smaller the window; and a process still running for nobody is one a reader
// has to explain.
func (app *App) stopLenderIfIdle() {
	if app.dryRun() {
		return
	}
	insts, err := state.List(app.InstDir)
	if err != nil {
		return
	}
	for _, in := range insts {
		loans, err := lend.ReadLoans(state.Dir(app.InstDir, in.Group, in.Name))
		if err == nil && len(loans) > 0 {
			return
		}
	}
	_ = lend.Daemon{Dir: app.InstDir}.Stop()
}

// loanSummary is the loans a sandbox holds, for `inspect` and `ls`.
func loanSlots(instDir, group, name string) []string {
	loans, err := lend.ReadLoans(state.Dir(instDir, group, name))
	if err != nil || len(loans) == 0 {
		return nil
	}
	out := make([]string, 0, len(loans))
	for _, l := range loans {
		out = append(out, l.Slot)
	}
	sort.Strings(out)
	return out
}

// lendState walks the lending chain for `doctor`: what is borrowed, whether a
// lender is answering, whether the host can still supply each credential, and
// whether a cs-vcr a sandbox was pointed at is reachable.
//
// It reports rather than repairs. Every hop it checks fails the same way from
// inside a sandbox — the agent says it is not signed in — so naming the dark
// hop is the whole job.
func (app *App) lendState() doctor.LendState {
	var st doctor.LendState
	slots := map[string]bool{}
	insts, _ := state.List(app.InstDir)
	for _, in := range insts {
		loans, err := lend.ReadLoans(state.Dir(app.InstDir, in.Group, in.Name))
		if err != nil || len(loans) == 0 {
			continue
		}
		st.Sandboxes++
		for _, l := range loans {
			slots[l.Slot] = true
		}
		if u := cassetteURL(app.InstDir, in.Group, in.Name); u != "" {
			st.Cassettes = append(st.Cassettes, doctor.CassetteCheck{
				Sandbox: in.Name, URL: u, Err: reachErr(u),
			})
		}
	}

	d := lend.Daemon{Dir: app.InstDir}
	pid, recorded, alive := d.Status()
	switch {
	case alive && recorded != "" && lend.Probe(lend.ProbeAddr(recorded)) == nil:
		st.Addr = recorded
	case pid != 0 || recorded != "":
		st.Recorded = recorded
	default:
		bind := envOr("CS_SANDBOX_LEND_ADDR", lend.DefaultBind)
		if lend.Probe(lend.ProbeAddr(bind)) == nil {
			st.Addr = bind
		}
	}

	home := paths.AgentLoginHome(app.Host.Home)
	keysDir := lend.KeysDir(home)
	for _, id := range append(lend.SlotIDs(lend.Login), lend.SlotIDs(lend.Key)...) {
		if !slots[id] {
			continue
		}
		s, _ := lend.SlotByID(id)
		c := doctor.CredentialCheck{Slot: id, Source: s.Source(home, keysDir)}
		if err := s.Available(home, keysDir); err != nil {
			c.Err = err.Error()
		}
		st.Credentials = append(st.Credentials, c)
	}
	return st
}

// cassetteURL is the cs-vcr endpoint a sandbox was pointed at, read back from
// the environment create seeded. Empty when it goes straight to the lender.
func cassetteURL(instDir, group, name string) string {
	data, err := os.ReadFile(filepath.Join(state.Dir(instDir, group, name), "seed", "inject-env"))
	if err != nil {
		return ""
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok || !strings.HasSuffix(k, "_BASE_URL") || !strings.Contains(v, "/c/") {
			continue
		}
		return v
	}
	return ""
}

// reachErr reports why an endpoint does not answer, or "" when it does. Any
// HTTP reply counts: a cs-vcr answering 404 for a path this check invented is
// still a cs-vcr that is running and reachable.
func reachErr(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return err.Error()
	}
	c := &http.Client{Timeout: 2 * time.Second}
	res, err := c.Get(u.Scheme + "://" + u.Host + "/")
	if err != nil {
		return err.Error()
	}
	res.Body.Close()
	return ""
}
