package lend

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cassetteDir holds the sessions the replay tier serves. Every answer in them
// is a golden reading for the meter: what the provider counted is in the
// answer, so what the meter reads must be exactly that.
const cassetteDir = "../../test/cassettes"

type cassetteCall struct {
	Seq     int    `json:"seq"`
	Method  string `json:"method"`
	Path    string `json:"path"`
	Surface string `json:"surface"`
	Status  int    `json:"status"`
}

func cassetteIndex(t *testing.T, cassette string) []cassetteCall {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(cassetteDir, cassette, "index.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var out []cassetteCall
	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		var c cassetteCall
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			t.Fatalf("%s: %v", cassette, err)
		}
		out = append(out, c)
	}
	return out
}

func cassetteFile(t *testing.T, cassette, dir string, seq int) []byte {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(cassetteDir, cassette, dir, fmt.Sprintf("%04d.*", seq)))
	if len(m) != 1 {
		t.Fatalf("%s/%s/%04d: want one file, found %v", cassette, dir, seq, m)
	}
	data, err := os.ReadFile(m[0])
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func pathOnly(p string) string { q, _, _ := strings.Cut(p, "?"); return q }

// scanAll reads an answer in chunks of n bytes, which is how a stream arrives:
// split anywhere, including inside a field name.
func scanAll(sf surface, body []byte, n int) reading {
	u := newScan(sf)
	for i := 0; i < len(body); i += n {
		u.Write(body[i:min(i+n, len(body))])
	}
	return u.done()
}

// upTo is a stream cut off just before an event, as an abort cuts one.
func upTo(t *testing.T, body []byte, marker string) []byte {
	t.Helper()
	i := bytes.Index(body, []byte(marker))
	if i < 0 {
		t.Fatalf("the stream holds no %q", marker)
	}
	return body[:i]
}

// cassetteReadings are the counts in each recorded answer, worked out by hand
// from the providers' own usage objects by tracer's rules. The traps each one
// holds: codex-login carries an attribution breakdown that sums to the same
// prompt again; claude-login writes its cache for an hour; Anthropic's
// message_delta repeats message_start's counts; Fireworks reports reasoning
// twice and both times inside its completion count.
var cassetteReadings = []struct {
	cassette string
	seq      int
	model    string
	want     Tokens
}{
	{"claude-login-lent", 2, "claude-opus-5", Tokens{Input: 1168, Output: 13}},
	{"claude-login-lent", 3, "claude-opus-5", Tokens{Input: 2, CacheWrite: 20409, CacheWrite1h: 20409, Output: 4}},
	{"claude-login-shared", 2, "claude-opus-5", Tokens{Input: 1168, Output: 13}},
	{"claude-login-shared", 3, "claude-opus-5", Tokens{Input: 2, CacheWrite: 4966, CacheWrite1h: 4966, CacheRead: 15272, Output: 4}},
	{"codex-login-lent", 3, "gpt-5.6-sol", Tokens{Input: 6583, CacheRead: 1920, Output: 17}},
	{"codex-login-lent", 4, "gpt-5.6-sol", Tokens{Input: 1976, CacheRead: 8576, Output: 5}},
	{"codex-login-shared", 3, "gpt-5.6-sol", Tokens{Input: 8503, Output: 17}},
	{"codex-login-shared", 4, "gpt-5.6-sol", Tokens{Input: 1976, CacheRead: 8576, Output: 5}},
	{"opencode-fireworks-lent", 1, "accounts/fireworks/models/kimi-k3", Tokens{Input: 6544, Output: 46, Reasoning: 31}},
	{"opencode-fireworks-lent", 2, "accounts/fireworks/models/kimi-k3", Tokens{Input: 468, Output: 99, Reasoning: 83}},
	{"opencode-fireworks-shared", 1, "accounts/fireworks/models/kimi-k3", Tokens{Input: 468, Output: 58, Reasoning: 42}},
	{"opencode-fireworks-shared", 2, "accounts/fireworks/models/kimi-k3", Tokens{Input: 6544, Output: 49, Reasoning: 34}},
}

