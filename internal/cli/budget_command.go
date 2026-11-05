package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/codesweep-ai/sandbox/internal/lend"
	"github.com/codesweep-ai/sandbox/internal/lock"
	"github.com/codesweep-ai/sandbox/internal/state"
	"github.com/spf13/cobra"
)

// Token budgets, from the host's side.
//
// A budget is a file the CLI writes beside a sandbox's loans, and the lender
// reads it on the next call (R147c). What the sandbox has spent is the
// lender's, in its group's usage directory, and the CLI only reads it: so
// `usage` answers for a stopped group too, and asks the lender nothing. The
// lender writes that record, so it is read with a size cap.

// parseTokenBudget reads --token-budget's value: TYPE=N pairs, comma-separated,
// with decimal K, M or G suffixes. A bare number is refused, because it would be
// one total, and cached input is nearly all of one.
func parseTokenBudget(flag, value string) (map[string]int64, error) {
	out := map[string]int64{}
	for part := range strings.SplitSeq(value, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		typ, num, ok := strings.Cut(part, "=")
		typ = strings.TrimSpace(typ)
		if !ok {
			return nil, fmt.Errorf("%s %q: name the token type each limit is for, as in input=20M,cached=1G,output=5M; "+
				"one total would be nearly all cached input, and say little about the rest", flag, part)
		}
		if !slices.Contains(lend.Types, typ) {
			return nil, fmt.Errorf("%s %q: unknown token type %q: use %s", flag, part, typ, strings.Join(lend.Types, ", "))
		}
		if _, dup := out[typ]; dup {
			return nil, fmt.Errorf("%s: %s is given twice", flag, typ)
		}
		n, err := parseTokenCount(num)
		if err != nil {
			return nil, fmt.Errorf("%s %q: %w", flag, part, err)
		}
		out[typ] = n
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: name at least one of %s, as in cached=1G", flag, strings.Join(lend.Types, ", "))
	}
	return out, nil
}

