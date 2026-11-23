// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const roleKey = "sk-ant-role-key-for-tests"

var testPrices = map[string]Price{
	"claude-opus-5-5":   {Input: 4, Output: 20, CacheRead: 0.2, CacheWrite5m: 5, CacheWrite1h: 8},
	"claude-sonnet-5-5": {Input: 2, Output: 10, CacheRead: 0.1, CacheWrite5m: 2.5, CacheWrite1h: 4},
	"claude-haiku-5-5": {Input: 0.1, Output: 0.5, CacheRead: 0.01, CacheWrite5m: 0.125, CacheWrite1h: 0.2,
		LongContext: &LongContextPrice{OverPromptTokens: 100000, Input: 0.5, Output: 2.5, CacheRead: 0.05, CacheWrite5m: 0.625, CacheWrite1h: 1}},
}

type harness struct {
	gate     *Gate
	role     *Role
	logs     *bytes.Buffer
	upstream *httptest.Server
	calls    atomic.Int32
	lastReq  atomic.Pointer[http.Request]
	lastBody atomic.Pointer[[]byte]
}

func newHarness(t *testing.T, upstream http.HandlerFunc, tweak func(*Config)) *harness {
	t.Helper()
	h := &harness{logs: &bytes.Buffer{}}
	h.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.calls.Add(1)
		b, _ := io.ReadAll(r.Body)
		h.lastReq.Store(r.Clone(r.Context()))
		h.lastBody.Store(&b)
		upstream(w, r)
	}))
	t.Cleanup(h.upstream.Close)
	cfg := &Config{
		Upstream: h.upstream.URL, Prices: testPrices,
		Roles: []Role{{
			Name: "watch-officer", Listen: ":0", KeyFile: "unused",
			Models:              []string{"claude-opus-5-5", "claude-sonnet-5-5"},
			MaxTokensPerRequest: 32000, HourlyUSD: 1, DailyUSD: 5,
			AllowedBetas: []string{},
		}},
	}
	if tweak != nil {
		tweak(cfg)
	}
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	cfg.Roles[0].key = roleKey
	b, _ := newBudgets(cfg.StateFile)
	h.gate = &Gate{cfg: cfg, budgets: b, metrics: newMetrics(), client: h.upstream.Client(),
		log: slog.New(slog.NewJSONHandler(h.logs, nil)), now: time.Now}
	h.role = &cfg.Roles[0]
	return h
}

func (h *harness) do(t *testing.T, method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	h.gate.handler(h.role).ServeHTTP(rr, req)
	return rr
}

func (h *harness) lastAudit(t *testing.T) map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(h.logs.String()), "\n")
	var m map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &m); err != nil {
		t.Fatalf("audit line not JSON: %v", err)
	}
	return m
}

const okJSON = `{"id":"msg_1","type":"message","model":"claude-opus-5-5","content":[{"type":"text","text":"hi"}],
"stop_reason":"end_turn","usage":{"input_tokens":1000,"output_tokens":500,"cache_read_input_tokens":10000,"cache_creation_input_tokens":2000}}`

