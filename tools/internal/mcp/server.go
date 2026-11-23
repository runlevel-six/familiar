// SPDX-License-Identifier: Apache-2.0

// Package mcp is a minimal, read-only MCP tool server over Streamable HTTP
// (stdlib only). It is dual-era:
//
//   - Modern (2026-07-28): stateless; every request carries
//     _meta["io.modelcontextprotocol/protocolVersion"], mirrored into the
//     MCP-Protocol-Version, Mcp-Method and Mcp-Name headers, which are
//     validated against the body (-32020 HeaderMismatch). server/discover is
//     implemented; results carry resultType "complete".
//   - Legacy (2025-03-26 .. 2025-11-25): an initialize handshake; served
//     statelessly (no Mcp-Session-Id is minted, which those revisions allow).
//
// Only tools are offered (no resources, prompts, sampling or elicitation),
// and every tool is read-only. Each call is audit-logged independently of
// the agent (T10), rate limited, and its output redacted.
package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/runlevel-six/familiar/tools/internal/redact"
)

const (
	ModernVersion = "2026-07-28"
	metaVersion   = "io.modelcontextprotocol/protocolVersion"
	metaClient    = "io.modelcontextprotocol/clientInfo"
	metaServer    = "io.modelcontextprotocol/serverInfo"

	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternal       = -32603
	codeHeaderMismatch = -32020
	codeUnsupported    = -32022
)

var (
	modernVersions = []string{ModernVersion}
	legacyVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26"}
)

// Tool is one read-only tool. Handler returns a JSON-encodable value, which
// is redacted and returned as structuredContent plus a JSON text block.
// Return a *ToolError for problems the model can fix (bad arguments etc.).
type Tool struct {
	Name        string
	Title       string
	Description string
	InputSchema map[string]any
	Handler     func(ctx context.Context, args json.RawMessage) (any, error)
}

// ToolError is reported to the model as a tool execution error (isError).
type ToolError struct{ Msg string }

func (e *ToolError) Error() string { return e.Msg }

// Errorf builds a ToolError.
func Errorf(format string, a ...any) error { return &ToolError{fmt.Sprintf(format, a...)} }

// Server serves tools at one endpoint.
type Server struct {
	Name         string
	Version      string
	Instructions string
	Tools        []Tool
	Log          *slog.Logger
	Redact       redact.Options
	// AllowedOrigins lists Origin header values to accept. Requests with any
	// other Origin get 403 (DNS-rebinding protection). Server-side clients
	// such as Hermes send none.
	AllowedOrigins []string
	// RatePerMinute limits tools/call (0 = 60).
	RatePerMinute int
	// MaxResultBytes caps the encoded result (0 = 256 KiB).
	MaxResultBytes int
	// CallTimeout bounds one tool call (0 = 30s).
	CallTimeout time.Duration

	once   sync.Once
	byName map[string]*Tool
	bucket *tokenBucket
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type commonParams struct {
	Meta      map[string]json.RawMessage `json:"_meta"`
	Name      string                     `json:"name"`
	Arguments json.RawMessage            `json:"arguments"`
	// legacy initialize
	ProtocolVersion string          `json:"protocolVersion"`
	ClientInfo      json.RawMessage `json:"clientInfo"`
}

func (s *Server) init() {
	s.once.Do(func() {
		s.byName = map[string]*Tool{}
		for i := range s.Tools {
			s.byName[s.Tools[i].Name] = &s.Tools[i]
		}
		rate := s.RatePerMinute
		if rate <= 0 {
			rate = 60
		}
		s.bucket = newBucket(rate)
		if s.MaxResultBytes == 0 {
			s.MaxResultBytes = 256 << 10
		}
		if s.CallTimeout == 0 {
			s.CallTimeout = 30 * time.Second
		}
		if s.Log == nil {
			s.Log = slog.New(slog.NewJSONHandler(io.Discard, nil))
		}
	})
}

// Handler returns the HTTP handler for the MCP endpoint (mount at /mcp) plus
// /healthz.
func (s *Server) Handler() http.Handler {
	s.init()
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	mux.HandleFunc("/mcp", s.serveMCP)
	return mux
}

func (s *Server) serveMCP(w http.ResponseWriter, r *http.Request) {
	if o := r.Header.Get("Origin"); o != "" && !contains(s.AllowedOrigins, o) {
		writeJSON(w, http.StatusForbidden, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"),
			Error: &rpcError{Code: codeInvalidRequest, Message: "origin not allowed"}})
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, errResp(nil, codeInvalidRequest, "request too large"))
		return
	}
	if t := strings.TrimSpace(string(body)); strings.HasPrefix(t, "[") {
		writeJSON(w, http.StatusBadRequest, errResp(nil, codeInvalidRequest, "batch requests are not supported"))
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp(nil, codeParse, "parse error"))
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		writeJSON(w, http.StatusBadRequest, errResp(req.ID, codeInvalidRequest, "invalid request"))
		return
	}
	var p commonParams
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &p); err != nil {
			writeJSON(w, http.StatusBadRequest, errResp(req.ID, codeInvalidParams, "invalid params"))
			return
		}
	}

	// Notifications (no id): accept with 202 and no body.
	if len(req.ID) == 0 || string(req.ID) == "null" {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	var version string
	if raw, ok := p.Meta[metaVersion]; ok {
		_ = json.Unmarshal(raw, &version)
	}
	modern := version != ""

	if modern {
		if status, e := validateModernHeaders(r, req.Method, version, p.Name); e != nil {
			writeJSON(w, status, rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: e})
			return
		}
		if !contains(modernVersions, version) {
			writeJSON(w, http.StatusBadRequest, rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{
				Code: codeUnsupported, Message: "Unsupported protocol version",
				Data: map[string]any{"supported": append(append([]string{}, modernVersions...), legacyVersions...), "requested": version},
			}})
			return
		}
	} else if h := r.Header.Get("MCP-Protocol-Version"); h != "" && req.Method != "initialize" && !contains(legacyVersions, h) && !contains(modernVersions, h) {
		writeJSON(w, http.StatusBadRequest, errResp(req.ID, codeInvalidRequest, "unsupported MCP-Protocol-Version "+h))
		return
	}

	result, rerr := s.dispatch(r.Context(), r, req, p, modern, version)
	if rerr != nil {
		status := http.StatusOK
		if modern && rerr.Code == codeMethodNotFound {
			status = http.StatusNotFound
		} else if modern && (rerr.Code == codeInvalidParams || rerr.Code == codeInvalidRequest) {
			status = http.StatusBadRequest
		}
		writeJSON(w, status, rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: rerr})
		return
	}
	if modern {
		if m, ok := result.(map[string]any); ok {
			m["resultType"] = "complete"
		}
	}
	writeJSON(w, http.StatusOK, rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result})
}

