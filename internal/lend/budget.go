package lend

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A token budget is the operator's, per sandbox, like a fault (R147c): a file
// the host writes beside the sandbox's loans, which no part of a request can
// reach. It limits each token type on its own (R147d), and a type it does not
// name has no limit.
//
// What the sandbox has consumed is the lender's, because only the lender sees
// every lent call: every session, every sub-agent, every side call and every
// script. It lives in the one directory the lender may write (R153g), and the
// count in memory is the authoritative one.

// BudgetFile is the limits' file name, beside loans.json in the instance
// directory.
const BudgetFile = "budget.json"

// DefaultAskAt is a hard budget's ask line, as a share of each limit: the agent
// is asked to hand off here, so its handoff fits under the ceiling. A soft
// budget asks at the limit itself.
const DefaultAskAt = 0.95

// maxHistory bounds the changes a budget file keeps.
const maxHistory = 50

// Budget is one sandbox's limits.
type Budget struct {
	// Limits holds one number per limited type. A type it leaves out is
	// unlimited.
	Limits map[string]int64 `json:"limits,omitempty"`
	// Hard makes every limit a ceiling: the lender refuses the sandbox's
	// billable calls once a type reaches its limit.
	Hard bool `json:"hard,omitempty"`
	// AskAt is a hard budget's ask line, as a share of each limit. Zero is
	// DefaultAskAt. A soft budget's ask line is the limit, whatever this says.
	AskAt float64 `json:"ask,omitempty"`
	// History is what changed the budget, newest last, so a top-up can be
	// accounted for later.
	History []BudgetChange `json:"history,omitempty"`
}

// BudgetChange is one change to a budget.
type BudgetChange struct {
	At     time.Time `json:"at"`
	Change string    `json:"change"`
}

// Empty says the budget limits nothing.
func (b Budget) Empty() bool { return len(b.Limits) == 0 }

// Record appends a change to the history, keeping it short.
func (b *Budget) Record(at time.Time, change string) {
	b.History = append(b.History, BudgetChange{At: at.UTC(), Change: change})
	if len(b.History) > maxHistory {
		b.History = b.History[len(b.History)-maxHistory:]
	}
}

// askShare is the share of each limit the ask line sits at.
func (b Budget) askShare() float64 {
	if !b.Hard {
		return 1
	}
	if b.AskAt <= 0 || b.AskAt >= 1 {
		return DefaultAskAt
	}
	return b.AskAt
}

// AskLine is where a limited type's agent is asked to stop.
func (b Budget) AskLine(typ string) (int64, bool) {
	limit, ok := b.Limits[typ]
	if !ok {
		return 0, false
	}
	return int64(math.Floor(float64(limit) * b.askShare())), true
}

// Status names the limited types past their ask line, and those past the limit
// itself, in report order.
func (b Budget) Status(used Tokens) (ask, spent []string) {
	for _, typ := range Types {
		limit, ok := b.Limits[typ]
		if !ok {
			continue
		}
		line, _ := b.AskLine(typ)
		if used.Of(typ) >= line {
			ask = append(ask, typ)
		}
		if used.Of(typ) >= limit {
			spent = append(spent, typ)
		}
	}
	return ask, spent
}

// Validate reports what is wrong with a budget before it is written.
func (b Budget) Validate() error {
	for typ, n := range b.Limits {
		if !slices.Contains(Types, typ) {
			return fmt.Errorf("unknown token type %q: use %s", typ, strings.Join(Types, ", "))
		}
		if n < 0 {
			return fmt.Errorf("the %s limit is negative", typ)
		}
	}
	if b.AskAt < 0 || b.AskAt >= 1 {
		return fmt.Errorf("the ask line is a share of the limit, below 1, not %g", b.AskAt)
	}
	return nil
}

