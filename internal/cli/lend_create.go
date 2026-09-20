package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/codesweep-ai/sandbox/internal/doctor"
	"github.com/codesweep-ai/sandbox/internal/engine"
	"github.com/codesweep-ai/sandbox/internal/lend"
	"github.com/codesweep-ai/sandbox/internal/lock"
	"github.com/codesweep-ai/sandbox/internal/paths"
	"github.com/codesweep-ai/sandbox/internal/renew"
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
func (app *App) resolveLoans(ctx context.Context, f *createFlags, name, injected string) (*loanPlan, error) {
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

	// Renewed before it is lent, when it needs renewing. Done ahead of the
	// availability check below, because an expired login is the case that check
	// refuses and the case this fixes — and the renewer cannot help here: it only
	// starts once the loan exists.
	notes, err := app.freshenLentLogins(ctx, lent, home, keysDir)
	if err != nil {
		return nil, err
	}
	plan.notes = append(plan.notes, notes...)

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
		u, moved := lenderUpstream(u)
		plan.origins[s.ID] = u
		plan.consumed = append(plan.consumed, s.BaseEnv)
		plan.notes = append(plan.notes,
			fmt.Sprintf("upstream: %s goes to %s (from %s, replaced by the lender URL inside the sandbox)", s.ID, u, s.BaseEnv))
		if moved != "" {
			plan.notes = append(plan.notes, moved)
		}
	}

	guestBase, err := app.ensureLender(ctx, f.group)
	if err != nil {
		return nil, err
	}
	// The same question Available asked above, asked again in the only frame of
	// reference that decides it.
	//
	// Available reads the credential from HERE, and here is the host. The lender
	// reads it from inside a container that mounts the agent home and nothing
	// else, so the two answers can differ — and when they do, nothing says so:
	// the lender reports the missing file once per request, from a container log
	// nobody opens, while the sandbox above it waits for a model turn that can
	// never arrive. Measured: a campaign sat twelve minutes with no request
	// reaching its recorder, and the only symptom was an agent that never
	// answered.
	//
	// Here, because create is where the operator still is, where the remedy is
	// one command, and where nothing has been provisioned that would have to be
	// torn down.
	if !app.dryRun() {
		box := app.lenderBox(f.group)
		for _, s := range lent {
			src := s.Source(home, keysDir)
			if err := box.canRead(ctx, src); err != nil {
				if !errors.Is(err, errUnreadable) {
					// The question could not be asked, which says nothing about
					// the credential. Reported as itself rather than as a
					// missing file, because the remedies share no word.
					return nil, fmt.Errorf(
						"the lender could not be asked whether it can read the %s credential: %w", s.ID, err)
				}
				return nil, fmt.Errorf(
					"the lender cannot read the %s credential this create would lend: %s\n"+
						"  it reads that path from inside a container, which mounts %s and nothing else, "+
						"so a symlink pointing out of that tree does not resolve there\n"+
						"  hold the file itself under that tree, or point CS_SANDBOX_AGENT_HOME at one that does",
					s.ID, src, home)
			}
		}
	}
	for _, s := range lent {
		g, err := s.MintGuest(name, home)
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
			// Anything the client keeps outside that file and still needs to
			// name the account, so the sandbox can say whose subscription it is
			// spending rather than reporting a signed-out-looking login.
			for _, e := range g.Extra {
				plan.seeded = append(plan.seeded, seed.LentCredential{Agent: g.Agent, File: e.File, Doc: e.Doc})
			}
			plan.env = append(plan.env, s.BaseEnv+"="+guestBase)
		} else {
			plan.env = append(plan.env, s.Env(g.Wire, guestBase)...)
		}
		what := "login"
		if s.Kind == lend.Key {
			what = "api key"
		}
		plan.notes = append(plan.notes, fmt.Sprintf("lent: %s %s (the credential stays on the host, in %s)",
			s.ID, what, s.Source(home, keysDir)))
		plan.notes = append(plan.notes, credentialClockNotes(s, home, keysDir)...)
	}

	// The half of an agent's traffic a base URL does not govern. Without this a
	// sandbox's agent reaches api.anthropic.com on its own, is refused there
	// because it holds no credential that host accepts, and reports itself
	// signed out while its model calls are working.
	if f.blockSideCalls {
		host := strings.TrimPrefix(guestBase, "http://")
		for _, k := range []string{"HTTPS_PROXY", "https_proxy"} {
			plan.env = append(plan.env, k+"=http://"+host)
		}
		// The host itself, by both the name the sandbox reaches it under and
		// the address that name has where podman runs natively.
		noProxy := "localhost,127.0.0.1," + engine.HostReachableName + "," + engine.HostReachableIP
		for _, k := range []string{"NO_PROXY", "no_proxy"} {
			plan.env = append(plan.env, k+"="+noProxy)
		}
		plan.notes = append(plan.notes, "side calls: blocked to "+strings.Join(lend.BlockedHosts(), ", ")+
			" (--block-side-calls=false to allow them)")
	}
	return plan, nil
}