func validateModernHeaders(r *http.Request, method, version, name string) (int, *rpcError) {
	mismatch := func(msg string) (int, *rpcError) {
		return http.StatusBadRequest, &rpcError{Code: codeHeaderMismatch, Message: "Header mismatch: " + msg}
	}
	if h := r.Header.Get("MCP-Protocol-Version"); h == "" {
		return mismatch("MCP-Protocol-Version header missing")
	} else if h != version {
		return mismatch(fmt.Sprintf("MCP-Protocol-Version %q does not match body %q", h, version))
	}
	if h := r.Header.Get("Mcp-Method"); h == "" {
		return mismatch("Mcp-Method header missing")
	} else if h != method {
		return mismatch(fmt.Sprintf("Mcp-Method %q does not match body %q", h, method))
	}
	if method == "tools/call" {
		h := r.Header.Get("Mcp-Name")
		if h == "" {
			return mismatch("Mcp-Name header missing")
		}
		dec, ok := decodeHeaderValue(h)
		if !ok {
			return mismatch("Mcp-Name header is malformed")
		}
		if dec != name {
			return mismatch(fmt.Sprintf("Mcp-Name %q does not match body %q", dec, name))
		}
	}
	return 0, nil
}

// decodeHeaderValue handles the =?base64?...?= sentinel encoding.
func decodeHeaderValue(v string) (string, bool) {
	if strings.HasPrefix(v, "=?base64?") && strings.HasSuffix(v, "?=") {
		b, err := base64.StdEncoding.DecodeString(v[len("=?base64?") : len(v)-2])
		if err != nil {
			return "", false
		}
		return string(b), true
	}
	for _, c := range v {
		if c < 0x20 && c != '\t' || c > 0x7e {
			return "", false
		}
	}
	return v, true
}

func (s *Server) capabilities() map[string]any {
	return map[string]any{"tools": map[string]any{"listChanged": false}}
}