// TestTheCassettesReadAsTheProviderCounted: every model call in every cassette
// reads exactly what its provider reported, however the stream is split. Each
// mistake this guards against counts tokens twice: adding a streamed partial,
// adding cached tokens to a prompt that already holds them, or adding reasoning
// to an output that already holds it.
func TestTheCassettesReadAsTheProviderCounted(t *testing.T) {
	want := map[string]int{}
	for i, c := range cassetteReadings {
		want[fmt.Sprintf("%s/%d", c.cassette, c.seq)] = i
	}
	dirs, _ := filepath.Glob(filepath.Join(cassetteDir, "*", "index.jsonl"))
	if len(dirs) == 0 {
		t.Fatal("no cassettes found")
	}
	for _, d := range dirs {
		cassette := filepath.Base(filepath.Dir(d))
		for _, c := range cassetteIndex(t, cassette) {
			sf := surfaceOf(c.Method, pathOnly(c.Path))
			if sf == surfaceFree {
				continue
			}
			if string(sf) != c.Surface {
				t.Errorf("%s/%d: %s %s reads as %q, and cs-vcr classed it %q", cassette, c.Seq, c.Method, c.Path, sf, c.Surface)
			}
			body := cassetteFile(t, cassette, "resp", c.Seq)
			if c.Status >= 400 {
				// A refusal carries no usage, and is charged nothing.
				got, est := charge(scanAll(sf, body, len(body)), c.Status, Tokens{Output: 1})
				if got != (Tokens{}) || est {
					t.Errorf("%s/%d: a %d was charged %+v (estimated %v)", cassette, c.Seq, c.Status, got, est)
				}
				continue
			}
			i, ok := want[fmt.Sprintf("%s/%d", cassette, c.Seq)]
			if !ok {
				t.Errorf("%s/%d is a model call with no expected reading: add one to cassetteReadings", cassette, c.Seq)
				continue
			}
			exp := cassetteReadings[i]
			for _, n := range []int{1, 7, 512, len(body)} {
				r := scanAll(sf, body, n)
				if !r.final || r.Tokens != exp.want || r.model != exp.model {
					t.Errorf("%s/%d in %d-byte chunks: read %+v from %q (final %v), want %+v from %q",
						cassette, c.Seq, n, r.Tokens, r.model, r.final, exp.want, exp.model)
				}
			}
		}
	}
}

// TestEveryTypeIsASumOfBuckets: new input holds cache writes, because they are
// prompt tokens the provider processed without a hit. Reasoning stays inside
// output.
func TestEveryTypeIsASumOfBuckets(t *testing.T) {
	tk := Tokens{Input: 2, CacheWrite: 20409, CacheRead: 15272, Output: 377, Reasoning: 366}
	for typ, want := range map[string]int64{TypeInput: 20411, TypeCached: 15272, TypeOutput: 377} {
		if got := tk.Of(typ); got != want {
			t.Errorf("%s = %d, want %d", typ, got, want)
		}
	}
}

// TestShapesNoCassetteHolds are fixtures made by hand for what the current
// cassettes cannot show: a Fireworks cache hit (measured on 2026-11-04), an
// Anthropic reply that thought, and an OpenAI prompt that wrote the cache (both
// from cassettes since dropped, whose keys expired).
func TestShapesNoCassetteHolds(t *testing.T) {
	for _, c := range []struct {
		name string
		sf   surface
		body string
		want Tokens
	}{
		{"a Fireworks cache hit is inside prompt_tokens", surfaceChat,
			`data: {"model":"accounts/fireworks/models/kimi-k3","choices":[],"usage":{"prompt_tokens":8851,"total_tokens":8883,` +
				`"completion_tokens":32,"prompt_tokens_details":{"cached_tokens":8850},"completion_tokens_details":{"reasoning_tokens":32},` +
				`"output_tokens_details":{"reasoning_tokens":32}}}` + "\n\ndata: [DONE]\n\n",
			Tokens{Input: 1, CacheRead: 8850, Output: 32, Reasoning: 32}},
		{"Anthropic thinking is part of output", surfaceMessages,
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-opus-5\",\"usage\":{\"input_tokens\":10," +
				"\"cache_read_input_tokens\":0,\"cache_creation_input_tokens\":0,\"output_tokens\":1}}}\n\n" +
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":377,\"output_tokens_details\":{\"thinking_tokens\":366}}}\n\n",
			Tokens{Input: 10, Output: 377, Reasoning: 366}},
		{"an OpenAI prompt holds its cache writes", surfaceResponses,
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-5.6\",\"usage\":{\"input_tokens\":9322," +
				"\"input_tokens_details\":{\"cached_tokens\":0,\"cache_write_tokens\":9319},\"output_tokens\":5,\"output_tokens_details\":{\"reasoning_tokens\":0}," +
				"\"total_tokens\":9327}}}\n\n",
			Tokens{Input: 3, CacheWrite: 9319, Output: 5}},
		{"a JSON answer from the Messages API", surfaceMessages,
			`{"type":"message","model":"claude-haiku-4-5","usage":{"input_tokens":40,"cache_read_input_tokens":4000,"output_tokens":9}}`,
			Tokens{Input: 40, CacheRead: 4000, Output: 9}},
		{"a JSON answer from the Responses API", surfaceResponses,
			`{"object":"response","model":"gpt-5.6","usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":60},"output_tokens":7}}`,
			Tokens{Input: 40, CacheRead: 60, Output: 7}},
		{"a CRLF-framed stream with multi-line data", surfaceChat,
			"data: {\"model\":\"m\",\r\ndata: \"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2}}\r\n\r\n",
			Tokens{Input: 5, Output: 2}},
	} {
		r := scanAll(c.sf, []byte(c.body), 3)
		if !r.final || r.Tokens != c.want {
			t.Errorf("%s: read %+v (final %v), want %+v", c.name, r.Tokens, r.final, c.want)
		}
	}
}

