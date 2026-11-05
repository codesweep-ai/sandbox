package lend

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
)

// Metering a lent call (R153d, R153e).
//
// The lender reads the usage a provider reports in each response it forwards,
// and charges it to the sandbox whose loan the call spent. It reads the
// response as it streams past and changes none of it, and it never reads the
// request body: what a call cost is the provider's own figure, on its answer.
//
// The arithmetic is the part that is easy to get wrong, and each reader below
// keeps the rule cs-tracer learned for its surface. Streamed counts are
// overwritten and never added. An OpenAI-shaped prompt count already holds its
// cached tokens, so they are subtracted. Reasoning is part of output on every
// surface, so it is reported and never added.

// The token types a budget limits. Each is a sum of the buckets in Tokens, and
// Tokens.Of says which.
const (
	TypeInput  = "input"  // new input: prompt tokens the provider processed without a cache hit, cache writes included
	TypeCached = "cached" // prompt tokens read back from the cache
	TypeOutput = "output" // output, reasoning included
)

// Types are the token types, in the order they are reported.
var Types = []string{TypeInput, TypeCached, TypeOutput}

// Tokens is one call's usage, or a sum of calls, in disjoint buckets. The names
// are tracer's, so the two can be compared row for row.
type Tokens struct {
	Input        int64 `json:"input"` // prompt tokens neither read from the cache nor written to it
	CacheWrite   int64 `json:"cacheWrite"`
	CacheWrite5m int64 `json:"cacheWrite5m,omitempty"`
	CacheWrite1h int64 `json:"cacheWrite1h,omitempty"`
	CacheRead    int64 `json:"cacheRead"`
	Output       int64 `json:"output"`
	Reasoning    int64 `json:"reasoning,omitempty"` // a part of Output, never added to it
}

// Of is the count of one token type.
func (t Tokens) Of(typ string) int64 {
	switch typ {
	case TypeInput:
		return t.Input + t.CacheWrite
	case TypeCached:
		return t.CacheRead
	case TypeOutput:
		return t.Output
	}
	return 0
}

// Add sums another call into this one.
func (t *Tokens) Add(o Tokens) {
	t.Input += o.Input
	t.CacheWrite += o.CacheWrite
	t.CacheWrite5m += o.CacheWrite5m
	t.CacheWrite1h += o.CacheWrite1h
	t.CacheRead += o.CacheRead
	t.Output += o.Output
	t.Reasoning += o.Reasoning
}

// prompt is every prompt token the call sent, cached or not.
func (t Tokens) prompt() int64 { return t.Input + t.CacheWrite + t.CacheRead }

// A surface is one shape of model API, and the unit the readers are keyed on.
// The names are cs-vcr's, which classifies a request the same way: by its path
// alone, with the version segment removed.
type surface string

const (
	surfaceMessages  surface = "anthropic.messages"
	surfaceResponses surface = "openai.responses"
	surfaceChat      surface = "openai.chat"
	// surfaceOther is any other POST a slot fronts: embeddings, compaction, a
	// path this build has never seen. It is charged an estimate unless its
	// answer carries usage a reader recognises. Refusing it instead would break
	// whatever a client adds next.
	surfaceOther surface = "other"
	// surfaceFree costs nothing: a GET, a HEAD, and Anthropic's token counter,
	// whose answer has an input_tokens field that is not a charge.
	surfaceFree surface = ""
)

// surfaceOf classifies a lent request by its method and its path. The free list
// is matched exactly, and a path the lender would not forward never gets this
// far (R146b).
func surfaceOf(method, p string) surface {
	if method != http.MethodPost {
		// Assumes a provider refuses every other method on its model paths, so
		// none of them can be billed. Not yet checked against each provider.
		return surfaceFree
	}
	switch apiPath(p) {
	case "/messages":
		return surfaceMessages
	case "/messages/count_tokens":
		return surfaceFree
	case "/responses":
		return surfaceResponses
	case "/chat/completions":
		return surfaceChat
	}
	return surfaceOther
}