func (s *Server) dispatch(ctx context.Context, r *http.Request, req rpcRequest, p commonParams, modern bool, version string) (any, *rpcError) {
	switch req.Method {
	case "server/discover":
		if !modern {
			return nil, &rpcError{Code: codeMethodNotFound, Message: "server/discover requires a modern protocol version"}
		}
		return map[string]any{
			"supportedVersions": append(append([]string{}, modernVersions...), legacyVersions...),
			"capabilities":      s.capabilities(),
			"_meta":             map[string]any{metaServer: map[string]string{"name": s.Name, "version": s.Version}},
			"instructions":      s.Instructions,
		}, nil
	case "initialize":
		v := p.ProtocolVersion
		if !contains(legacyVersions, v) {
			v = legacyVersions[0]
		}
		return map[string]any{
			"protocolVersion": v,
			"capabilities":    s.capabilities(),
			"serverInfo":      map[string]string{"name": s.Name, "version": s.Version},
			"instructions":    s.Instructions,
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		tools := make([]map[string]any, 0, len(s.Tools))
		names := make([]string, 0, len(s.byName))
		for n := range s.byName {
			names = append(names, n)
		}
		sort.Strings(names) // deterministic order helps prompt caching
		for _, n := range names {
			t := s.byName[n]
			tools = append(tools, map[string]any{
				"name": t.Name, "title": t.Title, "description": t.Description, "inputSchema": t.InputSchema,
				"annotations": map[string]any{"readOnlyHint": true, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false},
			})
		}
		return map[string]any{"tools": tools}, nil
	case "tools/call":
		return s.call(ctx, r, p, version)
	}
	return nil, &rpcError{Code: codeMethodNotFound, Message: "Method not found: " + req.Method}
}

func (s *Server) call(ctx context.Context, r *http.Request, p commonParams, version string) (any, *rpcError) {
	t := s.byName[p.Name]
	if t == nil {
		s.Log.Warn("tool_call", "tool", redact.String(p.Name, redact.Options{MaxString: 128}), "remote", r.RemoteAddr,
			"protocol", version, "outcome", "unknown_tool")
		return nil, &rpcError{Code: codeInvalidParams, Message: "Unknown tool: " + redact.String(p.Name, redact.Options{MaxString: 128})}
	}
	start := time.Now()
	client := ""
	if raw, ok := p.Meta[metaClient]; ok {
		client = string(raw)
	}
	args := p.Arguments
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage("{}")
	}
	audit := []any{"tool", t.Name, "args", redact.String(string(args), redact.Options{MaxString: 500}),
		"client", redact.String(client, redact.Options{MaxString: 200}), "remote", r.RemoteAddr, "protocol", version}

	if !s.bucket.take() {
		s.Log.Warn("tool_call", append(audit, "outcome", "rate_limited")...)
		return toolErrorResult("rate limit exceeded; try again in a minute"), nil
	}

	cctx, cancel := context.WithTimeout(ctx, s.CallTimeout)
	defer cancel()
	out, err := t.Handler(cctx, args)
	dur := time.Since(start).Milliseconds()
	if err != nil {
		if te, ok := errors.AsType[*ToolError](err); ok {
			s.Log.Info("tool_call", append(audit, "outcome", "tool_error", "error", te.Msg, "ms", dur)...)
			return toolErrorResult(redact.String(te.Msg, s.Redact)), nil
		}
		s.Log.Error("tool_call", append(audit, "outcome", "internal_error", "error", err.Error(), "ms", dur)...)
		return toolErrorResult("internal error in " + s.Name + " (see its log)"), nil
	}
	clean, err := redact.Value(out, s.Redact)
	if err != nil {
		s.Log.Error("tool_call", append(audit, "outcome", "encode_error", "error", err.Error(), "ms", dur)...)
		return nil, &rpcError{Code: codeInternal, Message: "could not encode result"}
	}
	text, _ := json.Marshal(clean)
	if len(text) > s.MaxResultBytes {
		s.Log.Warn("tool_call", append(audit, "outcome", "result_too_large", "bytes", len(text), "ms", dur)...)
		return toolErrorResult(fmt.Sprintf("result too large (%d bytes); narrow the query (namespace, limit)", len(text))), nil
	}
	s.Log.Info("tool_call", append(audit, "outcome", "ok", "bytes", len(text), "ms", dur)...)
	return map[string]any{
		"content":           []map[string]any{{"type": "text", "text": string(text)}},
		"structuredContent": clean,
		"isError":           false,
	}, nil
}

func toolErrorResult(msg string) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": msg}}, "isError": true}
}

func errResp(id json.RawMessage, code int, msg string) rpcResponse {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func contains(list []string, s string) bool {
	return slices.Contains(list, s)
}

// tokenBucket refills ratePerMinute tokens per minute, burst = rate/4 (min 5).
type tokenBucket struct {
	mu     sync.Mutex
	tokens float64
	max    float64
	perSec float64
	last   time.Time
}

func newBucket(perMinute int) *tokenBucket {
	burst := float64(max(perMinute/4, 5))
	return &tokenBucket{tokens: burst, max: burst, perSec: float64(perMinute) / 60, last: time.Now()}
}

func (b *tokenBucket) take() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	b.tokens = min(b.max, b.tokens+now.Sub(b.last).Seconds()*b.perSec)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// Schema helpers for tool input schemas.

// Object builds {"type":"object","properties":...,"required":...,"additionalProperties":false}.
func Object(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

// Str is a string property.
func Str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }

// Enum is a string property limited to values.
func Enum(desc string, values ...string) map[string]any {
	return map[string]any{"type": "string", "description": desc, "enum": values}
}

// Int is an integer property with bounds.
func Int(desc string, minV, maxV int) map[string]any {
	return map[string]any{"type": "integer", "description": desc, "minimum": minV, "maximum": maxV}
}

// Bool is a boolean property.
func Bool(desc string) map[string]any { return map[string]any{"type": "boolean", "description": desc} }

// Decode unmarshals tool arguments strictly (unknown fields are an error the
// model can fix).
func Decode(args json.RawMessage, v any) error {
	dec := json.NewDecoder(strings.NewReader(string(args)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return Errorf("invalid arguments: %v", err)
	}
	return nil
}