// TestEveryEstimateErrsHigh: for every request in the cassettes, the prompt an
// estimate charges is at least the prompt its provider counted. A low estimate
// lets a sandbox run past its line on calls the meter could not read.
func TestEveryEstimateErrsHigh(t *testing.T) {
	dirs, _ := filepath.Glob(filepath.Join(cassetteDir, "*", "index.jsonl"))
	checked := 0
	for _, d := range dirs {
		cassette := filepath.Base(filepath.Dir(d))
		for _, c := range cassetteIndex(t, cassette) {
			sf := surfaceOf(c.Method, pathOnly(c.Path))
			if sf == surfaceFree || c.Status >= 400 {
				continue
			}
			resp := cassetteFile(t, cassette, "resp", c.Seq)
			r := scanAll(sf, resp, len(resp))
			// cs-vcr keeps its own indented copy of a request, and the wire
			// carries it compact.
			var compact bytes.Buffer
			if err := json.Compact(&compact, cassetteFile(t, cassette, "req", c.Seq)); err != nil {
				t.Fatalf("%s/%d: %v", cassette, c.Seq, err)
			}
			est := estimate(call{surface: sf, length: int64(compact.Len())}, 0, 0)
			if est.prompt() < r.prompt() {
				t.Errorf("%s/%d: %d request bytes estimate %d prompt tokens, and the provider counted %d (%.2f bytes a token, divisor %g)",
					cassette, c.Seq, compact.Len(), est.prompt(), r.prompt(), float64(compact.Len())/float64(r.prompt()), bytesPerToken[sf])
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no request was checked")
	}
}

// TestTheFreeListIsExact: only these cost nothing. Anything else a slot fronts is
// metered, by its reader or by estimate, so a path this build has never seen is
// still forwarded and still counted.
func TestTheFreeListIsExact(t *testing.T) {
	for _, c := range []struct {
		method, path string
		want         surface
	}{
		{"POST", "/v1/messages", surfaceMessages},
		{"POST", "/messages", surfaceMessages}, // OpenCode's Anthropic client versions its base URL
		{"POST", "/responses", surfaceResponses},
		{"POST", "/v1/responses", surfaceResponses},
		{"POST", "/v1/chat/completions", surfaceChat},
		{"POST", "/v1/messages/count_tokens", surfaceFree},
		{"GET", "/models", surfaceFree},
		{"HEAD", "/v1/messages", surfaceFree},
		{"POST", "/v1/messages/count_tokens/", surfaceOther},
		{"POST", "/responses/compact", surfaceOther},
		{"POST", "/v1/embeddings", surfaceOther},
		{"POST", "/v2beta/messages", surfaceOther},
	} {
		if got := surfaceOf(c.method, c.path); got != c.want {
			t.Errorf("%s %s = %q, want %q", c.method, c.path, got, c.want)
		}
	}
}

// TestAnUnreadCallIsNeverFree: a call whose usage never arrived is charged at
// least its estimate, by type, and counted as estimated. The estimate comes from
// the request alone: a Responses stream echoes its instructions and tools, so its
// size says nothing about what it cost.
func TestAnUnreadCallIsNeverFree(t *testing.T) {
	body := cassetteFile(t, "codex-login-lent", "resp", 3)
	cut := upTo(t, body, "event: response.completed")
	c := call{surface: surfaceResponses, length: 45000}
	est := estimate(c, 0.5, 0)
	if want := int64(math.Ceil(45000 / 4.5)); est.prompt() != want || est.Output != estimateOutput {
		t.Fatalf("estimate = %+v, want a prompt of %d and %d output", est, want, estimateOutput)
	}
	if est.CacheRead != est.prompt()/2 {
		t.Errorf("the estimate split %+v, want half of the prompt cached, as the slot's last call was", est)
	}
	for _, cc := range []struct {
		name string
		r    reading
	}{
		{"the client went away before response.completed", scanAll(surfaceResponses, cut, 512)},
		{"the stream completed with no usage recognised", scanAll(surfaceResponses, []byte("event: x\ndata: {}\n\n"), 512)},
		{"the answer was not a stream or JSON", scanAll(surfaceOther, []byte("<html>"), 512)},
	} {
		got, estimated := charge(cc.r, 200, est)
		if !estimated || got != est {
			t.Errorf("%s: charged %+v (estimated %v), want the estimate %+v", cc.name, got, estimated, est)
		}
	}

	// Cut after message_start: the prompt counts are already in, and each type
	// is charged the larger of what was seen and the estimate.
	anth := cassetteFile(t, "claude-login-shared", "resp", 3)
	seen := scanAll(surfaceMessages, upTo(t, anth, "event: message_delta"), 512)
	if !seen.seen || seen.final {
		t.Fatalf("a stream cut before message_delta read %+v", seen)
	}
	small := Tokens{Input: 10, Output: estimateOutput}
	got, estimated := charge(seen, 200, small)
	if !estimated || got.CacheRead != 15272 || got.Of(TypeInput) != 4968 || got.Output != estimateOutput {
		t.Errorf("an abort after message_start charged %+v, want message_start's prompt and the output estimate", got)
	}
}

// TestAStreamThatReportedItsOwnErrorIsChargedWhatItReported: Anthropic sends an
// error event inside a 200, after message_start has counted the prompt.
func TestAStreamThatReportedItsOwnErrorIsChargedWhatItReported(t *testing.T) {
	body := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-opus-5\",\"usage\":{\"input_tokens\":7," +
		"\"cache_read_input_tokens\":900,\"output_tokens\":1}}}\n\nevent: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\"}}\n\n"
	r := scanAll(surfaceMessages, []byte(body), 5)
	got, estimated := charge(r, 200, Tokens{Input: 99999})
	if !r.failed || !estimated || got != (Tokens{Input: 7, CacheRead: 900, Output: 1}) {
		t.Errorf("charged %+v (estimated %v, failed %v), want message_start's counts", got, estimated, r.failed)
	}
}

// TestAFailedStreamIsChargedByWhetherTheModelAnswered: once output has begun, a
// stream that fails with no final usage may have cost anything, and is estimated.
// One that fails first is the provider's refusal, and costs what it reported. The
// recorded streams are here because their own event names decide it.
func TestAFailedStreamIsChargedByWhetherTheModelAnswered(t *testing.T) {
	const (
		failed     = "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"r\",\"usage\":null}}\n\n"
		anError    = "event: error\ndata: {\"type\":\"error\",\"message\":\"boom\"}\n\n"
		overloaded = "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\"}}\n\n"
		chatError  = "data: {\"error\":{\"message\":\"boom\"}}\n\n"
		bookkeep   = "event: response.created\ndata: {\"type\":\"response.created\"}\n\nevent: response.in_progress\ndata: {\"type\":\"response.in_progress\"}\n\n"
		delta      = "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n"
		bareDelta  = "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n"
		start      = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"m\",\"usage\":{\"input_tokens\":7,\"cache_read_input_tokens\":900,\"output_tokens\":1}}}\n\n"
		ping       = "event: ping\ndata: {\"type\":\"ping\"}\n\n"
		block      = "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"hi\"}}\n\n"
	)
	throttle, err := os.ReadFile("../../test/faults/openai-rate-limit.sse")
	if err != nil {
		t.Fatal(err)
	}
	codex := upTo(t, cassetteFile(t, "codex-login-lent", "resp", 3), "event: response.completed")
	codexSetup := upTo(t, codex, "event: response.output_item.added")
	claude := upTo(t, cassetteFile(t, "claude-login-shared", "resp", 3), "event: message_delta")
	claudeSetup := upTo(t, claude, "event: content_block_start")
	chat := bytes.SplitAfter(cassetteFile(t, "opencode-fireworks-lent", "resp", 1), []byte("\n\n"))

	est := Tokens{Input: 1000, Output: estimateOutput}
	reported := Tokens{Input: 7, CacheRead: 900, Output: 1}
	for _, tc := range []struct {
		name      string
		sf        surface
		body      string
		want      Tokens
		estimated bool
	}{
		{"a rate limit in a Responses stream", surfaceResponses, string(throttle), Tokens{}, false},
		{"a Responses stream failing during its bookkeeping", surfaceResponses, bookkeep + failed, Tokens{}, false},
		{"the recorded Responses bookkeeping, then a failure", surfaceResponses, string(codexSetup) + failed, Tokens{}, false},
		{"a Responses error event before any output", surfaceResponses, bookkeep + anError, Tokens{}, false},
		{"Responses output, then a failure", surfaceResponses, bookkeep + delta + failed, est, true},
		{"Responses output, then an error event", surfaceResponses, bookkeep + delta + anError, est, true},
		{"Responses output with no event names, then a failure", surfaceResponses, bareDelta + "data: {\"type\":\"response.failed\"}\n\n", est, true},
		{"the recorded Responses stream, then a failure", surfaceResponses, string(codex) + failed, est, true},
		{"Anthropic refusing before message_start", surfaceMessages, overloaded, Tokens{}, false},
		{"Anthropic refusing after message_start", surfaceMessages, start + ping + overloaded, reported, true},
		{"Anthropic output, then an error", surfaceMessages, start + block + overloaded,
			Tokens{Input: 1000, CacheRead: 900, Output: estimateOutput}, true},
		{"chat refusing at once", surfaceChat, chatError, Tokens{}, false},
		{"the recorded chat role chunk, then an error", surfaceChat, string(chat[0]) + chatError, Tokens{}, false},
		{"an empty chat delta, then an error", surfaceChat, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"\",\"tool_calls\":null}}]}\n\n" + chatError, Tokens{}, false},
		{"recorded chat reasoning, then an error", surfaceChat, string(chat[0]) + string(chat[1]) + chatError, est, true},
		{"chat content, then an error", surfaceChat, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" + chatError, est, true},
		{"a chat tool call, then an error", surfaceChat, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0}]}}]}\n\n" + chatError, est, true},
	} {
		// Split anywhere, as a stream arrives. Whether the client stayed to the
		// end makes no difference: the call is settled from these bytes alone.
		got, estimated := charge(scanAll(tc.sf, []byte(tc.body), 5), 200, est)
		if got != tc.want || estimated != tc.estimated {
			t.Errorf("%s: charged %+v (estimated %v), want %+v (estimated %v)", tc.name, got, estimated, tc.want, tc.estimated)
		}
	}

	// A recorded Anthropic stream keeps the prompt counts it reported.
	for _, tc := range []struct {
		name   string
		body   []byte
		output int64
	}{
		{"its bookkeeping", claudeSetup, 1},
		{"its output", claude, estimateOutput},
	} {
		got, estimated := charge(scanAll(surfaceMessages, append(bytes.Clone(tc.body), overloaded...), 5), 200, est)
		if !estimated || got.CacheRead != 15272 || got.Of(TypeInput) != 4968 || got.Output != tc.output {
			t.Errorf("a recorded Anthropic stream failing after %s charged %+v, want its prompt counts and %d output", tc.name, got, tc.output)
		}
	}
}