// credentialClockNotes says what is known about a lent login's two clocks, at
// the moment somebody is still standing here to read it.
//
// This is the cheapest place in the whole design to turn a mid-run failure into a
// decision. A credential with nine minutes left will be renewed by the renewer and
// is genuinely fine; the same credential two days from the end of its refresh
// chain is not, and nothing later in the run will be a better time to find out.
func credentialClockNotes(s lend.Slot, home, keysDir string) []string {
	var out []string
	if exp, ok, err := s.ExpiresAt(home, keysDir); err == nil && ok {
		left := time.Until(exp)
		switch {
		case !s.Renewable():
			out = append(out, fmt.Sprintf("%s: expires in %s and nothing renews it", s.ID, doctor.ShortDur(left)))
		case left < time.Hour:
			// Named specifically because it is the one case where a person
			// might reasonably wait a minute and start again on a fresh token
			// rather than trust the handover.
			out = append(out, fmt.Sprintf("%s: expires in %s; the renewer renews it in place, "+
				"so a run crossing that point keeps working", s.ID, doctor.ShortDur(left)))
		}
	}
	if dl, ok, err := s.RefreshDeadline(home, keysDir); err == nil && ok {
		if left := time.Until(dl); left < 7*24*time.Hour {
			out = append(out, fmt.Sprintf("%s: renewing stops working in %s (%s) — refreshing does not extend it, "+
				"so sign in on the host again before then",
				s.ID, doctor.ShortDur(left), dl.Local().Format("Mon 2 Jan 15:04")))
		}
	}
	return out
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
			"as in http://host.containers.internal:8080/c/anthropic/build", name, raw)
	}
	return nil
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

// ensureLender returns the base URL a sandbox in this group reaches the lender
// at, starting the group's lender container if one is not already serving it.
//
// One lender per GROUP, where there used to be one per host. That is the whole
// of the isolation: the container is attached to this group's network and no
// other, so a sandbox in another group has no interface to reach it on and no
// forwarding path to get there — where a host process on a fixed port was
// reachable by everything local, including another user's `create`, which would
// adopt it and report a loan it would never honour.
//
// Started on first use, the way a port forward starts its ssh child, so a
// sandbox created with a loan works without anyone having been told to run a
// daemon first.
func (app *App) ensureLender(ctx context.Context, group string) (guestBase string, err error) {
	b := app.lenderBox(group)
	if app.dryRun() {
		// A dry run starts nothing. It prints the command a real run would use,
		// so the environment it reports is the environment it would seed.
		fmt.Fprintf(app.stderr(), "+ %s\n", strings.Join(lenderBoxArgv(b.name(), b.Spec), " "))
		return b.guestBase(), nil
	}
	return b.ensure(ctx)
}

// lenderBox is this group's lender container, described.
func (app *App) lenderBox(group string) lenderBox {
	home := paths.AgentLoginHome(app.Host.Home)
	return lenderBox{Runner: app.Runner, Spec: lenderBoxSpec{
		Network: state.NetworkName(group),
		Image:   app.Image,
		Home:    home,
		InstDir: app.InstDir,
		Bin:     lenderBinary(),
		Stage:   filepath.Join(state.GroupDir(app.InstDir, group), ".lender", "cs-sandbox"),
		LogDir:  paths.LenderLogs(app.InstDir, group),
	}}
}