// apiPath is a path with its version segment removed, the way cs-vcr reads one,
// so a client that versions its base URL and one that does not read alike. The
// codex login's origin has no version segment, and its paths are read as they
// are.
func apiPath(p string) string {
	rest, ok := strings.CutPrefix(p, "/v")
	if !ok {
		return p
	}
	seg, tail, _ := strings.Cut(rest, "/")
	if seg == "" || strings.Trim(seg, "0123456789") != "" {
		return p
	}
	return "/" + tail
}

// bytesPerToken divides a request's length into an estimate of its prompt, for
// a call whose usage was not read. Each errs high against every request in the
// cassettes, which were 2.83-3.32 bytes a token on Anthropic, 4.71-4.90 on
// Responses and 4.62-4.77 on chat. High is the safe direction: an estimate that
// is too large asks a sandbox to stop a little early.
var bytesPerToken = map[surface]float64{
	surfaceMessages:  2.5,
	surfaceResponses: 4.5,
	surfaceChat:      4,
	surfaceOther:     2.5,
}

// estimateOutput is what an unread call is charged for output. On this host a
// call's output had a median of 512 tokens and a 90th percentile of 1,938.
const estimateOutput = 8 << 10

// flatPrompt is the prompt estimate for a sandbox's first unread call that has
// no usable length, before it has sent a prompt the meter could count.
const flatPrompt = 1_000_000

// maxEvent bounds what the scanner buffers: one usage-bearing event, or one
// whole JSON answer. A Responses terminal event echoes the instructions, the
// tools and the whole output, so it can run to megabytes. A call past this is
// estimated rather than read.
const maxEvent = 16 << 20

// reading is what a scanner found in one answer.
type reading struct {
	Tokens
	model  string
	seen   bool // some usage was read
	final  bool // the call's last usage was read
	failed bool // the stream reported an error of its own
	output bool // the model's answer had begun
	over   bool // an event was too large to read
}

// outputEvent says whether an event of this name is part of the model's answer.
// A stream's own bookkeeping comes before these, and a provider that refuses a
// call inside a stream sends none of them.
func outputEvent(name string) bool {
	return strings.HasPrefix(name, "content_block_") || strings.HasPrefix(name, "response.output_")
}

// usageScan reads usage out of an answer as it streams past. It frames SSE per
// the specification, joining multi-line data fields, and keeps only the events
// that carry usage. Every other event is passed over without being buffered.
// An answer that starts with a brace is a JSON body, read whole at the end.
type usageScan struct {
	surface surface
	mode    byte // 0 until the first byte decides, then 's' for SSE, 'j' for JSON, 'x' for neither
	r       reading

	// The SSE line being read: its first bytes until they name the field, what
	// follows the colon of a field this keeps, and the event so far.
	head  []byte
	field byte // 0 until the head decides, then 'd' a kept data line, 'e' an event name, 's' skipped
	val   []byte
	event string
	data  []byte
	lines int
	over  bool

	body []byte // a JSON answer
}

func newScan(sf surface) *usageScan { return &usageScan{surface: sf} }

// Write feeds the scanner the next bytes of the answer. It never fails: what it
// cannot read is estimated at the end.
func (u *usageScan) Write(p []byte) {
	if u.mode == 0 {
		p = bytes.TrimLeft(p, " \t\r\n")
		if len(p) == 0 {
			return
		}
		// The framing is told by the first bytes, never by Content-Type: the
		// codex login streams with no content type at all.
		u.mode = 's'
		if p[0] == '{' || p[0] == '[' {
			u.mode = 'j'
		}
	}
	switch u.mode {
	case 'j':
		if len(u.body)+len(p) > maxEvent {
			u.r.over, u.body, u.mode = true, nil, 'x'
			return
		}
		u.body = append(u.body, p...)
	case 's':
		for len(p) > 0 {
			i := bytes.IndexByte(p, '\n')
			if i < 0 {
				u.feed(p)
				return
			}
			u.feed(p[:i])
			u.endLine()
			p = p[i+1:]
		}
	}
}

