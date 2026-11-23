// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testServer(logs *bytes.Buffer) *Server {
	return &Server{
		Name: "test-server", Version: "0.1.0", Instructions: "read-only test tools",
		Log: slog.New(slog.NewJSONHandler(logs, nil)),
		Tools: []Tool{
			{Name: "echo", Title: "Echo", Description: "echo back", InputSchema: Object(map[string]any{"text": Str("t")}, "text"),
				Handler: func(_ context.Context, a json.RawMessage) (any, error) {
					var in struct{ Text string }
					if err := Decode(a, &in); err != nil {
						return nil, err
					}
					if in.Text == "fail" {
						return nil, Errorf("bad text %q", in.Text)
					}
					if in.Text == "boom" {
						return nil, context.DeadlineExceeded
					}
					if in.Text == "big" {
						items := make([]string, 3000)
						for i := range items {
							items[i] = strings.Repeat("x", 200)
						}
						return items, nil
					}
					return map[string]any{"echo": in.Text}, nil
				}},
			{Name: "aaa_first", Description: "sorts first", InputSchema: Object(map[string]any{}),
				Handler: func(context.Context, json.RawMessage) (any, error) { return "ok", nil }},
		},
	}
}

type call struct {
	method  string
	body    string
	headers map[string]string
}

func modernBody(id int, method, params string) string {
	meta := `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientInfo":{"name":"t","version":"1"},"io.modelcontextprotocol/clientCapabilities":{}}`
	if params == "" {
		params = "{" + meta + "}"
	} else {
		params = strings.TrimSuffix(params, "}") + "," + meta + "}"
	}
	return `{"jsonrpc":"2.0","id":` + itoa(id) + `,"method":"` + method + `","params":` + params + `}`
}

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }

func modernHeaders(method, name string) map[string]string {
	h := map[string]string{"MCP-Protocol-Version": "2026-07-28", "Mcp-Method": method, "Accept": "application/json, text/event-stream"}
	if name != "" {
		h["Mcp-Name"] = name
	}
	return h
}

func do(t *testing.T, s *Server, c call) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	if c.method == "" {
		c.method = http.MethodPost
	}
	req := httptest.NewRequest(c.method, "/mcp", strings.NewReader(c.body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	var m map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &m)
	return rr, m
}

func errCode(m map[string]any) float64 {
	if e, ok := m["error"].(map[string]any); ok {
		return e["code"].(float64)
	}
	return 0
}

func TestModernDiscoverListCall(t *testing.T) {
	s := testServer(&bytes.Buffer{})
	rr, m := do(t, s, call{body: modernBody(1, "server/discover", ""), headers: modernHeaders("server/discover", "")})
	res := m["result"].(map[string]any)
	if rr.Code != 200 || res["resultType"] != "complete" || res["supportedVersions"].([]any)[0] != "2026-07-28" {
		t.Fatalf("discover: %d %v", rr.Code, m)
	}
	if res["_meta"].(map[string]any)["io.modelcontextprotocol/serverInfo"].(map[string]any)["name"] != "test-server" {
		t.Fatalf("serverInfo missing: %v", res)
	}

	_, m = do(t, s, call{body: modernBody(2, "tools/list", ""), headers: modernHeaders("tools/list", "")})
	tools := m["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 2 || tools[0].(map[string]any)["name"] != "aaa_first" {
		t.Fatalf("tools not listed in sorted order: %v", tools)
	}
	if ann := tools[1].(map[string]any)["annotations"].(map[string]any); ann["readOnlyHint"] != true || ann["destructiveHint"] != false {
		t.Fatalf("annotations: %v", ann)
	}

	rr, m = do(t, s, call{body: modernBody(3, "tools/call", `{"name":"echo","arguments":{"text":"hello"}}`), headers: modernHeaders("tools/call", "echo")})
	res = m["result"].(map[string]any)
	if rr.Code != 200 || res["isError"] != false || res["structuredContent"].(map[string]any)["echo"] != "hello" || res["resultType"] != "complete" {
		t.Fatalf("call: %d %v", rr.Code, m)
	}
	if txt := res["content"].([]any)[0].(map[string]any)["text"]; txt != `{"echo":"hello"}` {
		t.Fatalf("text content = %v", txt)
	}
}