// lenderBinary is the cs-sandbox the lender container runs, or "" to let the
// image supply it.
//
// The image's own unless CS_SANDBOX_LENDER_BIN names another. A cs-sandbox
// names an image built from its own revision, and that image ships the
// cs-sandbox that made it, so the lender inside already matches the create
// that writes its loans. Nothing needs copying in (SBX-041).
//
// The variable is for the two cases where that does not hold. The slim image
// the test tiers boot carries no cs-sandbox at all, and a change to the lender
// reaches a sandbox without an image rebuild only this way. The binary named
// must be a Linux one for the image's architecture, and create stages a copy
// of it (see lenderBoxSpec.Stage).
func lenderBinary() string {
	return os.Getenv("CS_SANDBOX_LENDER_BIN")
}

// lenderUpstream retargets an upstream that names the loopback at the host.
//
// `--env ANTHROPIC_BASE_URL=http://127.0.0.1:8080/c/anthropic/build` is the
// documented spelling for putting a recorder in front of a provider, and it
// meant "this host" for as long as the lender was a host process. It is a
// container now, where 127.0.0.1 is the container itself and nothing answers
// there. The host is still reachable from inside it, by the name podman
// publishes for it (R52a), so the address is moved onto that name.
//
// REPORTED rather than done quietly. The upstream is where a real credential
// goes; moving one silently is the single thing this must never do, and a
// caller who meant a service inside the fabric needs to see that their spelling
// was read as "the host".
func lenderUpstream(raw string) (string, string) {
	u, err := url.Parse(raw)
	if err != nil {
		return raw, ""
	}
	h, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		h, port = u.Host, ""
	}
	ip := net.ParseIP(h)
	if ip == nil || !ip.IsLoopback() {
		return raw, ""
	}
	u.Host = engine.HostReachableName
	if port != "" {
		u.Host = net.JoinHostPort(engine.HostReachableName, port)
	}
	return u.String(), fmt.Sprintf(
		"upstream: %s is the lender's own loopback now that it runs on the group's network — reading it as %s, "+
			"the name a container reaches this host by", raw, u.Host)
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

// lenderUse is the declaration that this group's lender is being relied on by
// something no loan record names yet.
//
// A loan is what keeps a lender alive, and a create writes one only once its
// sandbox exists — several seconds after it started the lender, checked the
// credential through it, and minted the token. For that whole stretch the group
// holds no loan at all, so a destroy running beside it reads the group as idle
// and removes a lender that is about to be, or already is, in use.
//
// Measured, on the replay matrix, where cells create and destroy in parallel in
// one group: the create fails outright when the container goes between the
// ensure and the readability check ("no container with ID … in database"), and
// fails far worse when it goes just after — create succeeds, the loan lands, and
// nothing ever starts a lender again, so the agent spends its whole turn
// dialling a name that resolves to nothing. Both shapes were flakes in the tier
// that runs on every push.
//
// Shared, and held by every create that lends, because creates do not race each
// other here: they are all saying the same true thing. Exclusive only on the
// side that would take the lender away.
func (app *App) lenderUse(group string) *lock.Lock {
	return lock.NewAt(filepath.Join(state.GroupDir(app.InstDir, group), ".lender", "in-use.lock"))
}

// stopLenderIfIdle ends the lender once no sandbox on this host holds a loan.
//
// It holds refreshed credentials in memory, so the fewer minutes it exists the
// smaller the window; and a process still running for nobody is one a reader
// has to explain.
//
// "Idle" is read under lenderUse, exclusively, so that a create between its own
// ensure and its own loan record counts as a user of the lender rather than as
// nothing. Try rather than wait: a create holds that lock for as long as it
// takes to build a sandbox, and a destroy that queued behind one would hold up
// the command a person is watching. A skipped stop is not a leak — the next
// destroy in the group takes it, and tearing the group down removes the lender
// with the network either way.
func (app *App) stopLenderIfIdle(ctx context.Context, group string) {
	if app.dryRun() {
		return
	}
	// Deferred, and on every path out of here including the early ones: the
	// renewer's question is not this function's question. A lender is per group,
	// so a loan in another group says nothing about this one — but the renewer is
	// per host, and a loan anywhere is a reason for it to stay up.
	defer app.stopRenewerIfIdle()
	use := app.lenderUse(group)
	if held, err := use.TryAcquire(); err != nil || !held {
		return
	}
	defer use.Release()
	insts, err := state.List(app.InstDir)
	if err != nil {
		return
	}
	for _, in := range insts {
		// This group's lender serves this group's sandboxes and nothing else
		// can reach it, so what keeps it up is a loan in HERE. A loan in another
		// group holds that group's lender, and says nothing about this one.
		if in.Group != group {
			continue
		}
		loans, err := lend.ReadLoans(state.Dir(app.InstDir, in.Group, in.Name))
		if err == nil && len(loans) > 0 {
			return
		}
	}
	_ = app.lenderBox(group).stop(ctx)
}