// parseTokenCount reads a count such as 20M or 1.5G. The suffixes are decimal:
// 1G is 1,000,000,000.
func parseTokenCount(s string) (int64, error) {
	s = strings.TrimSpace(s)
	mult := 1.0
	if s != "" {
		switch s[len(s)-1] {
		case 'k', 'K':
			mult = 1e3
		case 'm', 'M':
			mult = 1e6
		case 'g', 'G':
			mult = 1e9
		}
		if mult != 1 {
			s = s[:len(s)-1]
		}
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 || math.IsInf(f, 0) || math.IsNaN(f) || f*mult > math.MaxInt64/2 {
		return 0, errors.New("a limit is a number of tokens, such as 500K, 20M or 1G")
	}
	return int64(math.Round(f * mult)), nil
}

// parseAskAt reads --ask-at: a share of each limit, as 90% or 0.9. It is a
// hard budget's, and stays below the limit, because the agent's handoff has to
// fit under the ceiling the lender refuses at.
func parseAskAt(s string) (float64, error) {
	t := strings.TrimSpace(s)
	pct := strings.HasSuffix(t, "%")
	f, err := strconv.ParseFloat(strings.TrimSuffix(t, "%"), 64)
	if pct {
		f /= 100
	}
	if err != nil || f <= 0 || f >= 1 {
		return 0, fmt.Errorf("--ask-at %q: a share of each limit, above 0 and below 100%%, such as 90%%: "+
			"the agent's handoff has to fit under the limit", s)
	}
	return f, nil
}

// limitText writes a limit the way it was most likely typed: 20M rather than
// 20.00M.
func limitText(n int64) string {
	for _, u := range []struct {
		div    int64
		suffix string
	}{{1e9, "G"}, {1e6, "M"}, {1e3, "K"}} {
		if n >= u.div && n%u.div == 0 {
			return strconv.FormatInt(n/u.div, 10) + u.suffix
		}
	}
	return lend.FormatTokens(n)
}

// budgetMode says how a budget stops a sandbox, in a few words.
func budgetMode(b lend.Budget) string {
	if b.Hard {
		share := b.AskAt
		if share <= 0 {
			share = lend.DefaultAskAt
		}
		return "hard, asks at " + pctText(share)
	}
	return "soft"
}

func pctText(f float64) string {
	return strconv.FormatFloat(math.Round(f*1000)/10, 'f', -1, 64) + "%"
}

// budgetText is a budget's limits on one line, in report order.
func budgetText(b lend.Budget) string {
	var parts []string
	for _, typ := range lend.Types {
		if n, ok := b.Limits[typ]; ok {
			parts = append(parts, typ+" "+limitText(n))
		}
	}
	return strings.Join(parts, ", ")
}

// sandboxCount is what is known of one sandbox's tokens on the host's side: its
// budget, and the lender's record of what it has spent.
type sandboxCount struct {
	Sandbox string      `json:"sandbox"`
	Group   string      `json:"group"`
	Budget  lend.Budget `json:"budget"`
	Usage   lend.Usage  `json:"usage"`
	// Metered is false when the lender has written no record for this
	// creation of the sandbox yet.
	Metered bool `json:"metered"`
	// Applied says the lender has read the budget as it stands now. A budget it
	// has not read is reported as not yet applied, rather than as a figure
	// that looks fine.
	Applied bool `json:"applied"`
	// Readable is false for a sandbox created before token budgets: it holds no
	// meter loan, so nothing inside it can read its count, and a soft budget
	// asks no agent to stop.
	Readable bool   `json:"readable"`
	meter    string // the meter id of this creation
}

// countOf reads a sandbox's budget and usage. A sandbox that borrows nothing
// has neither, and says so.
func (app *App) countOf(in *state.Instance) (sandboxCount, error) {
	dir := state.Dir(app.InstDir, in.Group, in.Name)
	loans, err := lend.ReadLoans(dir)
	if err != nil {
		return sandboxCount{}, err
	}
	if len(loans) == 0 {
		return sandboxCount{}, fmt.Errorf("%s borrows nothing, so no lender meters it: a token budget applies to a sandbox created with --lend-agent-login or --lend-api-key", in.Name)
	}
	c := sandboxCount{Sandbox: in.Name, Group: in.Group, meter: lend.MeterID(loans)}
	for _, l := range loans {
		if l.Kind == lend.MeterLoan {
			c.Readable = true
		}
	}
	if c.Budget, err = lend.ReadBudget(dir); err != nil {
		return sandboxCount{}, err
	}
	u, err := lend.ReadUsage(lend.UsagePath(app.InstDir, in.Group, in.Name, c.meter))
	switch {
	case err == nil:
		c.Usage, c.Metered = u, true
	case !errors.Is(err, os.ErrNotExist):
		return sandboxCount{}, err
	}
	c.Applied = c.Usage.Applies(c.Budget)
	return c, nil
}

// writeCount is `usage` for one sandbox.
func writeCount(out io.Writer, c sandboxCount, detail bool) {
	u := c.Usage
	head := []string{c.Sandbox + ":"}
	switch {
	case !c.Budget.Empty():
		head = append(head, budgetMode(c.Budget)+" budget")
	default:
		head = append(head, "no budget")
	}
	head = append(head, fmt.Sprintf("%d calls · %d in flight · %d estimated", u.Calls, u.InFlight, u.Estimated))
	if c.Budget.Hard || u.Refused > 0 {
		head = append(head, fmt.Sprintf("%d refused", u.Refused))
	}
	fmt.Fprintln(out, strings.Join(head, " "))
	if !c.Budget.Empty() && !c.Applied {
		if c.Metered {
			fmt.Fprintln(out, "  not yet applied: the lender has not read this budget since it changed, and reads it when the sandbox next calls a model or checks its count")
		} else {
			fmt.Fprintln(out, "  not yet applied: the lender has metered no call of this sandbox since it was created")
		}
	}
	if !c.Readable && !c.Budget.Empty() {
		fmt.Fprintln(out, "  created before token budgets: nothing inside it can read its count, so no agent is asked to stop; "+
			"--hard still refuses its calls at the limit")
	}

	total := u.Total()
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  type\tused\tlimit\tleft")
	for _, typ := range lend.Types {
		used := total.Of(typ)
		limit, limited := c.Budget.Limits[typ]
		if !limited {
			fmt.Fprintf(tw, "  %s\t%s\t-\t-\n", typ, lend.FormatTokens(used))
			continue
		}
		line, _ := c.Budget.AskLine(typ)
		left := "0%"
		if limit > 0 {
			left = strconv.FormatInt(max(0, (limit-used)*100/limit), 10) + "%"
		}
		switch {
		case used >= limit:
			left = "spent, " + lend.FormatTokens(used-limit) + " past the limit"
		case used >= line:
			left += ", past its ask line: the agent is asked to stop"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", typ, lend.FormatTokens(used), limitText(limit), left)
	}
	_ = tw.Flush()

	if len(u.Rows) > 0 {
		fmt.Fprintln(out)
		tw = tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		if detail {
			fmt.Fprintln(tw, "  slot\tsurface\tmodel\tuncached\tcache write\t5m\t1h\tcached\toutput\treasoning\tcalls\testimated")
		} else {
			fmt.Fprintln(tw, "  slot\tmodel\tinput\tcached\toutput\tcalls")
		}
		for _, r := range u.Rows {
			model := r.Model
			if model == "" {
				model = "(unread)"
			}
			if detail {
				fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%d\n", r.Slot, r.Surface, model,
					lend.FormatTokens(r.Input), lend.FormatTokens(r.CacheWrite), lend.FormatTokens(r.CacheWrite5m),
					lend.FormatTokens(r.CacheWrite1h), lend.FormatTokens(r.CacheRead), lend.FormatTokens(r.Output),
					lend.FormatTokens(r.Reasoning), r.Calls, r.Estimated)
				continue
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%d\n", r.Slot, model, lend.FormatTokens(r.Of(lend.TypeInput)),
				lend.FormatTokens(r.Of(lend.TypeCached)), lend.FormatTokens(r.Of(lend.TypeOutput)), r.Calls)
		}
		_ = tw.Flush()
	}
	if u.Estimated > 0 {
		e := u.EstimatedTokens
		fmt.Fprintf(out, "\n  %d calls estimated (aborted or unread): %s input, %s cached, %s output\n", u.Estimated,
			lend.FormatTokens(e.Of(lend.TypeInput)), lend.FormatTokens(e.Of(lend.TypeCached)), lend.FormatTokens(e.Of(lend.TypeOutput)))
	}
}

func newUsageCmd(app *App) *cobra.Command {
	var detail, asJSON bool
	cmd := &cobra.Command{
		Use:               "usage [name]",
		Short:             "Show the tokens a lent sandbox has spent, by type, against its budget",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: app.completeSandbox,
		RunE: func(cmd *cobra.Command, args []string) error {
			var counts []sandboxCount
			if len(args) == 1 {
				_, in, err := app.engineFor(args[0])
				if err != nil {
					return err
				}
				c, err := app.countOf(in)
				if err != nil {
					return err
				}
				counts = append(counts, c)
			} else {
				insts, err := state.List(app.InstDir)
				if err != nil {
					return err
				}
				for _, in := range insts {
					if len(loanSlots(app.InstDir, in.Group, in.Name)) == 0 {
						continue
					}
					c, err := app.countOf(in)
					if err != nil {
						return err
					}
					counts = append(counts, c)
				}
			}
			out := cmd.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(counts)
			}
			if len(counts) == 0 {
				fmt.Fprintln(out, "no sandbox borrows a credential, so no lender meters one")
				return nil
			}
			for i, c := range counts {
				if i > 0 {
					fmt.Fprintln(out)
				}
				writeCount(out, c, detail)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&detail, "detail", false,
		"add the finer buckets per row: uncached input and cache writes apart, the 5m/1h split of the writes, and reasoning")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the budget and the lender's record as JSON")
	return cmd
}

// budgetFlags are `budget`'s changes, applied in this order: clear, set, add,
// then the mode.
type budgetFlags struct {
	set, add, clear string
	hard, soft      bool
	askAt           string
}

func newBudgetCmd(app *App) *cobra.Command {
	f := &budgetFlags{}
	cmd := &cobra.Command{
		Use:   "budget <name>",
		Short: "Set, raise or clear a lent sandbox's token budget",
		Long: "Set, raise or clear a lent sandbox's token budget. A change applies from the sandbox's next call.\n\n" +
			"T is input, cached or output, and one flag can carry several, as in --add cached=250M,output=1M.\n" +
			"--clear removes every limit, and --clear=T the limits named. With no flag, budget prints the sandbox's usage.",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: app.completeSandbox,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBudget(cmd, app, args[0], f)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.set, "set", "", "set limits: T=N[,T=N…]; below what is already spent exhausts that type at once")
	fl.StringVar(&f.add, "add", "", "raise limits by this much: T=N[,T=N…]")
	fl.StringVar(&f.clear, "clear", "", "remove every limit, or with =T[,T…] the limits named")
	fl.Lookup("clear").NoOptDefVal = "*"
	fl.BoolVar(&f.hard, "hard", false, "make every limit a ceiling: the lender refuses the sandbox's calls once a type reaches its limit")
	fl.BoolVar(&f.soft, "soft", false, "make the budget soft again: the agent is asked to stop at each limit, and no call is refused")
	fl.StringVar(&f.askAt, "ask-at", "", "where a hard budget asks the agent to stop, as a share of each limit (default 95%)")
	return cmd
}

func runBudget(cmd *cobra.Command, app *App, ref string, f *budgetFlags) error {
	_, in, err := app.engineFor(ref)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	changing := f.set != "" || f.add != "" || f.clear != "" || f.hard || f.soft || f.askAt != ""
	if !changing {
		c, err := app.countOf(in)
		if err != nil {
			return err
		}
		writeCount(out, c, false)
		return nil
	}
	if f.hard && f.soft {
		return errors.New("--hard and --soft ask for opposite things: pick one")
	}
	var set, add map[string]int64
	if f.set != "" {
		if set, err = parseTokenBudget("--set", f.set); err != nil {
			return err
		}
	}
	if f.add != "" {
		if add, err = parseTokenBudget("--add", f.add); err != nil {
			return err
		}
	}
	var clear []string
	if f.clear != "" && f.clear != "*" {
		for typ := range strings.SplitSeq(f.clear, ",") {
			typ = strings.TrimSpace(typ)
			if !slices.Contains(lend.Types, typ) {
				return fmt.Errorf("--clear=%s: unknown token type %q: use %s", f.clear, typ, strings.Join(lend.Types, ", "))
			}
			clear = append(clear, typ)
		}
	}
	var askAt float64
	if f.askAt != "" {
		if askAt, err = parseAskAt(f.askAt); err != nil {
			return err
		}
	}
	if _, err := app.countOf(in); err != nil {
		return err
	}

	dir := state.Dir(app.InstDir, in.Group, in.Name)
	// Per instance, never the root's create lock: a top-up must not wait behind
	// every create on the host.
	l := lock.NewAt(filepath.Join(dir, ".budget.lock"))
	if err := l.Acquire(); err != nil {
		return err
	}
	defer l.Release()
	b, err := lend.ReadBudget(dir)
	if err != nil {
		return err
	}
	if b.Limits == nil {
		b.Limits = map[string]int64{}
	}
	now := time.Now()
	switch {
	case f.clear == "*":
		b.Limits = map[string]int64{}
		b.Record(now, "clear")
	case len(clear) > 0:
		for _, typ := range clear {
			delete(b.Limits, typ)
		}
		b.Record(now, "clear "+strings.Join(clear, ","))
	}
	if set != nil {
		maps.Copy(b.Limits, set)
		b.Record(now, "set "+budgetText(lend.Budget{Limits: set}))
	}
	if add != nil {
		for typ, n := range add {
			if _, ok := b.Limits[typ]; !ok {
				return fmt.Errorf("--add %s: %s has no limit to raise; --set gives it one", f.add, typ)
			}
			b.Limits[typ] += n
		}
		b.Record(now, "add "+budgetText(lend.Budget{Limits: add}))
	}
	switch {
	case f.hard:
		b.Hard = true
	case f.soft:
		b.Hard, b.AskAt = false, 0
		b.Record(now, "soft")
	}
	if askAt != 0 {
		if !b.Hard {
			return errors.New("--ask-at moves a hard budget's ask line, and this budget is soft: a soft budget asks at each limit; add --hard")
		}
		b.AskAt = askAt
	}
	if f.hard || askAt != 0 {
		b.Record(now, budgetMode(b))
	}
	if b.Empty() && b.Hard {
		// A budget with no limit left is no budget, and its mode goes with it.
		// Only a command that asked for --hard and set nothing is a mistake.
		if f.hard || askAt != 0 {
			return errors.New("a hard budget needs a limit to hold: --set one, or --soft")
		}
		b.Hard, b.AskAt = false, 0
	}
	if !b.Empty() {
		if err := app.checkLenderEnforces(cmd.Context(), in.Group, b.Hard); err != nil {
			return err
		}
	}
	if err := lend.WriteBudget(dir, b); err != nil {
		return err
	}
	c, err := app.countOf(in)
	if err != nil {
		return err
	}
	writeCount(out, c, false)
	return nil
}

// checkLenderEnforces asks the group's running lender whether it meters, and
// for a hard budget whether it refuses. A lender older than token budgets would
// ignore the file and read as a test that passed, so it is refused here, with
// the command that replaces it. A group whose lender is not running is not an
// error: the budget applies when it starts, and the one that starts is current.
func (app *App) checkLenderEnforces(ctx context.Context, group string, hard bool) error {
	if app.dryRun() {
		return nil
	}
	b := app.lenderBox(group)
	if !b.running(ctx) {
		fmt.Fprintf(app.stderr(), "cs-sandbox: %s's lender is not running; the budget applies when it starts, with the group's next create or start\n", group)
		return nil
	}
	feats, err := b.features(ctx)
	if err != nil {
		return err
	}
	need := []string{"meter"}
	if hard {
		need = append(need, "refuse")
	}
	for _, n := range need {
		if !slices.Contains(feats, n) {
			return fmt.Errorf("the credential lender of group %s predates token budgets, so it would ignore this one: "+
				"`cs-sandbox start <a sandbox of %s>` replaces it, cutting the calls in flight once, and then set the budget again",
				group, group)
		}
	}
	return nil
}

// tokenView is a sandbox's count as inspect and ls show it.
type tokenView struct {
	Used    map[string]int64 `json:"used"`
	Limits  map[string]int64 `json:"limits,omitempty"`
	Mode    string           `json:"mode,omitempty"`
	Applied bool             `json:"applied"`
}

func viewOf(c sandboxCount) *tokenView {
	t := c.Usage.Total()
	v := &tokenView{Used: map[string]int64{}, Limits: c.Budget.Limits, Applied: c.Applied}
	for _, typ := range lend.Types {
		v.Used[typ] = t.Of(typ)
	}
	if !c.Budget.Empty() {
		v.Mode = budgetMode(c.Budget)
	}
	return v
}

// budgetLine is each limited type's use against its limit, as in
// "input 4.80M/20M · cached 1.00G/1G (spent) · output 1.70M/5M, soft".
func (v *tokenView) budgetLine() string {
	var parts []string
	for _, typ := range lend.Types {
		limit, ok := v.Limits[typ]
		if !ok {
			continue
		}
		p := fmt.Sprintf("%s %s/%s", typ, lend.FormatTokens(v.Used[typ]), limitText(limit))
		if v.Used[typ] >= limit {
			p += " (spent)"
		}
		parts = append(parts, p)
	}
	line := strings.Join(parts, " · ") + ", " + v.Mode
	if !v.Applied {
		line += " (not yet applied)"
	}
	return line
}

func (v *tokenView) usedLine() string {
	return fmt.Sprintf("input %s · cached %s · output %s (no budget)", lend.FormatTokens(v.Used[lend.TypeInput]),
		lend.FormatTokens(v.Used[lend.TypeCached]), lend.FormatTokens(v.Used[lend.TypeOutput]))
}

// fullest is the limited type with the largest share used, for ls: "cached 59%".
func (v *tokenView) fullest() string {
	best, share := "", -1.0
	for _, typ := range lend.Types {
		limit, ok := v.Limits[typ]
		if !ok {
			continue
		}
		s := 1.0
		if limit > 0 {
			s = float64(v.Used[typ]) / float64(limit)
		}
		if s > share {
			best, share = typ, s
		}
	}
	if best == "" {
		return "-"
	}
	return fmt.Sprintf("%s %d%%", best, int(math.Floor(share*100)))
}

// finalCount is what a sandbox spent, read before destroy or rm removes the
// directory its record is named from: the line to print, and the record's path
// for the caller to remove once the sandbox is gone. A budget belongs to one
// creation, so rm resets it as destroy does.
func (app *App) finalCount(in *state.Instance) (line, path string) {
	c, err := app.countOf(in)
	if err != nil || !c.Metered {
		return "", ""
	}
	t := c.Usage.Total()
	line = fmt.Sprintf("  tokens spent: input %s · cached %s · output %s, over %d calls (%d estimated, %d in flight)",
		lend.FormatTokens(t.Of(lend.TypeInput)), lend.FormatTokens(t.Of(lend.TypeCached)),
		lend.FormatTokens(t.Of(lend.TypeOutput)), c.Usage.Calls, c.Usage.Estimated, c.Usage.InFlight)
	return line, lend.UsagePath(app.InstDir, in.Group, in.Name, c.meter)
}

// dropCount removes a gone sandbox's usage record, and sweeps its group.
func (app *App) dropCount(group, path string) {
	if path != "" && !app.dryRun() {
		_ = os.Remove(path)
	}
	app.sweepUsage(group)
}

// usageSweepAfter is how long a usage record no live sandbox owns is left
// before the sweep removes it. A call in flight when its sandbox is destroyed
// settles when its stream ends and writes the record again, and the loan table
// honours a destroyed sandbox for a moment after it goes.
const usageSweepAfter = time.Minute

// sweepUsage removes the usage records in a group that belong to no live
// sandbox (R155d). destroy, rm and create run it, so nothing is left until the
// group goes.
func (app *App) sweepUsage(group string) {
	if app.dryRun() {
		return
	}
	dir := lend.UsageDir(app.InstDir, group)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	live := map[string]bool{}
	insts, _ := state.List(app.InstDir)
	for _, in := range insts {
		if in.Group != group {
			continue
		}
		if loans, err := lend.ReadLoans(state.Dir(app.InstDir, in.Group, in.Name)); err == nil && len(loans) > 0 {
			live[filepath.Base(lend.UsagePath(app.InstDir, group, in.Name, lend.MeterID(loans)))] = true
		}
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || live[e.Name()] {
			continue
		}
		if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) > usageSweepAfter {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