// feed takes part of one line, without its newline.
func (u *usageScan) feed(seg []byte) {
	if u.field == 0 {
		n := min(6-len(u.head), len(seg))
		u.head = append(u.head, seg[:n]...)
		seg = seg[n:]
		if len(u.head) < 6 {
			return
		}
		u.decide()
	}
	u.take(seg)
}

// decide names the line's field from its first bytes.
func (u *usageScan) decide() {
	h := u.head
	switch {
	case bytes.HasPrefix(h, []byte("data:")):
		u.field = 's'
		if u.keep() {
			u.field = 'd'
		}
		u.take(h[len("data:"):])
	case bytes.HasPrefix(h, []byte("event:")):
		u.field = 'e'
		u.take(h[len("event:"):])
	default:
		// A comment, an id, a retry, or a field nothing here reads.
		u.field = 's'
	}
	u.head = u.head[:0]
}

func (u *usageScan) take(b []byte) {
	switch u.field {
	case 'e':
		if len(u.val)+len(b) <= 256 {
			u.val = append(u.val, b...)
		}
	case 'd':
		if u.over {
			return
		}
		if len(u.data)+len(u.val)+len(b) > maxEvent {
			u.over, u.r.over, u.val, u.data = true, true, u.val[:0], u.data[:0]
			return
		}
		u.val = append(u.val, b...)
	}
}

func (u *usageScan) endLine() {
	if u.field == 0 {
		if len(bytes.TrimRight(u.head, "\r")) == 0 {
			u.head = u.head[:0]
			u.dispatch()
			return
		}
		u.decide()
	}
	v := bytes.TrimPrefix(bytes.TrimSuffix(u.val, []byte("\r")), []byte(" "))
	switch u.field {
	case 'e':
		u.event = string(v)
		u.r.output = u.r.output || outputEvent(u.event)
	case 'd':
		if !u.over {
			if u.lines > 0 {
				u.data = append(u.data, '\n')
			}
			u.data = append(u.data, v...)
			u.lines++
		}
	}
	u.field, u.val = 0, u.val[:0]
}

// keep says whether the event being read can carry usage. The event name comes
// before its data, so an event this passes over is never buffered.
func (u *usageScan) keep() bool {
	switch u.surface {
	case surfaceMessages:
		switch u.event {
		case "", "message_start", "message_delta", "error":
			return true
		}
		return false
	case surfaceResponses:
		switch u.event {
		case "", "response.completed", "response.incomplete", "response.failed", "error":
			return true
		}
		return false
	}
	// A chat stream names no events, and its chunks are small.
	return true
}

func (u *usageScan) dispatch() {
	if u.lines > 0 && !u.over {
		u.onEvent(u.data)
	}
	u.event, u.data, u.lines, u.over = "", u.data[:0], 0, false
}

func (u *usageScan) onEvent(data []byte) {
	if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
		return
	}
	switch u.surface {
	case surfaceMessages:
		u.r.anthropicEvent(data)
	case surfaceResponses:
		u.r.responsesEvent(data)
	case surfaceChat:
		u.r.chatChunk(data)
	default:
		u.r.anyAnswer(data)
	}
}

// done ends the answer: the last event of a stream that did not end in a blank
// line, or the whole of a JSON body.
func (u *usageScan) done() reading {
	switch u.mode {
	case 's':
		if u.field != 0 || len(u.head) > 0 {
			u.endLine()
		}
		u.dispatch()
	case 'j':
		switch u.surface {
		case surfaceMessages:
			u.r.anthropicJSON(u.body)
		case surfaceResponses, surfaceChat, surfaceOther:
			u.r.anyAnswer(u.body)
		}
		u.body = nil
	}
	// Anthropic's 5m/1h split arrives only with message_start. Published only
	// when it still adds up to the final count of cache writes, as tracer does.
	if u.r.CacheWrite5m+u.r.CacheWrite1h != u.r.CacheWrite {
		u.r.CacheWrite5m, u.r.CacheWrite1h = 0, 0
	}
	return u.r
}