func TestModernHeaderValidation(t *testing.T) {
	s := testServer(&bytes.Buffer{})
	body := modernBody(1, "tools/call", `{"name":"echo","arguments":{"text":"x"}}`)
	for name, h := range map[string]map[string]string{
		"missing version header": {"Mcp-Method": "tools/call", "Mcp-Name": "echo"},
		"version mismatch":       {"MCP-Protocol-Version": "2025-11-25", "Mcp-Method": "tools/call", "Mcp-Name": "echo"},
		"missing method header":  {"MCP-Protocol-Version": "2026-07-28", "Mcp-Name": "echo"},
		"method mismatch":        {"MCP-Protocol-Version": "2026-07-28", "Mcp-Method": "tools/list", "Mcp-Name": "echo"},
		"missing name header":    {"MCP-Protocol-Version": "2026-07-28", "Mcp-Method": "tools/call"},
		"name mismatch":          {"MCP-Protocol-Version": "2026-07-28", "Mcp-Method": "tools/call", "Mcp-Name": "other"},
		"bad base64 name":        {"MCP-Protocol-Version": "2026-07-28", "Mcp-Method": "tools/call", "Mcp-Name": "=?base64?!!!?="},
	} {
		rr, m := do(t, s, call{body: body, headers: h})
		if rr.Code != 400 || errCode(m) != codeHeaderMismatch {
			t.Errorf("%s: %d %v", name, rr.Code, m)
		}
	}
	// base64 sentinel form is accepted
	h := modernHeaders("tools/call", "=?base64?"+base64.StdEncoding.EncodeToString([]byte("echo"))+"?=")
	if rr, m := do(t, s, call{body: body, headers: h}); rr.Code != 200 || m["error"] != nil {
		t.Fatalf("base64 Mcp-Name rejected: %d %v", rr.Code, m)
	}
}

func TestModernUnsupportedVersionAndUnknownMethod(t *testing.T) {
	s := testServer(&bytes.Buffer{})
	body := strings.Replace(modernBody(1, "tools/list", ""), "2026-07-28", "2099-01-01", 1)
	h := modernHeaders("tools/list", "")
	h["MCP-Protocol-Version"] = "2099-01-01"
	rr, m := do(t, s, call{body: body, headers: h})
	if rr.Code != 400 || errCode(m) != codeUnsupported {
		t.Fatalf("unsupported version: %d %v", rr.Code, m)
	}
	data := m["error"].(map[string]any)["data"].(map[string]any)
	if data["requested"] != "2099-01-01" || len(data["supported"].([]any)) < 2 {
		t.Fatalf("error data: %v", data)
	}
	rr, m = do(t, s, call{body: modernBody(2, "resources/list", ""), headers: modernHeaders("resources/list", "")})
	if rr.Code != 404 || errCode(m) != codeMethodNotFound {
		t.Fatalf("unknown method: %d %v", rr.Code, m)
	}
}

func TestLegacyInitializeFlow(t *testing.T) {
	s := testServer(&bytes.Buffer{})
	rr, m := do(t, s, call{body: `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"hermes","version":"x"}}}`})
	res := m["result"].(map[string]any)
	if rr.Code != 200 || res["protocolVersion"] != "2025-06-18" || res["serverInfo"].(map[string]any)["name"] != "test-server" {
		t.Fatalf("initialize: %d %v", rr.Code, m)
	}
	if rr.Header().Get("Mcp-Session-Id") != "" {
		t.Fatal("session id minted")
	}
	// an unknown legacy version gets the newest legacy version
	_, m = do(t, s, call{body: `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-01-01"}}`})
	if m["result"].(map[string]any)["protocolVersion"] != "2025-11-25" {
		t.Fatalf("version fallback: %v", m)
	}
	rr, _ = do(t, s, call{body: `{"jsonrpc":"2.0","method":"notifications/initialized"}`, headers: map[string]string{"MCP-Protocol-Version": "2025-06-18"}})
	if rr.Code != 202 || rr.Body.Len() != 0 {
		t.Fatalf("notification: %d %q", rr.Code, rr.Body)
	}
	rr, m = do(t, s, call{body: `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hi"}}}`,
		headers: map[string]string{"MCP-Protocol-Version": "2025-06-18", "Mcp-Session-Id": "ignored"}})
	res = m["result"].(map[string]any)
	if rr.Code != 200 || res["structuredContent"].(map[string]any)["echo"] != "hi" {
		t.Fatalf("legacy call: %d %v", rr.Code, m)
	}
	if _, has := res["resultType"]; has {
		t.Fatal("legacy result carries resultType")
	}
	// unknown legacy method: JSON-RPC error with HTTP 200
	if rr, m := do(t, s, call{body: `{"jsonrpc":"2.0","id":3,"method":"prompts/list"}`}); rr.Code != 200 || errCode(m) != codeMethodNotFound {
		t.Fatalf("legacy unknown method: %d %v", rr.Code, m)
	}
	// bogus legacy version header
	if rr, _ := do(t, s, call{body: `{"jsonrpc":"2.0","id":4,"method":"tools/list"}`, headers: map[string]string{"MCP-Protocol-Version": "1999-01-01"}}); rr.Code != 400 {
		t.Fatalf("bogus header accepted: %d", rr.Code)
	}
}