// ReadBudget returns the budget of the sandbox whose instance directory this
// is. None is not an error, and reads as an empty budget.
func ReadBudget(instDir string) (Budget, error) {
	data, err := os.ReadFile(filepath.Join(instDir, BudgetFile))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Budget{}, nil
		}
		return Budget{}, err
	}
	var b Budget
	if err := json.Unmarshal(data, &b); err != nil {
		return Budget{}, fmt.Errorf("%s is not readable: %w", BudgetFile, err)
	}
	return b, nil
}

// WriteBudget replaces the sandbox's budget. It is renamed into place at 0600,
// because the lender reads it per call and must never see half of one.
func WriteBudget(instDir string, b Budget) error {
	if err := b.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(instDir, BudgetFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// MeterID names one creation of a sandbox: the first 16 hex digits of a SHA-256
// over its loan tokens, sorted. Loan tokens are minted once per creation and
// never again, so the id changes exactly when the sandbox is created again, and
// it needs no field of its own in loans.json. A 64-bit prefix of a hash over
// 128-bit random tokens discloses nothing of them.
func MeterID(loans []Loan) string {
	toks := make([]string, 0, len(loans))
	for _, l := range loans {
		toks = append(toks, l.Token)
	}
	slices.Sort(toks)
	sum := sha256.Sum256([]byte(strings.Join(toks, "\n")))
	return hex.EncodeToString(sum[:8])
}

// UsageDir is a group's usage directory, the one the lender may write (R153g).
// It sits under .lender, beside the staged lender binary, and is mounted on its
// own: a lender that could write .lender could write its own next binary.
func UsageDir(instances, group string) string {
	return filepath.Join(instances, group, ".lender", "usage")
}

// UsagePath is one sandbox's usage record.
func UsagePath(instances, group, name, meter string) string {
	return filepath.Join(UsageDir(instances, group), name+"."+meter+".json")
}

// MaxUsageFile bounds what a reader of a usage file accepts. A compromised
// lender writes these, so the CLI reads them defensively.
const MaxUsageFile = 4 << 20

// Usage is one sandbox's consumption, as the lender keeps it.
type Usage struct {
	Sandbox string    `json:"sandbox"`
	Group   string    `json:"group"`
	Meter   string    `json:"meter"`
	Updated time.Time `json:"updated"`
	// Calls are the settled calls, and Estimated those of them whose usage
	// was not read, charged at least their estimate. EstimatedTokens is what
	// those were charged.
	Calls           int64  `json:"calls"`
	Estimated       int64  `json:"estimated"`
	EstimatedTokens Tokens `json:"estimated_tokens"`
	// Refused are the calls a hard budget refused, which cost nothing.
	Refused int64 `json:"refused"`
	// InFlight are the calls answered and not yet settled, as of the last
	// write.
	InFlight int `json:"in_flight"`
	// Rows are keyed by slot, surface and model.
	Rows []UsageRow `json:"rows"`
	// Applied is the budget the lender last read for this sandbox, without its
	// history, so a reader can tell a budget the lender applies from one it
	// has never seen.
	Applied *Budget `json:"applied,omitempty"`
}

// UsageRow is the consumption of one slot, surface and model.
type UsageRow struct {
	Slot    string `json:"slot"`
	Surface string `json:"surface"`
	Model   string `json:"model"`
	Tokens
	Calls     int64 `json:"calls"`
	Estimated int64 `json:"estimated"`
}

// Total is every row added up.
func (u Usage) Total() Tokens {
	var t Tokens
	for _, r := range u.Rows {
		t.Add(r.Tokens)
	}
	return t
}

func (u *Usage) add(slot string, sf surface, model string, t Tokens, estimated bool) {
	u.Calls++
	if estimated {
		u.Estimated++
		u.EstimatedTokens.Add(t)
	}
	for i := range u.Rows {
		r := &u.Rows[i]
		if r.Slot == slot && r.Surface == string(sf) && r.Model == model {
			r.Add(t)
			r.Calls++
			if estimated {
				r.Estimated++
			}
			return
		}
	}
	row := UsageRow{Slot: slot, Surface: string(sf), Model: model, Tokens: t, Calls: 1}
	if estimated {
		row.Estimated = 1
	}
	u.Rows = append(u.Rows, row)
}

// ReadUsage reads one usage record, refusing one larger than MaxUsageFile.
func ReadUsage(path string) (Usage, error) {
	f, err := os.Open(path)
	if err != nil {
		return Usage{}, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxUsageFile+1))
	if err != nil {
		return Usage{}, err
	}
	if len(data) > MaxUsageFile {
		return Usage{}, fmt.Errorf("%s is larger than a usage record can be", path)
	}
	var u Usage
	if err := json.Unmarshal(data, &u); err != nil {
		return Usage{}, fmt.Errorf("%s is not readable: %w", path, err)
	}
	return u, nil
}

// Meter keeps every sandbox's count, for one lender. The count in memory is the
// authoritative one, and every settle writes it through to the sandbox's usage
// record, so a lender that restarts carries on from the file.
type Meter struct {
	// Dir is the instances root, where each sandbox's budget is read and each
	// group's usage directory is.
	Dir string
	// Group is the group this lender serves, or "" for a lender on the host,
	// which serves every group.
	Group string
	Log   *slog.Logger

	mu    sync.Mutex
	books map[string]*book
}

// book is one sandbox's count, for one creation of it.
type book struct {
	path  string
	usage Usage
	// share is each slot's last split of a read call's prompt into cached
	// input, which an estimate follows. largest is the largest prompt read.
	share   map[string]float64
	largest int64

	budget     Budget
	budgetSeen os.FileInfo
	budgetErr  error

	inFlight   int
	unwritable bool // said once: the record could not be written, so this counts in memory only
	flatSaid   bool // said once: a request with no usable length was estimated
}

// NewMeter returns a meter over this instances root.
func NewMeter(dir, group string, log *slog.Logger) *Meter {
	return &Meter{Dir: dir, Group: group, Log: log}
}

func (m *Meter) log() *slog.Logger {
	if m.Log == nil {
		return slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return m.Log
}

// Features are what this lender can enforce, for /healthz. A lender that
// cannot write its usage directory counts in memory only, which a restart
// loses, so it does not claim to meter.
func (m *Meter) Features() []string {
	if m == nil {
		return nil
	}
	if m.Group != "" && !writable(UsageDir(m.Dir, m.Group)) {
		return nil
	}
	return []string{"meter", "refuse"}
}

func writable(dir string) bool {
	f, err := os.CreateTemp(dir, ".probe-*")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return true
}

// open returns a sandbox's book, loading its usage record the first time the
// sandbox is seen rather than scanning every group at start. Called with mu
// held.
func (m *Meter) open(loan Loan) *book {
	id := loan.Meter
	if id == "" {
		id = MeterID([]Loan{loan})
	}
	key := loan.Group + "/" + loan.Name + "/" + id
	if b, ok := m.books[key]; ok {
		return b
	}
	if m.books == nil {
		m.books = map[string]*book{}
	}
	b := &book{path: UsagePath(m.Dir, loan.Group, loan.Name, id), share: map[string]float64{}}
	if u, err := ReadUsage(b.path); err == nil {
		b.usage = u
		for _, r := range u.Rows {
			b.largest = max(b.largest, r.prompt()/max(r.Calls, 1))
		}
	}
	// The calls in flight died with the process that held them.
	b.usage.InFlight = 0
	b.usage.Sandbox, b.usage.Group, b.usage.Meter = loan.Name, loan.Group, id
	m.books[key] = b
	return b
}

// budgetOf re-reads the sandbox's budget when the file has changed. Every
// rename makes a new file, so a top-up applies from the next call. Called with
// mu held.
func (m *Meter) budgetOf(loan Loan, b *book) Budget {
	path := filepath.Join(m.Dir, loan.Group, loan.Name, BudgetFile)
	fi, err := os.Stat(path)
	switch {
	case err != nil:
		b.budget, b.budgetSeen, b.budgetErr = Budget{}, nil, nil
	case b.budgetSeen == nil || !os.SameFile(fi, b.budgetSeen) ||
		fi.ModTime() != b.budgetSeen.ModTime() || fi.Size() != b.budgetSeen.Size():
		b.budget, b.budgetErr = ReadBudget(filepath.Dir(path))
		b.budgetSeen = fi
	}
	applied := b.budget
	applied.History = nil
	if applied.Empty() && !applied.Hard {
		b.usage.Applied = nil
	} else {
		b.usage.Applied = &applied
	}
	return b.budget
}

// write puts the sandbox's count on disk. Called with mu held.
func (m *Meter) write(b *book) {
	b.usage.Updated = time.Now().UTC()
	b.usage.InFlight = b.inFlight
	data, err := json.MarshalIndent(b.usage, "", "  ")
	if err == nil {
		if m.Group == "" {
			// A lender on the host writes wherever its instances are. One in a
			// container writes only its group's usage directory, which create
			// made before it started, and makes nothing.
			err = os.MkdirAll(filepath.Dir(b.path), 0o700)
		}
		if err == nil {
			tmp := b.path + ".tmp"
			if err = os.WriteFile(tmp, append(data, '\n'), 0o600); err == nil {
				err = os.Rename(tmp, b.path)
			}
		}
	}
	if err != nil && !b.unwritable {
		b.unwritable = true
		m.log().Warn("cannot write the usage record, so this sandbox is counted in memory only",
			slog.String("sandbox", b.usage.Sandbox), slog.String("group", b.usage.Group), slog.Any("err", err))
	}
}

// begin counts a call whose answer has arrived and whose usage is still to
// come.
func (m *Meter) begin(c call) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.open(c.loan).inFlight++
}

// settle charges a call, and says what it charged.
func (m *Meter) settle(c call, r reading, status int) (Tokens, bool, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.open(c.loan)
	b.inFlight = max(b.inFlight-1, 0)
	est := estimate(c, b.share[c.slot], b.largest)
	if (c.length < 0 || c.encoded) && !r.final && status < 400 && !b.flatSaid {
		b.flatSaid = true
		m.log().Warn("estimated a call whose request has no usable length",
			slog.String("sandbox", c.loan.Name), slog.String("slot", c.slot),
			slog.Bool("encoded", c.encoded), slog.Int64("prompt_tokens", est.prompt()))
	}
	t, estimated := charge(r, status, est)
	if !estimated && r.final {
		if p := t.prompt(); p > 0 {
			b.share[c.slot] = float64(t.CacheRead) / float64(p)
			b.largest = max(b.largest, p)
		}
	}
	model := r.model
	b.usage.add(c.slot, c.surface, model, t, estimated)
	m.budgetOf(c.loan, b)
	m.write(b)
	return t, estimated, model
}

// Reading is what a sandbox is told of its own count, by the lender's answer
// on MeterPath (R153f). The turn driver reads it before a turn, and the
// hand-off hook after each tool call.
type Reading struct {
	Sandbox string           `json:"sandbox"`
	Hard    bool             `json:"hard"`
	AskAt   float64          `json:"ask_at"`
	Limits  map[string]int64 `json:"limits"`
	Used    map[string]int64 `json:"used"`
	// Ask names the limited types at or past their ask line, and Spent those
	// at or past the limit itself.
	Ask       []string `json:"ask"`
	Spent     []string `json:"spent"`
	Calls     int64    `json:"calls"`
	Estimated int64    `json:"estimated"`
	Refused   int64    `json:"refused"`
	// Applied is false when the sandbox's budget file could not be read, so
	// nothing is being enforced against it.
	Applied bool `json:"applied"`
}

// Read is a sandbox's reading.
func (m *Meter) Read(loan Loan) Reading {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.open(loan)
	bud := m.budgetOf(loan, b)
	used := b.usage.Total()
	ask, spent := bud.Status(used)
	rd := Reading{
		Sandbox: loan.Name, Hard: bud.Hard, AskAt: bud.askShare(),
		Limits: map[string]int64{}, Used: map[string]int64{},
		Ask: nonNil(ask), Spent: nonNil(spent),
		Calls: b.usage.Calls, Estimated: b.usage.Estimated, Refused: b.usage.Refused,
		Applied: b.budgetErr == nil,
	}
	maps.Copy(rd.Limits, bud.Limits)
	for _, typ := range Types {
		rd.Used[typ] = used.Of(typ)
	}
	return rd
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// Spent is the type a hard budget has run out of, which the next billable call
// is refused for, with what was used of it and its limit. Under a soft budget,
// or with every type under its limit, it returns false.
type Spent struct {
	Type        string
	Used, Limit int64
}

// refusal says whether a sandbox's next billable call is to be refused under a
// hard budget (R153h), and counts it when it is.
func (m *Meter) refusal(loan Loan) (Spent, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.open(loan)
	bud := m.budgetOf(loan, b)
	if !bud.Hard {
		return Spent{}, false
	}
	used := b.usage.Total()
	for _, typ := range Types {
		limit, ok := bud.Limits[typ]
		if ok && used.Of(typ) >= limit {
			b.usage.Refused++
			m.write(b)
			return Spent{Type: typ, Used: used.Of(typ), Limit: limit}, true
		}
	}
	return Spent{}, false
}

// SpentMarker opens every refusal of a spent budget, and every turn driver
// matches on it before any rule of its own. A 400 is also what a malformed
// request gets, and only Codex keeps the error's type, so the message is all a
// driver can tell the two apart by. Lowercase, because the drivers compare
// against the lowercased message.
const SpentMarker = "cs-sandbox token budget spent"

// typeWords name a token type in a refusal.
var typeWords = map[string]string{
	TypeInput:  "new-input tokens",
	TypeCached: "cached-input tokens",
	TypeOutput: "output tokens",
}

// TypeWords is how a refusal names a token type.
func TypeWords(typ string) string { return typeWords[typ] }

// classWords are what the turn drivers read as another class: a throttle, an
// oversized context, an unreachable provider, a provider at capacity, or a wait
// to honour. A refusal that held one would be retried, or restarted, rather
// than held.
var classWords = []string{
	"rate limit", "too many requests", "context", "limit", "length", "window", "too long",
	"connection", "timed out", "network", "fetch failed", "socket", "unable to connect",
	"overloaded", "capacity", "try again", "retry",
}

// spentMessage is the refusal's message. It names the type that ran out,
// because that is the type the operator raises, and it addresses the operator
// rather than the agent: the sandbox ships cs-sandbox, and an agent told to run
// it would try, and spend a turn. The sandbox's name is the operator's, and is
// left out when it holds a word a driver would read as another class.
func spentMessage(sandbox string, s Spent) string {
	name := "sandbox " + sandbox
	low := strings.ToLower(sandbox)
	for _, w := range classWords {
		if strings.Contains(low, w) {
			name = "this sandbox"
			break
		}
	}
	return fmt.Sprintf("%s: %s has used %s of its %s %s. The operator can raise it on the host.",
		SpentMarker, name, FormatTokens(s.Used), FormatTokens(s.Limit), TypeWords(s.Type))
}

// FormatTokens writes a count the way the commands and the refusal do: 2.00M,
// 1.00G, 512.0K.
func FormatTokens(n int64) string {
	switch a := math.Abs(float64(n)); {
	case a >= 1e9:
		return fmt.Sprintf("%.2fG", float64(n)/1e9)
	case a >= 1e6:
		return fmt.Sprintf("%.2fM", float64(n)/1e6)
	case a >= 1e3:
		return fmt.Sprintf("%.1fK", float64(n)/1e3)
	}
	return strconv.FormatInt(n, 10)
}