func jsonUpstream(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Request-Id", "req_upstream_1")
		w.Header().Set("Set-Cookie", "tracking=1")
		_, _ = io.WriteString(w, body)
	}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestKeyInjectionAndHeaderHandling(t *testing.T) {
	h := newHarness(t, jsonUpstream(okJSON), func(c *Config) { c.Roles[0].AllowedBetas = []string{"a-beta", "b-beta"} })
	rr := h.do(t, "POST", "/v1/messages",
		`{"model":"claude-opus-5-5","max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{
			"X-Api-Key": "sk-ant-agent-should-not-have-this", "Authorization": "Bearer stolen",
			"Anthropic-Version": "2026-10-21", "Anthropic-Beta": "a-beta, b-beta", "Content-Type": "application/json",
			"X-Forwarded-For": "10.0.0.9",
		})
	if rr.Code != 200 {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	up := h.lastReq.Load()
	if got := up.Header.Get("X-Api-Key"); got != roleKey {
		t.Errorf("upstream x-api-key = %q, want role key", got)
	}
	if up.Header.Get("Authorization") != "" || up.Header.Get("X-Forwarded-For") != "" {
		t.Errorf("credential/forwarding headers leaked upstream: %v", up.Header)
	}
	if up.Header.Get("Anthropic-Version") != "2026-10-21" {
		t.Errorf("anthropic-version not passed through")
	}
	if got := up.Header.Values("Anthropic-Beta"); len(got) != 2 || got[0] != "a-beta" || got[1] != "b-beta" {
		t.Errorf("betas = %v", got)
	}
	if rr.Header().Get("Set-Cookie") != "" {
		t.Errorf("Set-Cookie passed back to the agent")
	}
	if rr.Body.String() != okJSON {
		t.Errorf("response body altered")
	}
	// cost: 1000*4 + 500*20 + 10000*0.2 + 2000*5 (all /1e6) = 0.026
	a := h.lastAudit(t)
	if !near(a["cost_usd"].(float64), 0.026) || a["upstream_request_id"] != "req_upstream_1" || a["role"] != "watch-officer" {
		t.Errorf("audit = %v", a)
	}
	if strings.Contains(h.logs.String(), "hi\"") || strings.Contains(h.logs.String(), roleKey) {
		t.Errorf("audit log contains prompt content or the key")
	}
}

func TestRejectionsNeverReachUpstream(t *testing.T) {
	cases := []struct {
		name, method, path, body string
		hdr                      map[string]string
		status                   int
		reason                   string
	}{
		{"batches endpoint", "POST", "/v1/messages/batches", `{}`, nil, 403, "path_not_allowed"},
		{"files endpoint", "POST", "/v1/files", `{}`, nil, 403, "path_not_allowed"},
		{"admin endpoint", "GET", "/v1/organizations/me", ``, nil, 403, "path_not_allowed"},
		{"models subpath traversal", "GET", "/v1/models/x/y", ``, nil, 403, "path_not_allowed"},
		{"wrong method", "GET", "/v1/messages", ``, nil, 405, "method_not_allowed"},
		{"bad json", "POST", "/v1/messages", `{not json`, nil, 400, "invalid_json"},
		{"model not allowed", "POST", "/v1/messages", `{"model":"claude-fable-5-2","max_tokens":10}`, nil, 403, "model_not_allowed"},
		{"max_tokens over cap", "POST", "/v1/messages", `{"model":"claude-opus-5-5","max_tokens":64000}`, nil, 400, "max_tokens_over_cap"},
		{"max_tokens missing", "POST", "/v1/messages", `{"model":"claude-opus-5-5"}`, nil, 400, "max_tokens_over_cap"},
		{"web search tool", "POST", "/v1/messages", `{"model":"claude-opus-5-5","max_tokens":10,"tools":[{"type":"web_search_20260209","name":"web_search"}]}`, nil, 403, "server_tool_not_allowed"},
		{"web fetch tool", "POST", "/v1/messages", `{"model":"claude-opus-5-5","max_tokens":10,"tools":[{"type":"web_fetch_20260209","name":"web_fetch"}]}`, nil, 403, "server_tool_not_allowed"},
		{"code execution tool", "POST", "/v1/messages", `{"model":"claude-opus-5-5","max_tokens":10,"tools":[{"type":"code_execution_20260521","name":"code_execution"}]}`, nil, 403, "server_tool_not_allowed"},
		{"mcp toolset", "POST", "/v1/messages", `{"model":"claude-opus-5-5","max_tokens":10,"tools":[{"type":"mcp_toolset","mcp_server_name":"x"}]}`, nil, 403, "server_tool_not_allowed"},
		{"mcp servers", "POST", "/v1/messages", `{"model":"claude-opus-5-5","max_tokens":10,"mcp_servers":[{"type":"url","url":"https://evil.example","name":"x"}]}`, nil, 403, "mcp_servers_not_allowed"},
		{"container", "POST", "/v1/messages", `{"model":"claude-opus-5-5","max_tokens":10,"container":{"skills":[]}}`, nil, 403, "container_not_allowed"},
		{"default fallbacks", "POST", "/v1/messages", `{"model":"claude-opus-5-5","max_tokens":10,"fallbacks":"default"}`, nil, 403, "fallbacks_not_allowed"},
		{"fallback model not allowed", "POST", "/v1/messages", `{"model":"claude-opus-5-5","max_tokens":10,"fallbacks":[{"model":"claude-fable-5-2"}]}`, nil, 403, "fallback_model_not_allowed"},
		{"count_tokens model not allowed", "POST", "/v1/messages/count_tokens", `{"model":"claude-fable-5-2"}`, nil, 403, "model_not_allowed"},
		// Finding 2: allowlist, not blocklist. Unknown and client-side Anthropic types need opting in.
		{"unknown tool type", "POST", "/v1/messages", `{"model":"claude-opus-5-5","max_tokens":10,"tools":[{"type":"some_new_server_tool_20270101","name":"x"}]}`, nil, 403, "tool_type_not_allowed"},
		{"anthropic tool search", "POST", "/v1/messages", `{"model":"claude-opus-5-5","max_tokens":10,"tools":[{"type":"tool_search_tool_regex_20251119","name":"tool_search_tool_regex"}]}`, nil, 403, "tool_type_not_allowed"},
		{"bash tool not opted in", "POST", "/v1/messages", `{"model":"claude-opus-5-5","max_tokens":10,"tools":[{"type":"bash_20250124","name":"bash"}]}`, nil, 403, "tool_type_not_allowed"},
		{"unlisted beta", "POST", "/v1/messages", `{"model":"claude-opus-5-5","max_tokens":10}`, map[string]string{"Anthropic-Beta": "mcp-client-2025-11-20"}, 403, "beta_not_allowed"},
		// Finding 1 (2026-11-22 review): the gate's parser and the API's must not disagree.
		{"duplicate tools", "POST", "/v1/messages", `{"model":"claude-opus-5-5","max_tokens":10,"tools":[{"type":"web_fetch_20260209","name":"web_fetch"}],"tools":[]}`, nil, 400, "ambiguous_json"},
		{"case-variant tools", "POST", "/v1/messages", `{"model":"claude-opus-5-5","max_tokens":10,"tools":[{"type":"web_fetch_20260209","name":"web_fetch"}],"TOOLS":[]}`, nil, 400, "ambiguous_json"},
		{"case-variant model", "POST", "/v1/messages", `{"model":"claude-fable-5-2","max_tokens":10,"Model":"claude-opus-5-5"}`, nil, 400, "ambiguous_json"},
		{"case-variant tool type", "POST", "/v1/messages", `{"model":"claude-opus-5-5","max_tokens":10,"tools":[{"type":"web_fetch_20260209","Type":"custom","name":"x"}]}`, nil, 400, "ambiguous_json"},
		{"duplicate max_tokens", "POST", "/v1/messages", `{"model":"claude-opus-5-5","max_tokens":999999,"max_tokens":10}`, nil, 400, "ambiguous_json"},
		{"kelvin-sign max_tokens", "POST", "/v1/messages", `{"model":"claude-opus-5-5","max_toKens":999999,"max_tokens":10}`, nil, 400, "ambiguous_json"},
		{"case-variant fallback model", "POST", "/v1/messages", `{"model":"claude-opus-5-5","max_tokens":10,"fallbacks":[{"model":"claude-sonnet-5-5","MODEL":"claude-fable-5-2"}]}`, nil, 400, "ambiguous_json"},
		{"duplicate key deep in messages", "POST", "/v1/messages", `{"model":"claude-opus-5-5","max_tokens":10,"messages":[{"role":"user","content":"hi","content":"bye"}]}`, nil, 400, "ambiguous_json"},
		{"invalid utf-8", "POST", "/v1/messages", "{\"model\":\"claude-opus-5-5\",\"max_tokens\":10,\"tools\xff\":[]}", nil, 400, "ambiguous_json"},
		{"ambiguous count_tokens", "POST", "/v1/messages/count_tokens", `{"model":"claude-fable-5-2","Model":"claude-opus-5-5"}`, nil, 400, "ambiguous_json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, jsonUpstream(okJSON), nil)
			rr := h.do(t, tc.method, tc.path, tc.body, tc.hdr)
			if rr.Code != tc.status {
				t.Fatalf("status %d, want %d (%s)", rr.Code, tc.status, rr.Body)
			}
			if h.calls.Load() != 0 {
				t.Fatalf("request reached upstream")
			}
			if a := h.lastAudit(t); a["rejected"] != tc.reason {
				t.Fatalf("reason %v, want %s", a["rejected"], tc.reason)
			}
			var e struct {
				Type  string `json:"type"`
				Error struct{ Type, Message string }
			}
			if json.Unmarshal(rr.Body.Bytes(), &e) != nil || e.Type != "error" || e.Error.Message == "" {
				t.Fatalf("error body not in API shape: %s", rr.Body)
			}
		})
	}
}

func TestAllowedRequestsPass(t *testing.T) {
	for name, tc := range map[string]struct{ method, path, body string }{
		"client tools":      {"POST", "/v1/messages", `{"model":"claude-opus-5-5","max_tokens":10,"tools":[{"name":"cluster_status","input_schema":{"type":"object"}},{"type":"custom","name":"x","input_schema":{"type":"object"}}]}`},
		"allowed fallbacks": {"POST", "/v1/messages", `{"model":"claude-opus-5-5","max_tokens":10,"fallbacks":[{"model":"claude-sonnet-5-5"}]}`},
		"count tokens":      {"POST", "/v1/messages/count_tokens", `{"model":"claude-opus-5-5","messages":[]}`},
		"list models":       {"GET", "/v1/models", ``},
		"get model":         {"GET", "/v1/models/claude-opus-5-5", ``},
		// Case variants are only rejected where the gate reads them: a tool's
		// own schema may use any property names.
		"schema property named Type": {"POST", "/v1/messages", `{"model":"claude-opus-5-5","max_tokens":10,"tools":[{"name":"t","input_schema":{"type":"object","properties":{"Type":{"type":"string"},"MODEL":{"type":"string"}}}}]}`},
		"unicode text":               {"POST", "/v1/messages", `{"model":"claude-opus-5-5","max_tokens":10,"messages":[{"role":"user","content":"héllo ✓ \u00e9"}]}`},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, jsonUpstream(`{"ok":true}`), nil)
			if rr := h.do(t, tc.method, tc.path, tc.body, nil); rr.Code != 200 || h.calls.Load() != 1 {
				t.Fatalf("status %d, calls %d: %s", rr.Code, h.calls.Load(), rr.Body)
			}
		})
	}
}

func TestBetaAllowlistAndToolTypeOptIn(t *testing.T) {
	h := newHarness(t, jsonUpstream(okJSON), func(c *Config) {
		c.Roles[0].AllowedBetas = []string{"compact-2026-01-12"}
		c.Roles[0].AllowedToolTypes = []string{"web_search", "bash_"}
	})
	body := `{"model":"claude-opus-5-5","max_tokens":10}`
	if rr := h.do(t, "POST", "/v1/messages", body, map[string]string{"Anthropic-Beta": "compact-2026-01-12,mcp-client-2025-11-20"}); rr.Code != 403 {
		t.Fatalf("unlisted beta: status %d", rr.Code)
	}
	if rr := h.do(t, "POST", "/v1/messages", body, map[string]string{"Anthropic-Beta": "compact-2026-01-12"}); rr.Code != 200 {
		t.Fatalf("listed beta: status %d", rr.Code)
	}
	ws := `{"model":"claude-opus-5-5","max_tokens":10,"tools":[{"type":"web_search_20260209","name":"web_search"}]}`
	if rr := h.do(t, "POST", "/v1/messages", ws, nil); rr.Code != 200 {
		t.Fatalf("opted-in server tool: status %d", rr.Code)
	}
	bash := `{"model":"claude-opus-5-5","max_tokens":10,"tools":[{"type":"bash_20250124","name":"bash"}]}`
	if rr := h.do(t, "POST", "/v1/messages", bash, nil); rr.Code != 200 {
		t.Fatalf("opted-in client tool type: status %d", rr.Code)
	}
}

// Finding 3 (2026-11-22 review): every role must list its betas; [] means none.
func TestBetasRequiredAndEmptyMeansNone(t *testing.T) {
	cfg := &Config{Prices: testPrices, Roles: []Role{{Name: "r", Listen: ":1", Models: []string{"claude-opus-5-5"}, MaxTokensPerRequest: 1}}}
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "allowed_betas") {
		t.Fatalf("missing allowed_betas: err %v", err)
	}
	h := newHarness(t, jsonUpstream(okJSON), nil) // AllowedBetas: []
	rr := h.do(t, "POST", "/v1/messages", `{"model":"claude-opus-5-5","max_tokens":10}`, map[string]string{"Anthropic-Beta": "interleaved-thinking-2025-05-14"})
	if rr.Code != 403 || h.calls.Load() != 0 {
		t.Fatalf("beta with empty allowlist: status %d, calls %d", rr.Code, h.calls.Load())
	}
	if a := h.lastAudit(t); a["rejected"] != "beta_not_allowed" {
		t.Fatalf("reason %v", a["rejected"])
	}
}

// What Hermes v0.23.3 sends through model-gate (read from its
// agent/anthropic_adapter.py: API key, non-Azure base URL, fast mode off).
func TestHermesRequestShapePasses(t *testing.T) {
	cfg, err := loadConfig("config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, jsonUpstream(okJSON), func(c *Config) {
		c.Roles[0].AllowedBetas = cfg.Roles[0].AllowedBetas
		c.Roles[0].AllowedToolTypes = cfg.Roles[0].AllowedToolTypes
	})
	body := `{"model":"claude-sonnet-5-5","max_tokens":32000,"stream":true,"system":[{"type":"text","text":"You are GIDEON","cache_control":{"type":"ephemeral"}}],` +
		`"tools":[{"name":"tool_search","description":"Find tools","input_schema":{"type":"object","properties":{"query":{"type":"string"}}}},` +
		`{"name":"cluster_status","description":"","input_schema":{"type":"object","properties":{}},"cache_control":{"type":"ephemeral"}}],` +
		`"thinking":{"type":"adaptive","display":"summarized"},"messages":[{"role":"user","content":"status?"}]}`
	rr := h.do(t, "POST", "/v1/messages", body, map[string]string{"Anthropic-Beta": "interleaved-thinking-2025-05-14,fine-grained-tool-streaming-2025-05-14"})
	if rr.Code != 200 || h.calls.Load() != 1 {
		t.Fatalf("status %d, calls %d: %s", rr.Code, h.calls.Load(), rr.Body)
	}
}

func TestStreamingPassThroughAndUsage(t *testing.T) {
	events := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","type":"message","model":"claude-sonnet-5-5","usage":{"input_tokens":2000,"output_tokens":1,"cache_read_input_tokens":50000,"cache_creation_input_tokens":0}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":300}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{},"usage":{"output_tokens":700}}` + "\n\n" +
		"event: message_stop\n" + `data: {"type":"message_stop"}` + "\n\n"
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		for i := 0; i < len(events); i += 7 { // split mid-line on purpose
			_, _ = io.WriteString(w, events[i:min(i+7, len(events))])
			f.Flush()
		}
	}, nil)
	rr := h.do(t, "POST", "/v1/messages", `{"model":"claude-sonnet-5-5","max_tokens":1000,"stream":true}`, nil)
	if rr.Code != 200 || rr.Body.String() != events {
		t.Fatalf("stream altered (status %d)", rr.Code)
	}
	// 2000*2 + 700*10 + 50000*0.1 (all /1e6) = 0.016
	a := h.lastAudit(t)
	if !near(a["cost_usd"].(float64), 0.016) || a["output_tokens"].(float64) != 700 || a["response_model"] != "claude-sonnet-5-5" {
		t.Fatalf("audit = %v", a)
	}
	if _, bad := a["error"]; bad {
		t.Fatalf("unexpected error: %v", a["error"])
	}
}