// freshenLentLogins renews any lent login that has expired or is close enough to
// expiring that its own client will refresh it, before the sandbox is built
// around it.
//
// Two cases, one reason. An already-expired login would otherwise fail the create
// outright and send somebody off to type a command the tool could have run
// itself. A login inside its refresh window would survive the create and then die
// minutes later, leaving the sandbox to start with ninety seconds of credential
// instead of eight hours.
//
// Anything with more life than that is left alone, which is the important half:
// its client would decline to refresh, so a attempt there spends a real turn on the
// subscription and changes nothing.
func (app *App) freshenLentLogins(ctx context.Context, lent []lend.Slot, home, keysDir string) ([]string, error) {
	// Asked once for every slot, because it is a question about the image and
	// there is one image. Said even when the credentials are healthy: that is
	// exactly when it is easy to miss, since the sandbox works now and stops at
	// expiry for a reason that has nothing to do with the login. A warning and not
	// a refusal — what is lent is good until then.
	if !app.dryRun() {
		blocked := renew.Possible(ctx, app.renewerConfig(), lent)
		for _, id := range sortedSlotIDs(blocked) {
			fmt.Fprintf(app.stderr(), "cs-sandbox: warning: %v\n", blocked[id])
		}
	}

	var notes []string
	for _, s := range lent {
		if !s.Renewable() {
			continue
		}
		if app.dryRun() {
			// A dry run must not spend a turn, and must not refuse either: what
			// it reports is what a real run would do, and a real run would renew
			// this rather than stop at it.
			if exp, ok, err := s.ExpiresAt(home, keysDir); err == nil && ok && time.Until(exp) <= s.RenewWithin() {
				notes = append(notes, fmt.Sprintf("%s: login expires in %s, so a real run would renew it first by running its own client",
					s.ID, doctor.ShortDur(time.Until(exp))))
			}
			continue
		}
		got, err := renew.Now(ctx, app.renewerConfig(), s)
		if err != nil {
			return nil, err
		}
		switch got.Outcome {
		case renew.Renewed:
			what := "was close to expiring"
			if got.WasExpired {
				what = "had already expired"
			}
			notes = append(notes, fmt.Sprintf("%s: the host login %s, so it was renewed before being lent (now expires in %s)",
				s.ID, what, doctor.ShortDur(time.Until(got.Expires))))
		case renew.AlreadyRenewed:
			notes = append(notes, fmt.Sprintf("%s: the host login was renewed by something else just now (expires in %s)",
				s.ID, doctor.ShortDur(time.Until(got.Expires))))
		}
	}
	return notes, nil
}

// renewerConfig is the renewer's own configuration, as this host resolves it. Built
// in one place because create's one-shot renewal and the renewer process itself
// have to agree about every path — above all the host-global lock directory, which
// is what stops the two of them running a client at the same moment.
func (app *App) renewerConfig() renew.Config {
	home := paths.AgentLoginHome(app.Host.Home)
	return renew.Config{
		Home:     home,
		KeysDir:  lend.KeysDir(home),
		InstDir:  app.InstDir,
		Dir:      paths.Renewer(app.InstDir),
		StateDir: paths.RenewerState(),
		// A renewal runs the client out of this image rather than one on the
		// host's PATH — see internal/renew/container.go. ImageErr travels with it
		// so a host without an image says why rather than saying nothing.
		Image:    app.Image,
		ImageErr: imageErrText(app.ImageErr),
	}
}