// anthropicUsage is Anthropic's usage object. Every field is a pointer, because
// a message_delta repeats the fields it carries and a field it leaves out keeps
// the value message_start gave it.
type anthropicUsage struct {
	InputTokens   *int64 `json:"input_tokens"`
	CacheRead     *int64 `json:"cache_read_input_tokens"`
	CacheCreate   *int64 `json:"cache_creation_input_tokens"`
	CacheCreation *struct {
		E5m int64 `json:"ephemeral_5m_input_tokens"`
		E1h int64 `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
	OutputTokens  *int64 `json:"output_tokens"`
	OutputDetails *struct {
		Thinking *int64 `json:"thinking_tokens"`
	} `json:"output_tokens_details"`
}

// anthropic overwrites what the usage carries, and never adds: message_delta's
// counts are cumulative, and output_tokens climbs across one response.
func (r *reading) anthropic(u *anthropicUsage, final bool) {
	if u == nil {
		return
	}
	set := func(dst *int64, v *int64) {
		if v != nil {
			*dst = *v
		}
	}
	set(&r.Input, u.InputTokens)
	set(&r.CacheRead, u.CacheRead)
	set(&r.CacheWrite, u.CacheCreate)
	set(&r.Output, u.OutputTokens)
	if u.OutputDetails != nil {
		set(&r.Reasoning, u.OutputDetails.Thinking)
	}
	if u.CacheCreation != nil {
		r.CacheWrite5m, r.CacheWrite1h = u.CacheCreation.E5m, u.CacheCreation.E1h
	}
	r.seen = true
	r.final = r.final || final
}

func (r *reading) anthropicEvent(data []byte) {
	var ev struct {
		Type    string `json:"type"`
		Message *struct {
			Model string          `json:"model"`
			Usage *anthropicUsage `json:"usage"`
		} `json:"message"`
		Usage *anthropicUsage `json:"usage"`
	}
	if json.Unmarshal(data, &ev) != nil {
		return
	}
	switch ev.Type {
	case "message_start":
		if ev.Message != nil {
			if ev.Message.Model != "" {
				r.model = ev.Message.Model
			}
			r.anthropic(ev.Message.Usage, false)
		}
	case "message_delta":
		r.anthropic(ev.Usage, true)
	case "error":
		r.failed = true
	default:
		r.output = r.output || outputEvent(ev.Type)
	}
}

func (r *reading) anthropicJSON(data []byte) {
	var m struct {
		Type  string          `json:"type"`
		Model string          `json:"model"`
		Usage *anthropicUsage `json:"usage"`
	}
	if json.Unmarshal(data, &m) != nil || m.Type == "error" {
		return
	}
	if m.Model != "" {
		r.model = m.Model
	}
	r.anthropic(m.Usage, true)
}

// responsesUsage is the Responses API's usage object, which the codex login's
// answers carry too. Its attribution breaks the same total down further, and is
// never read: adding it up would count the prompt twice.
type responsesUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	InputDetails struct {
		Cached     int64 `json:"cached_tokens"`
		CacheWrite int64 `json:"cache_write_tokens"`
	} `json:"input_tokens_details"`
	OutputTokens  int64 `json:"output_tokens"`
	OutputDetails struct {
		Reasoning int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

// responses reads a whole-prompt count, which holds both cache classes.
func (r *reading) responses(u *responsesUsage) {
	r.CacheRead = u.InputDetails.Cached
	r.CacheWrite = u.InputDetails.CacheWrite
	r.CacheWrite5m, r.CacheWrite1h = 0, 0
	r.Input = max(u.InputTokens-r.CacheRead-r.CacheWrite, 0)
	r.Output = u.OutputTokens
	r.Reasoning = u.OutputDetails.Reasoning
	r.seen, r.final = true, true
}

func (r *reading) responsesEvent(data []byte) {
	var ev struct {
		Type     string `json:"type"`
		Response *struct {
			Model string          `json:"model"`
			Usage *responsesUsage `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(data, &ev) != nil {
		return
	}
	switch ev.Type {
	case "response.completed", "response.incomplete", "response.failed":
		r.failed = r.failed || ev.Type == "response.failed"
		if ev.Response != nil {
			if ev.Response.Model != "" {
				r.model = ev.Response.Model
			}
			if ev.Response.Usage != nil {
				r.responses(ev.Response.Usage)
			}
		}
	case "error":
		r.failed = true
	default:
		r.output = r.output || outputEvent(ev.Type)
	}
}

// chatUsage is the chat completions usage object. Fireworks counts its cached
// tokens inside prompt_tokens, as OpenAI does: measured on 2026-11-04, a cache
// hit left prompt_tokens unchanged. It also repeats reasoning_tokens under
// output_tokens_details, which is never read.
type chatUsage struct {
	Prompt        int64 `json:"prompt_tokens"`
	Completion    int64 `json:"completion_tokens"`
	PromptDetails *struct {
		Cached int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionDetails *struct {
		Reasoning int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

func (r *reading) chat(u *chatUsage) {
	var cached, reasoning int64
	if u.PromptDetails != nil {
		cached = u.PromptDetails.Cached
	}
	if u.CompletionDetails != nil {
		reasoning = u.CompletionDetails.Reasoning
	}
	r.CacheRead, r.CacheWrite, r.CacheWrite5m, r.CacheWrite1h = cached, 0, 0, 0
	r.Input = max(u.Prompt-cached, 0)
	r.Output = u.Completion
	r.Reasoning = reasoning
	r.seen, r.final = true, true
}

// chatChunk keeps the last usage that is not null. A stream carries one only
// when the client asked for it with stream_options.include_usage.
func (r *reading) chatChunk(data []byte) {
	var c struct {
		Model   string          `json:"model"`
		Usage   *chatUsage      `json:"usage"`
		Error   json.RawMessage `json:"error"`
		Choices json.RawMessage `json:"choices"`
	}
	if json.Unmarshal(data, &c) != nil {
		return
	}
	if c.Model != "" {
		r.model = c.Model
	}
	if len(c.Error) > 0 && string(c.Error) != "null" {
		r.failed = true
	}
	if c.Usage != nil {
		r.chat(c.Usage)
	}
	r.output = r.output || chatOutput(c.Choices)
}

// chatOutput says whether a chunk's choices hold any of the model's answer. The
// first chunk names only the role, and any field of a delta can come empty. It
// reads every other field as output, reasoning and tool calls included, and is
// decoded apart so a shape it does not know costs no chunk its usage.
func chatOutput(choices json.RawMessage) bool {
	var cs []struct {
		Delta map[string]json.RawMessage `json:"delta"`
	}
	if json.Unmarshal(choices, &cs) != nil {
		return false
	}
	for _, c := range cs {
		for k, v := range c.Delta {
			if k != "role" && string(v) != `""` && string(v) != "null" && string(v) != "[]" {
				return true
			}
		}
	}
	return false
}

// anyAnswer reads a JSON answer, or an event of a surface with no reader of its
// own, by the shape of the usage it carries: a prompt_tokens count is chat's,
// cache_* fields are Anthropic's, and any other input_tokens is the Responses
// API's, nested under "response" when the answer is an event.
func (r *reading) anyAnswer(data []byte) {
	var m struct {
		Model    string          `json:"model"`
		Usage    json.RawMessage `json:"usage"`
		Response *struct {
			Model string          `json:"model"`
			Usage json.RawMessage `json:"usage"`
		} `json:"response"`
		Message *struct {
			Model string          `json:"model"`
			Usage json.RawMessage `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(data, &m) != nil {
		return
	}
	model, usage := m.Model, m.Usage
	for _, nested := range []*struct {
		Model string          `json:"model"`
		Usage json.RawMessage `json:"usage"`
	}{m.Response, m.Message} {
		if (len(usage) == 0 || string(usage) == "null") && nested != nil {
			model, usage = nested.Model, nested.Usage
		}
	}
	if len(usage) == 0 || string(usage) == "null" {
		return
	}
	var keys map[string]json.RawMessage
	if json.Unmarshal(usage, &keys) != nil {
		return
	}
	if model != "" {
		r.model = model
	}
	switch {
	case keys["prompt_tokens"] != nil:
		var u chatUsage
		if json.Unmarshal(usage, &u) == nil {
			r.chat(&u)
		}
	case keys["cache_read_input_tokens"] != nil || keys["cache_creation_input_tokens"] != nil:
		var u anthropicUsage
		if json.Unmarshal(usage, &u) == nil {
			r.anthropic(&u, true)
		}
	case keys["input_tokens"] != nil:
		var u responsesUsage
		if json.Unmarshal(usage, &u) == nil {
			r.responses(&u)
		}
	}
}

// call is what the lender knows of a lent call before its answer: whose it is,
// what kind, and how long the request was. The request body itself is never
// read.
type call struct {
	loan    Loan
	slot    string
	surface surface
	length  int64 // the request's Content-Length, or -1 when it has none
	encoded bool  // the request carries a Content-Encoding, so its length says little
}

func newCall(loan Loan, slot string, sf surface, r *http.Request) call {
	return call{loan: loan, slot: slot, surface: sf, length: r.ContentLength,
		encoded: r.Header.Get("Content-Encoding") != ""}
}

// estimate is what a call whose usage was not read is charged: its request's
// length as a prompt, split between new and cached input the way this slot's
// last read call split, and a flat allowance of output.
func estimate(c call, share float64, largest int64) Tokens {
	var prompt int64
	if c.length < 0 || c.encoded {
		// A chunked request has no length yet, and a compressed one is several
		// times shorter than its prompt.
		prompt = largest
		if prompt == 0 {
			prompt = flatPrompt
		}
	} else {
		prompt = int64(math.Ceil(float64(c.length) / bytesPerToken[c.surface]))
	}
	cached := int64(float64(prompt) * share)
	return Tokens{Input: prompt - cached, CacheRead: cached, Output: estimateOutput}
}

// larger is, for each token type, the larger of what was seen and what was
// estimated.
func larger(seen, est Tokens) Tokens {
	out := seen
	if est.Of(TypeInput) > seen.Of(TypeInput) {
		out.Input, out.CacheWrite, out.CacheWrite5m, out.CacheWrite1h = est.Input, est.CacheWrite, 0, 0
	}
	if est.CacheRead > seen.CacheRead {
		out.CacheRead = est.CacheRead
	}
	if est.Output > seen.Output {
		out.Output = est.Output
	}
	return out
}

// charge decides what a settled call costs (R153e). Only the final usage is a
// reading. Anything less is charged at least the estimate, by type, and counted
// as estimated, because an unread call is never free. A provider's refusal is
// the exception, and costs what it reported.
func charge(r reading, status int, est Tokens) (Tokens, bool) {
	switch {
	case status >= 400:
		// A refusal carries no usage, as a rule, and costs nothing.
		if r.seen {
			return r.Tokens, false
		}
		return Tokens{}, false
	case r.final:
		return r.Tokens, false
	case r.failed && !r.output:
		// A refusal inside a 200, which is how a rate limit can arrive, and an
		// agent retries it. It costs what a 429 does, or message_start's prompt
		// counts on Anthropic, whether or not the client stayed to the end.
		return r.Tokens, r.seen
	case !r.seen:
		// Completed with no usage recognised, or the client went away before
		// any arrived.
		return est, true
	}
	// Usage began and its final count never came: a stream cut short. An agent
	// that aborts a Responses stream just before response.completed already has
	// every delta, so this must not come out free.
	return larger(r.Tokens, est), true
}

// meteredBody replaces a lent answer's body. It feeds the scanner as the
// reverse proxy copies, and settles the call exactly once: at EOF when the
// answer completes, or in Close when the client went away first.
type meteredBody struct {
	io.ReadCloser
	scan   *usageScan
	once   sync.Once
	settle func(r reading)
}

func (b *meteredBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.scan.Write(p[:n])
	}
	if err == io.EOF {
		b.finish()
	}
	return n, err
}

// Close settles too. When a client goes away mid-copy, the reverse proxy
// closes the body and then panics with http.ErrAbortHandler, so nothing placed
// after ServeHTTP can be relied on to run.
func (b *meteredBody) Close() error {
	err := b.ReadCloser.Close()
	b.finish()
	return err
}

func (b *meteredBody) finish() {
	b.once.Do(func() { b.settle(b.scan.done()) })
}