func TestStreamIncompleteIsFlagged(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"type":"message_start","message":{"model":"claude-opus-5-5","usage":{"input_tokens":10,"output_tokens":1}}}`+"\n\n")
	}, nil)
	h.do(t, "POST", "/v1/messages", `{"model":"claude-opus-5-5","max_tokens":100,"stream":true}`, nil)
	if a := h.lastAudit(t); a["error"] != "stream_incomplete" || a["cost_usd"] == nil {
		t.Fatalf("audit = %v", a)
	}
}

func TestBudgetExhaustionReturns429(t *testing.T) {
	h := newHarness(t, jsonUpstream(okJSON), func(c *Config) { c.Roles[0].HourlyUSD = 0.03 })
	body := `{"model":"claude-opus-5-5","max_tokens":1024}`
	for i, want := range []int{200, 200, 429, 429} { // each costs 0.026
		if rr := h.do(t, "POST", "/v1/messages", body, nil); rr.Code != want {
			t.Fatalf("request %d: status %d, want %d", i, rr.Code, want)
		}
	}
	if h.calls.Load() != 2 {
		t.Fatalf("upstream calls = %d, want 2", h.calls.Load())
	}
	if a := h.lastAudit(t); a["rejected"] != "hourly_budget_exhausted" {
		t.Fatalf("audit = %v", a)
	}
	// count_tokens is free and stays available
	if rr := h.do(t, "POST", "/v1/messages/count_tokens", `{"model":"claude-opus-5-5"}`, nil); rr.Code != 200 {
		t.Fatalf("count_tokens blocked by budget: %d", rr.Code)
	}
}

func TestUpstreamErrorsPassThroughAndCostNothing(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(529)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
	}, nil)
	rr := h.do(t, "POST", "/v1/messages", `{"model":"claude-opus-5-5","max_tokens":10}`, nil)
	if rr.Code != 529 || !strings.Contains(rr.Body.String(), "overloaded_error") {
		t.Fatalf("status %d body %s", rr.Code, rr.Body)
	}
	if s := h.gate.budgets.Snapshot("watch-officer", time.Now()); s.HourUSD != 0 {
		t.Fatalf("error response was charged: %v", s)
	}
}

func TestIterationsAreSummedPerModel(t *testing.T) {
	cfg := &Config{Prices: testPrices}
	u := Usage{InputTokens: 1, OutputTokens: 1, Iterations: []Iteration{
		{Type: "message", Model: "claude-opus-5-5", Usage: Usage{InputTokens: 1000, OutputTokens: 0}},
		{Type: "fallback_message", Model: "claude-sonnet-5-5", Nested: &Usage{InputTokens: 1000, OutputTokens: 100}},
	}}
	got, unpriced := cfg.totalCost("claude-opus-5-5", u)
	if want := (1000*4.0 + 1000*2.0 + 100*10.0) / 1e6; !near(got, want) || unpriced {
		t.Fatalf("cost %g, want %g (unpriced=%v)", got, want, unpriced)
	}
	// an unknown model is charged at the most expensive configured rate
	got, unpriced = cfg.totalCost("claude-unknown", Usage{OutputTokens: 1000})
	if !near(got, 1000*20.0/1e6) || !unpriced {
		t.Fatalf("unknown model cost %g unpriced=%v", got, unpriced)
	}
}

func TestCacheWriteTTLBreakdown(t *testing.T) {
	p := testPrices["claude-opus-5-5"]
	u := Usage{CacheCreationInputTokens: 3000, CacheCreation: &CacheCreation{Ephemeral5m: 1000, Ephemeral1h: 2000}}
	if got, want := costUSD(p, u), (1000*5.0+2000*8.0)/1e6; !near(got, want) {
		t.Fatalf("got %g want %g", got, want)
	}
}

func TestBudgetWindowsRollAndPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	b, _ := newBudgets(path)
	r := &Role{Name: "r", HourlyUSD: 1, DailyUSD: 2}
	t0 := time.Date(2026, 11, 22, 10, 30, 0, 0, time.UTC)
	b.Add("r", 1.5, t0)
	if b.Check(r, t0) != "hourly_budget_exhausted" {
		t.Fatal("hour not exhausted")
	}
	b2, err := newBudgets(path) // restart
	if err != nil {
		t.Fatal(err)
	}
	if b2.Check(r, t0.Add(10*time.Minute)) != "hourly_budget_exhausted" {
		t.Fatal("state not persisted across restart")
	}
	t1 := t0.Add(time.Hour)
	if b2.Check(r, t1) != "" {
		t.Fatal("hour window did not roll")
	}
	b2.Add("r", 0.6, t1)
	if b2.Check(r, t1) != "daily_budget_exhausted" {
		t.Fatal("day budget not enforced")
	}
	if b2.Check(r, t0.Add(24*time.Hour)) != "" {
		t.Fatal("day window did not roll")
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("state file mode %v", fi.Mode())
	}
}

func TestMetricsExposition(t *testing.T) {
	h := newHarness(t, jsonUpstream(okJSON), nil)
	h.do(t, "POST", "/v1/messages", `{"model":"claude-opus-5-5","max_tokens":10}`, nil)
	h.do(t, "POST", "/v1/files", `{}`, nil)
	var buf bytes.Buffer
	h.gate.metrics.Write(&buf, h.gate.cfg, h.gate.budgets, func() RoleSpendFor {
		return func(role string) RoleSpend { return h.gate.budgets.Snapshot(role, time.Now()) }
	})
	out := buf.String()
	for _, want := range []string{
		`model_gate_requests_total{role="watch-officer",outcome="ok",status="2xx"} 1`,
		`model_gate_rejections_total{role="watch-officer",reason="path_not_allowed"} 1`,
		`model_gate_tokens_total{role="watch-officer",model="claude-opus-5-5",kind="output"} 500`,
		`model_gate_cost_usd_total{role="watch-officer"} 0.026`,
		`model_gate_budget_spent_usd{role="watch-officer",window="hour"} 0.026`,
		`model_gate_budget_limit_usd{role="watch-officer",window="day"} 5`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics missing %q\n%s", want, out)
		}
	}
}

func TestConfigValidation(t *testing.T) {
	base := func() *Config {
		return &Config{Prices: testPrices, Roles: []Role{{Name: "a", Listen: ":1", Models: []string{"claude-opus-5-5"}, MaxTokensPerRequest: 10, AllowedBetas: []string{}}}}
	}
	for name, mut := range map[string]func(*Config){
		"plain http upstream": func(c *Config) { c.Upstream = "http://api.anthropic.com" },
		"unpriced model":      func(c *Config) { c.Roles[0].Models = []string{"claude-x"} },
		"no models":           func(c *Config) { c.Roles[0].Models = nil },
		"no max tokens":       func(c *Config) { c.Roles[0].MaxTokensPerRequest = 0 },
		"duplicate listen": func(c *Config) {
			c.Roles = append(c.Roles, Role{Name: "b", Listen: ":1", Models: []string{"claude-opus-5-5"}, MaxTokensPerRequest: 1, AllowedBetas: []string{}})
		},
		"metrics on role port": func(c *Config) { c.MetricsListen = ":1" },
		"no allowed_betas":     func(c *Config) { c.Roles[0].AllowedBetas = nil },
	} {
		c := base()
		mut(c)
		if c.validate() == nil {
			t.Errorf("%s: expected validation error", name)
		}
	}
	if err := base().validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	// keys come from files and must be non-empty
	dir := t.TempDir()
	c := base()
	c.Roles[0].KeyFile = filepath.Join(dir, "key")
	_ = os.WriteFile(c.Roles[0].KeyFile, []byte("\n"), 0o600)
	if c.loadKeys() == nil {
		t.Error("empty key file accepted")
	}
}

func TestClampMaxTokens(t *testing.T) {
	clamp := func(c *Config) { c.Roles[0].ClampMaxTokens = true }

	t.Run("over cap is lowered, not rejected", func(t *testing.T) {
		h := newHarness(t, jsonUpstream(okJSON), clamp)
		rr := h.do(t, "POST", "/v1/messages", `{"model":"claude-opus-5-5","max_tokens":128000,"messages":[{"role":"user","content":"hi"}],"stream":false}`, nil)
		if rr.Code != 200 {
			t.Fatalf("status %d: %s", rr.Code, rr.Body)
		}
		var sent map[string]any
		if err := json.Unmarshal(*h.lastBody.Load(), &sent); err != nil {
			t.Fatal(err)
		}
		if sent["max_tokens"] != float64(32000) || sent["model"] != "claude-opus-5-5" || sent["messages"] == nil {
			t.Fatalf("upstream body: %v", sent)
		}
		a := h.lastAudit(t)
		if a["max_tokens"] != float64(32000) || a["max_tokens_requested"] != float64(128000) {
			t.Fatalf("audit: %v", a)
		}
	})

	t.Run("duplicate key is rejected, not clamped", func(t *testing.T) {
		h := newHarness(t, jsonUpstream(okJSON), clamp)
		rr := h.do(t, "POST", "/v1/messages", `{"model":"claude-opus-5-5","max_tokens":10,"max_tokens":128000}`, nil)
		if rr.Code != 400 || h.calls.Load() != 0 {
			t.Fatalf("status %d, calls %d: %s", rr.Code, h.calls.Load(), rr.Body)
		}
	})

	t.Run("under cap is untouched", func(t *testing.T) {
		h := newHarness(t, jsonUpstream(okJSON), clamp)
		body := `{"model":"claude-opus-5-5","max_tokens":100}`
		if rr := h.do(t, "POST", "/v1/messages", body, nil); rr.Code != 200 {
			t.Fatalf("status %d", rr.Code)
		}
		if got := string(*h.lastBody.Load()); got != body {
			t.Fatalf("body was rewritten: %s", got)
		}
		if _, ok := h.lastAudit(t)["max_tokens_requested"]; ok {
			t.Fatal("max_tokens_requested set without clamping")
		}
	})

	t.Run("missing max_tokens is still rejected", func(t *testing.T) {
		h := newHarness(t, jsonUpstream(okJSON), clamp)
		if rr := h.do(t, "POST", "/v1/messages", `{"model":"claude-opus-5-5"}`, nil); rr.Code != 400 || h.calls.Load() != 0 {
			t.Fatalf("status %d, calls %d", rr.Code, h.calls.Load())
		}
	})

	t.Run("off by default", func(t *testing.T) {
		h := newHarness(t, jsonUpstream(okJSON), nil)
		if rr := h.do(t, "POST", "/v1/messages", `{"model":"claude-opus-5-5","max_tokens":128000}`, nil); rr.Code != 400 || h.calls.Load() != 0 {
			t.Fatalf("status %d, calls %d", rr.Code, h.calls.Load())
		}
	})
}

// Finding 4 (2026-11-22 review): Claude Haiku 5.5 has a second rate card for
// prompts over 100,000 tokens, where the prompt includes cache reads and writes.
func TestLongContextTier(t *testing.T) {
	cfg := &Config{Prices: testPrices}
	for name, tc := range map[string]struct {
		u    Usage
		want float64
	}{
		"at threshold is base tier": {Usage{InputTokens: 100000, OutputTokens: 1000},
			(100000*0.1 + 1000*0.5) / 1e6},
		"one over is long tier": {Usage{InputTokens: 100001, OutputTokens: 1000},
			(100001*0.5 + 1000*2.5) / 1e6},
		"cache reads count toward the threshold": {Usage{InputTokens: 1000, CacheReadInputTokens: 99001, OutputTokens: 10},
			(1000*0.5 + 99001*0.05 + 10*2.5) / 1e6},
		"cache writes count toward the threshold": {Usage{InputTokens: 1000, CacheCreationInputTokens: 99001,
			CacheCreation: &CacheCreation{Ephemeral5m: 99000, Ephemeral1h: 1}},
			(1000*0.5 + 99000*0.625 + 1*1.0) / 1e6},
		"cached prompt under threshold stays base": {Usage{InputTokens: 1000, CacheReadInputTokens: 90000, OutputTokens: 10},
			(1000*0.1 + 90000*0.01 + 10*0.5) / 1e6},
	} {
		t.Run(name, func(t *testing.T) {
			got, unpriced := cfg.totalCost("claude-haiku-5-5", tc.u)
			if !near(got, tc.want) || unpriced {
				t.Fatalf("cost %g, want %g (unpriced=%v)", got, tc.want, unpriced)
			}
		})
	}
	// a model without a long-context tier is unaffected by prompt length
	if got, _ := cfg.totalCost("claude-sonnet-5-5", Usage{InputTokens: 500000}); !near(got, 500000*2.0/1e6) {
		t.Fatalf("sonnet long prompt cost %g", got)
	}
	// each fallback iteration picks its own tier
	u := Usage{Iterations: []Iteration{
		{Model: "claude-haiku-5-5", Usage: Usage{InputTokens: 150000}},
		{Model: "claude-haiku-5-5", Usage: Usage{InputTokens: 50000}},
	}}
	if got, _ := cfg.totalCost("claude-haiku-5-5", u); !near(got, (150000*0.5+50000*0.1)/1e6) {
		t.Fatalf("iterations cost %g", got)
	}
}

func TestUnknownModelUsesHighestRateIncludingLongContext(t *testing.T) {
	cfg := &Config{Prices: map[string]Price{
		"cheap": {Input: 1, Output: 2, CacheRead: 0.1, CacheWrite5m: 1.25, CacheWrite1h: 2,
			LongContext: &LongContextPrice{OverPromptTokens: 10, Input: 9, Output: 30, CacheRead: 0.9, CacheWrite5m: 11, CacheWrite1h: 18}},
		"mid": {Input: 5, Output: 25, CacheRead: 0.5, CacheWrite5m: 6.25, CacheWrite1h: 10},
	}}
	p, ok := cfg.priceFor("claude-unknown")
	if ok || p.Input != 9 || p.Output != 30 || p.CacheRead != 0.9 || p.CacheWrite5m != 11 || p.CacheWrite1h != 18 {
		t.Fatalf("got %+v ok=%v", p, ok)
	}
}

func TestLongContextNeedsThreshold(t *testing.T) {
	cfg := &Config{Prices: map[string]Price{"m": {Input: 1, LongContext: &LongContextPrice{Input: 2}}},
		Roles: []Role{{Name: "r", Listen: ":1", Models: []string{"m"}, MaxTokensPerRequest: 1, AllowedBetas: []string{}}}}
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "over_prompt_tokens") {
		t.Fatalf("err %v", err)
	}
}

func TestExampleConfigLoads(t *testing.T) {
	cfg, err := loadConfig("config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	if lc := cfg.Prices["claude-haiku-5-5"].LongContext; lc == nil || lc.OverPromptTokens != 100000 {
		t.Fatalf("haiku long-context tier missing: %+v", lc)
	}
}