func TestTransportRules(t *testing.T) {
	s := testServer(&bytes.Buffer{})
	if rr, _ := do(t, s, call{method: http.MethodGet}); rr.Code != 405 {
		t.Errorf("GET: %d", rr.Code)
	}
	if rr, _ := do(t, s, call{method: http.MethodDelete}); rr.Code != 405 {
		t.Errorf("DELETE: %d", rr.Code)
	}
	if rr, _ := do(t, s, call{body: `{"jsonrpc":"2.0","id":1,"method":"ping"}`, headers: map[string]string{"Origin": "https://evil.example"}}); rr.Code != 403 {
		t.Errorf("foreign Origin: %d", rr.Code)
	}
	if rr, _ := do(t, s, call{body: `[{"jsonrpc":"2.0","id":1,"method":"ping"}]`}); rr.Code != 400 {
		t.Errorf("batch: %d", rr.Code)
	}
	if rr, m := do(t, s, call{body: `{nope`}); rr.Code != 400 || errCode(m) != codeParse {
		t.Errorf("parse error: %d %v", rr.Code, m)
	}
	if rr, m := do(t, s, call{body: `{"jsonrpc":"2.0","id":1,"method":"ping"}`}); rr.Code != 200 || m["result"] == nil {
		t.Errorf("ping: %d %v", rr.Code, m)
	}
}

func TestToolErrorsAndLimits(t *testing.T) {
	logs := &bytes.Buffer{}
	s := testServer(logs)
	legacy := func(text string) map[string]any {
		_, m := do(t, s, call{body: `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"text":"` + text + `"}}}`})
		return m
	}
	if r := legacy("fail")["result"].(map[string]any); r["isError"] != true || !strings.Contains(r["content"].([]any)[0].(map[string]any)["text"].(string), "bad text") {
		t.Errorf("tool error: %v", r)
	}
	if r := legacy("boom")["result"].(map[string]any); r["isError"] != true || strings.Contains(r["content"].([]any)[0].(map[string]any)["text"].(string), "deadline") {
		t.Errorf("internal error leaked or not flagged: %v", r)
	}
	if r := legacy("big")["result"].(map[string]any); r["isError"] != true {
		t.Errorf("oversized result not refused: %v", r)
	}
	// redaction applies to results
	if r := legacy("token=abc123secret")["result"].(map[string]any); r["structuredContent"].(map[string]any)["echo"] != "token=[REDACTED]" {
		t.Errorf("result not redacted: %v", r)
	}
	// unknown tool is a protocol error
	_, m := do(t, s, call{body: `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"nope"}}`})
	if errCode(m) != codeInvalidParams {
		t.Errorf("unknown tool: %v", m)
	}
	// strict argument decoding
	_, m = do(t, s, call{body: `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"text":"a","extra":1}}}`})
	if m["result"].(map[string]any)["isError"] != true {
		t.Errorf("unknown argument accepted: %v", m)
	}
	// every call was audited, and secrets in arguments are redacted in the log
	if n := strings.Count(logs.String(), `"msg":"tool_call"`); n != 6 {
		t.Errorf("audit lines = %d, want 6 (5 calls + one unknown tool)", n)
	}
	if strings.Contains(logs.String(), "abc123secret") {
		t.Error("secret argument written to audit log")
	}
}

func TestRateLimit(t *testing.T) {
	s := testServer(&bytes.Buffer{})
	s.RatePerMinute = 4 // burst 5
	limited := 0
	for range 8 {
		_, m := do(t, s, call{body: `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"aaa_first"}}`})
		if r := m["result"].(map[string]any); r["isError"] == true {
			limited++
		}
	}
	if limited != 3 {
		t.Fatalf("limited %d of 8, want 3", limited)
	}
}