// imageErrText is why this host has no sandbox image, or "" when it has one.
func imageErrText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// renewerUse is the declaration that the credential renewer is being relied on by
// something no loan record names yet.
//
// The same shape as lenderUse and for the same reason, one scope wider. A create
// that lends starts the renewer seconds before its loan reaches disk, and a destroy
// running beside it reads the host as idle in that window and stops the renewer a
// command in flight is about to depend on. Shared, because creates do not race each
// other here — they are all saying the same true thing — and exclusive only on the
// side that would take the renewer away.
//
// Host-global, where lenderUse is per group, because the renewer is: one credential,
// one renewer, one lock.
func (app *App) renewerUse() *lock.Lock {
	return lock.NewAt(filepath.Join(paths.RenewerState(), "in-use.lock"))
}

// ensureRenewer starts the host-side credential renewer, if any of the loans just
// recorded names a login something can renew.
//
// Best effort, and reported when it fails. A sandbox whose renewer did not start
// is still a working sandbox — it will simply lose its borrowed credential when
// that credential expires, hours later and far from here. So this must not fail
// the create, and it must not be quiet either: a renewer that is not running is
// indistinguishable from one that is, right up to the moment a long run dies.
func (app *App) ensureRenewer(loans []lend.Loan) {
	if app.dryRun() {
		return
	}
	want := false
	for _, ln := range loans {
		if s, ok := lend.SlotByID(ln.Slot); ok && s.Renewable() {
			want = true
			break
		}
	}
	if !want {
		return
	}
	bin, err := os.Executable()
	if err != nil {
		fmt.Fprintf(app.stderr(), "cs-sandbox: warning: cannot find this binary to start the credential renewer: %v\n"+
			"  the lent login will not be renewed; run 'cs-sandbox renewer' yourself to keep it alive\n", err)
		return
	}
	if err := renew.Start(paths.Renewer(app.InstDir), bin); err != nil {
		fmt.Fprintf(app.stderr(), "cs-sandbox: warning: could not start the credential renewer: %v\n"+
			"  the sandbox works, but a lent login will go stale when it expires; 'cs-sandbox doctor' reports this\n", err)
	}
}