// TestAChatChunkOfAnUnknownShapeKeepsItsUsage: reading the answer's start must not
// cost a chunk the usage it carries.
func TestAChatChunkOfAnUnknownShapeKeepsItsUsage(t *testing.T) {
	r := scanAll(surfaceChat, []byte("data: {\"choices\":\"none\",\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2}}\n\n"), 5)
	if !r.final || r.Input != 10 || r.Output != 2 || r.output {
		t.Errorf("read %+v (final %v, output %v), want the chunk's usage and no output", r.Tokens, r.final, r.output)
	}
}

// TestAnEventTooLargeToReadIsEstimated: the scanner buffers one usage-bearing
// event at most, under a cap, and an event past the cap is estimated rather than
// read. Events that carry no usage are passed over without being buffered.
func TestAnEventTooLargeToReadIsEstimated(t *testing.T) {
	u := newScan(surfaceResponses)
	u.Write([]byte("event: response.output_text.delta\ndata: "))
	big := bytes.Repeat([]byte("x"), 1<<20)
	for range 20 {
		u.Write(big)
	}
	if len(u.val) != 0 || len(u.data) != 0 {
		t.Fatalf("an event with no usage was buffered: %d bytes", len(u.val)+len(u.data))
	}
	u.Write([]byte("\n\nevent: response.completed\ndata: {\"response\":{\"usage\":{\"input_tokens\":1}},\"pad\":\""))
	for range 17 {
		u.Write(big)
	}
	u.Write([]byte("\"}\n\n"))
	r := u.done()
	if r.final || !r.over {
		t.Errorf("an event past the cap read as %+v (final %v, over %v)", r.Tokens, r.final, r.over)
	}
}