// stopRenewerIfIdle ends the renewer once nothing under this instances root
// borrows a renewable login.
//
// Host-wide by group, unlike the lender: the renewer exists for the credential,
// and one credential serves every group. Cheap enough to run on every destroy,
// because it reads the same loan records the lender's own idle check reads.
func (app *App) stopRenewerIfIdle() {
	if app.dryRun() {
		return
	}
	dir := paths.Renewer(app.InstDir)
	if !renew.Running(dir) {
		return
	}
	// "Idle" is read under renewerUse, exclusively, so that a create between
	// starting the renewer and recording its loan counts as a user of it rather
	// than as nothing. Try rather than wait: a destroy that queued behind a create
	// would hold up a command somebody is watching, and a skipped stop is not a
	// leak — the next destroy takes it.
	use := app.renewerUse()
	if held, err := use.TryAcquire(); err != nil || !held {
		return
	}
	defer use.Release()
	insts, err := state.List(app.InstDir)
	if err != nil {
		return // cannot prove it is idle, so leave it running
	}
	for _, in := range insts {
		loans, err := lend.ReadLoans(state.Dir(app.InstDir, in.Group, in.Name))
		if err != nil {
			continue
		}
		for _, ln := range loans {
			if s, ok := lend.SlotByID(ln.Slot); ok && s.Renewable() {
				return
			}
		}
	}
	renew.Stop(dir, paths.RenewerState())
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
func (app *App) lendState(ctx context.Context) doctor.LendState {
	var st doctor.LendState
	slots := map[string]bool{}
	groups := map[string]bool{}
	insts, _ := state.List(app.InstDir)
	for _, in := range insts {
		loans, err := lend.ReadLoans(state.Dir(app.InstDir, in.Group, in.Name))
		if err != nil || len(loans) == 0 {
			continue
		}
		st.Sandboxes++
		groups[in.Group] = true
		for _, l := range loans {
			slots[l.Slot] = true
		}
		// The upstream a loan was created with: a recorder or a gateway the
		// lender forwards to. As dark a hop as any, and asked of the lender —
		// which is the only party that dials it.
		for _, l := range loans {
			if l.Origin == "" {
				continue
			}
			st.Upstreams = append(st.Upstreams, doctor.UpstreamCheck{
				Sandbox: in.Name, URL: l.Origin, Slot: l.Slot,
				Err: app.lenderBox(in.Group).reach(ctx, l.Origin),
			})
		}
	}
	for _, g := range sortedKeys(groups) {
		b := app.lenderBox(g)
		c := doctor.LenderCheck{Group: g, Where: b.guestBase()}
		if !b.running(ctx) {
			c.Err = "the group's lender container is not running"
		} else if why := b.probe(ctx, "http://"+lend.ProbeAddr(lend.DefaultBind)+"/healthz"); why != "" {
			c.Err = why
		}
		st.Lenders = append(st.Lenders, c)
	}

	home := paths.AgentLoginHome(app.Host.Home)
	keysDir := lend.KeysDir(home)

	// What the renewer last managed to do, read from the file it publishes rather
	// than asked of the process. A renewer that is running but no longer reporting
	// is wedged, and that is the one state it cannot describe for itself — so it
	// is measured from out here, by how old its last report is.
	// One probe for every lent slot, for the same reason create does it once:
	// the question is about the image, and there is one image.
	var lentSlots []lend.Slot
	for _, id := range sortedKeys(slots) {
		if sl, ok := lend.SlotByID(id); ok {
			lentSlots = append(lentSlots, sl)
		}
	}
	renewable := renew.Possible(ctx, app.renewerConfig(), lentSlots)

	kdir := paths.Renewer(app.InstDir)
	renewerSlots := map[string]renew.SlotStatus{}
	kc := doctor.RenewerCheck{Running: renew.Running(kdir)}
	for _, id := range slotIDsSorted(slots) {
		if sl, ok := lend.SlotByID(id); ok && sl.Renewable() {
			kc.Wanted = true
		}
	}
	if kst, ok, err := renew.ReadStatus(kdir); err != nil {
		kc.Err = err.Error()
	} else if ok {
		kc.Reported = true
		kc.LastReport = time.Since(kst.Updated)
		for _, ss := range kst.Slots {
			renewerSlots[ss.Slot] = ss
		}
	}
	st.Renewer = kc

	for _, id := range append(lend.SlotIDs(lend.Login), lend.SlotIDs(lend.Key)...) {
		if !slots[id] {
			continue
		}
		s, _ := lend.SlotByID(id)
		c := doctor.CredentialCheck{Slot: id, Source: s.Source(home, keysDir)}
		if err := s.Available(home, keysDir); err != nil {
			c.Err = err.Error()
		}
		// Both clocks, when the slot has them. "Lendable" and "lendable for
		// another nine minutes" are different answers to the question somebody
		// about to start a long unattended run is actually asking, and the
		// second one turns a failure four hours from now into a decision made
		// here.
		if exp, ok, err := s.ExpiresAt(home, keysDir); err == nil && ok {
			c.Expires = exp
		}
		if dl, ok, err := s.RefreshDeadline(home, keysDir); err == nil && ok {
			c.RefreshDeadline = dl
		}
		if ks, ok := renewerSlots[id]; ok {
			c.RenewErr = ks.Err
		}
		// Reported through the same field, because it is the same fact from the
		// reader's side: this login is not going to be renewed. The message says
		// which of the two reasons it is.
		if c.RenewErr == "" {
			if err, ok := renewable[id]; ok {
				c.RenewErr = err.Error()
			}
		}
		st.Credentials = append(st.Credentials, c)
	}
	return st
}

// slotIDsSorted is the lent slot ids in a stable order.
func slotIDsSorted(slots map[string]bool) []string { return sortedKeys(slots) }

// sortedSlotIDs keeps a per-slot error map's reporting order stable, so two runs
// of the same command print the same lines in the same order.
func sortedSlotIDs(m map[string]error) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sortedKeys keeps doctor's output stable across runs, which is what makes two
// reports comparable.
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
