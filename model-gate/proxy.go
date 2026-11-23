// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Paths the gate forwards; everything else is 403. Batches, Files, Skills,
// Managed Agents and the Admin API are deliberately absent.
var allowedRoutes = map[string]string{
	"/v1/messages":              http.MethodPost,
	"/v1/messages/count_tokens": http.MethodPost,
	"/v1/models":                http.MethodGet,
}

// Model-discovery probes Hermes sends to every custom provider, looking for a
// model list or a local server (Ollama, llama.cpp, LM Studio), even with
// discover_models off. Answered 404 here and never forwarded, with their own
// reason so they don't set off the endpoint alert. Exact paths, GET only:
// anything else unknown is still path_not_allowed.
var discoveryProbes = map[string]bool{
	"/models": true, "/api/v1/models": true, "/api/tags": true, "/props": true, "/v1/props": true, "/version": true,
}

// Server-side tools that make Anthropic reach the network on the caller's
// behalf, bypassing the cluster egress policy (T2, T9). Every non-custom tool
// type is denied unless allowed_tool_types lists it; these only get a more
// specific rejection reason.
var egressToolPrefixes = []string{"web_search", "web_fetch", "code_execution", "mcp_toolset"}

// Request headers never forwarded. Credentials are replaced by the role key.
var dropRequestHeaders = map[string]bool{
	"X-Api-Key": true, "Authorization": true, "Cookie": true, "Accept-Encoding": true,
	"Connection": true, "Keep-Alive": true, "Proxy-Authorization": true, "Proxy-Connection": true,
	"Te": true, "Trailer": true, "Transfer-Encoding": true, "Upgrade": true, "Host": true,
	"Content-Length": true, "Forwarded": true, "X-Forwarded-For": true, "X-Forwarded-Host": true,
	"X-Forwarded-Proto": true, "X-Real-Ip": true,
}

var dropResponseHeaders = map[string]bool{
	"Set-Cookie": true, "Connection": true, "Keep-Alive": true, "Transfer-Encoding": true,
	"Content-Length": true, "Content-Encoding": true, "Trailer": true, "Upgrade": true,
}

type Gate struct {
	cfg     *Config
	budgets *Budgets
	metrics *Metrics
	client  *http.Client
	log     *slog.Logger
	now     func() time.Time
}

// requestInfo is what the gate reads from a request body.
type requestInfo struct {
	Model      string          `json:"model"`
	MaxTokens  int64           `json:"max_tokens"`
	Stream     bool            `json:"stream"`
	Tools      []toolDef       `json:"tools"`
	MCPServers json.RawMessage `json:"mcp_servers"`
	Container  json.RawMessage `json:"container"`
	Fallbacks  json.RawMessage `json:"fallbacks"`
}

type toolDef struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

type rejection struct {
	status int
	reason string
	msg    string
}

func (g *Gate) handler(r *Role) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		g.serve(r, w, req)
	})
}

func (g *Gate) serve(role *Role, w http.ResponseWriter, req *http.Request) {
	start := g.now()
	rec := auditRecord{Role: role.Name, Method: req.Method, Path: req.URL.Path, ID: newID()}
	defer func() {
		rec.LatencyMS = g.now().Sub(start).Milliseconds()
		g.audit(rec)
	}()

	reject := func(rj rejection) {
		rec.Status, rec.Rejected = rj.status, rj.reason
		g.metrics.Add("model_gate_rejections_total", 1, "role", role.Name, "reason", rj.reason)
		g.metrics.Add("model_gate_requests_total", 1, "role", role.Name, "outcome", "rejected", "status", fmt.Sprintf("%dxx", rj.status/100))
		writeAPIError(w, rj.status, rj.msg)
	}

	if rj := checkRoute(req); rj != nil {
		reject(*rj)
		return
	}
	isMessages := req.URL.Path == "/v1/messages"
	if isMessages {
		if reason := g.budgets.Check(role, start); reason != "" {
			reject(rejection{http.StatusTooManyRequests, reason, "model-gate: " + strings.ReplaceAll(reason, "_", " ") + " for role " + role.Name})
			return
		}
	}

	var body []byte
	if req.Method == http.MethodPost {
		var err error
		body, err = io.ReadAll(http.MaxBytesReader(w, req.Body, g.cfg.MaxRequestBytes))
		if err != nil {
			reject(rejection{http.StatusRequestEntityTooLarge, "request_too_large", "model-gate: request body too large or unreadable"})
			return
		}
		var info requestInfo
		if err := json.Unmarshal(body, &info); err != nil {
			reject(rejection{http.StatusBadRequest, "invalid_json", "model-gate: request body is not valid JSON"})
			return
		}
		if err := checkUnambiguous(body); err != nil {
			reject(rejection{http.StatusBadRequest, "ambiguous_json", "model-gate: ambiguous request body: " + err.Error()})
			return
		}
		if err := checkSources(body); err != nil {
			reject(rejection{http.StatusForbidden, "source_not_allowed", "model-gate: " + err.Error()})
			return
		}
		rec.Model, rec.Stream, rec.MaxTokens = info.Model, info.Stream, info.MaxTokens
		if isMessages && role.ClampMaxTokens && info.MaxTokens > role.MaxTokensPerRequest {
			clamped, err := setMaxTokens(body, role.MaxTokensPerRequest)
			if err != nil {
				reject(rejection{http.StatusBadRequest, "invalid_json", "model-gate: request body is not a JSON object"})
				return
			}
			body = clamped
			rec.MaxTokensRequested, rec.MaxTokens = info.MaxTokens, role.MaxTokensPerRequest
			info.MaxTokens = role.MaxTokensPerRequest
		}
		if rj := checkBody(role, &info, isMessages); rj != nil {
			reject(*rj)
			return
		}
	}

	betas, rj := filterBetas(role, req.Header.Values("Anthropic-Beta"))
	if rj != nil {
		reject(*rj)
		return
	}

	up, err := http.NewRequestWithContext(req.Context(), req.Method, g.cfg.Upstream+req.URL.Path, bytes.NewReader(body))
	if err != nil {
		reject(rejection{http.StatusInternalServerError, "internal", "model-gate: internal error"})
		return
	}
	up.URL.RawQuery = req.URL.RawQuery
	for k, vs := range req.Header {
		if dropRequestHeaders[http.CanonicalHeaderKey(k)] || http.CanonicalHeaderKey(k) == "Anthropic-Beta" {
			continue
		}
		for _, v := range vs {
			up.Header.Add(k, v)
		}
	}
	for _, b := range betas {
		up.Header.Add("Anthropic-Beta", b)
	}
	up.Header.Set("X-Api-Key", role.key)
	up.ContentLength = int64(len(body))

	g.capture(role, rec.ID, "request", body)

	resp, err := g.client.Do(up)
	if err != nil {
		rec.Status, rec.Error = http.StatusBadGateway, "upstream_unreachable"
		g.metrics.Add("model_gate_requests_total", 1, "role", role.Name, "outcome", "upstream_error", "status", "5xx")
		writeAPIError(w, http.StatusBadGateway, "model-gate: upstream unreachable")
		return
	}
	defer resp.Body.Close()
	rec.Status = resp.StatusCode
	rec.UpstreamRequestID = resp.Header.Get("Request-Id")

	for k, vs := range resp.Header {
		if dropResponseHeaders[http.CanonicalHeaderKey(k)] {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}

	var usage Usage
	var model string
	var haveUsage bool
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		w.WriteHeader(resp.StatusCode)
		sse := &sseUsage{}
		capture := g.captureWriter(role, rec.ID)
		flusher, _ := w.(http.Flusher)
		buf := make([]byte, 32<<10)
		for {
			n, rerr := resp.Body.Read(buf)
			if n > 0 {
				_, _ = sse.Write(buf[:n])
				if capture != nil {
					_, _ = capture.Write(buf[:n])
				}
				if _, werr := w.Write(buf[:n]); werr != nil {
					rec.Error = "client_disconnected"
					break
				}
				if flusher != nil {
					flusher.Flush()
				}
			}
			if rerr != nil {
				if !errors.Is(rerr, io.EOF) && rec.Error == "" {
					rec.Error = "upstream_stream_error"
				}
				break
			}
		}
		if capture != nil {
			_ = capture.Close()
		}
		usage, model, haveUsage = sse.Usage, sse.Model, sse.Seen
		if sse.Error != "" {
			rec.Error = "stream_error:" + sse.Error
		} else if !sse.Complete && rec.Error == "" && resp.StatusCode == http.StatusOK {
			rec.Error = "stream_incomplete"
		}
	} else {
		data, rerr := io.ReadAll(io.LimitReader(resp.Body, g.cfg.MaxResponseBytes+1))
		if rerr != nil || int64(len(data)) > g.cfg.MaxResponseBytes {
			rec.Status, rec.Error = http.StatusBadGateway, "upstream_response_too_large_or_broken"
			writeAPIError(w, http.StatusBadGateway, "model-gate: upstream response too large or broken")
			g.metrics.Add("model_gate_requests_total", 1, "role", role.Name, "outcome", "upstream_error", "status", "5xx")
			return
		}
		g.capture(role, rec.ID, "response", data)
		w.Header().Set("Content-Length", fmt.Sprint(len(data)))
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(data)
		if isMessages && resp.StatusCode == http.StatusOK {
			model, usage, haveUsage = parseJSONUsage(data)
		}
	}

	outcome := "ok"
	if resp.StatusCode >= 400 {
		outcome = "upstream_error"
	}
	g.metrics.Add("model_gate_requests_total", 1, "role", role.Name, "outcome", outcome, "status", fmt.Sprintf("%dxx", resp.StatusCode/100))

	if !isMessages || !haveUsage {
		return
	}
	if model == "" {
		model = rec.Model
	}
	cost, unpriced := g.cfg.totalCost(model, usage)
	hourUSD, dayUSD := g.budgets.Add(role.Name, cost, g.now())
	rec.ResponseModel = model
	rec.InputTokens, rec.OutputTokens = usage.InputTokens, usage.OutputTokens
	rec.CacheReadTokens, rec.CacheWriteTokens = usage.CacheReadInputTokens, usage.CacheCreationInputTokens
	rec.Iterations = len(usage.Iterations)
	rec.CostUSD, rec.HourUSD, rec.DayUSD = cost, hourUSD, dayUSD
	g.metrics.Add("model_gate_cost_usd_total", cost, "role", role.Name)
	for kind, n := range map[string]int64{
		"input": usage.InputTokens, "output": usage.OutputTokens,
		"cache_read": usage.CacheReadInputTokens, "cache_write": usage.CacheCreationInputTokens,
	} {
		g.metrics.Add("model_gate_tokens_total", float64(n), "role", role.Name, "model", model, "kind", kind)
	}
	if unpriced {
		rec.Unpriced = true
		g.metrics.Add("model_gate_unpriced_total", 1, "role", role.Name, "model", model)
	}
}

// setMaxTokens returns body with max_tokens replaced, re-encoded from its
// top-level fields. checkUnambiguous has already ruled out duplicate and
// case-variant keys, so nothing else in the body changes meaning.
func setMaxTokens(body []byte, n int64) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil || m == nil {
		return nil, errors.New("not a JSON object")
	}
	m["max_tokens"] = json.RawMessage(strconv.FormatInt(n, 10))
	return json.Marshal(m)
}

func checkRoute(req *http.Request) *rejection {
	path := req.URL.Path
	method, ok := allowedRoutes[path]
	if !ok && strings.HasPrefix(path, "/v1/models/") && !strings.Contains(path[len("/v1/models/"):], "/") {
		method, ok = http.MethodGet, true
	}
	switch {
	case !ok && req.Method == http.MethodGet && discoveryProbes[path]:
		return &rejection{http.StatusNotFound, "discovery_probe", "model-gate: not found: " + path}
	case !ok:
		return &rejection{http.StatusForbidden, "path_not_allowed", "model-gate: endpoint not allowed: " + path}
	case req.Method != method:
		return &rejection{http.StatusMethodNotAllowed, "method_not_allowed", "model-gate: method not allowed"}
	}
	return nil
}

func checkBody(role *Role, info *requestInfo, isMessages bool) *rejection {
	if !contains(role.Models, info.Model) {
		return &rejection{http.StatusForbidden, "model_not_allowed", fmt.Sprintf("model-gate: model %q not allowed for role %s", info.Model, role.Name)}
	}
	if isMessages && (info.MaxTokens <= 0 || info.MaxTokens > role.MaxTokensPerRequest) {
		return &rejection{http.StatusBadRequest, "max_tokens_over_cap", fmt.Sprintf("model-gate: max_tokens must be 1..%d for role %s", role.MaxTokensPerRequest, role.Name)}
	}
	for _, t := range info.Tools {
		if t.Type == "" || t.Type == "custom" || hasPrefixIn(role.AllowedToolTypes, t.Type) {
			continue
		}
		if hasPrefixIn(egressToolPrefixes, t.Type) {
			return &rejection{http.StatusForbidden, "server_tool_not_allowed", fmt.Sprintf("model-gate: server-side tool %q not allowed (it reaches the network from Anthropic's side)", t.Type)}
		}
		return &rejection{http.StatusForbidden, "tool_type_not_allowed", fmt.Sprintf("model-gate: tool type %q not allowed for role %s (only custom tools by default)", t.Type, role.Name)}
	}
	if nonEmptyJSON(info.MCPServers) {
		return &rejection{http.StatusForbidden, "mcp_servers_not_allowed", "model-gate: mcp_servers (MCP connector) not allowed"}
	}
	if nonEmptyJSON(info.Container) {
		return &rejection{http.StatusForbidden, "container_not_allowed", "model-gate: container (skills/code execution) not allowed"}
	}
	if nonEmptyJSON(info.Fallbacks) {
		var s string
		if json.Unmarshal(info.Fallbacks, &s) == nil {
			if !role.AllowDefaultFallbacks {
				return &rejection{http.StatusForbidden, "fallbacks_not_allowed", "model-gate: fallbacks \"default\" not allowed for this role"}
			}
		} else {
			var list []struct {
				Model string `json:"model"`
			}
			if json.Unmarshal(info.Fallbacks, &list) != nil {
				return &rejection{http.StatusBadRequest, "fallbacks_invalid", "model-gate: unrecognised fallbacks value"}
			}
			for _, f := range list {
				if !contains(role.Models, f.Model) {
					return &rejection{http.StatusForbidden, "fallback_model_not_allowed", fmt.Sprintf("model-gate: fallback model %q not allowed", f.Model)}
				}
			}
		}
	}
	return nil
}

// filterBetas splits comma-separated anthropic-beta values and rejects
// anything outside the role's allowlist (an empty list allows none).
func filterBetas(role *Role, values []string) ([]string, *rejection) {
	var out []string
	for _, v := range values {
		for b := range strings.SplitSeq(v, ",") {
			b = strings.TrimSpace(b)
			if b == "" {
				continue
			}
			if !contains(role.AllowedBetas, b) {
				return nil, &rejection{http.StatusForbidden, "beta_not_allowed", fmt.Sprintf("model-gate: anthropic-beta %q not allowed", b)}
			}
			out = append(out, b)
		}
	}
	return out, nil
}

type auditRecord struct {
	ID                 string  `json:"id"`
	Role               string  `json:"role"`
	Method             string  `json:"method"`
	Path               string  `json:"path"`
	Model              string  `json:"model,omitempty"`
	ResponseModel      string  `json:"response_model,omitempty"`
	Stream             bool    `json:"stream,omitempty"`
	MaxTokens          int64   `json:"max_tokens,omitempty"`
	MaxTokensRequested int64   `json:"max_tokens_requested,omitempty"`
	Status             int     `json:"status"`
	Rejected           string  `json:"rejected,omitempty"`
	Error              string  `json:"error,omitempty"`
	UpstreamRequestID  string  `json:"upstream_request_id,omitempty"`
	InputTokens        int64   `json:"input_tokens,omitempty"`
	OutputTokens       int64   `json:"output_tokens,omitempty"`
	CacheReadTokens    int64   `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens   int64   `json:"cache_write_tokens,omitempty"`
	Iterations         int     `json:"iterations,omitempty"`
	CostUSD            float64 `json:"cost_usd,omitempty"`
	HourUSD            float64 `json:"hour_usd,omitempty"`
	DayUSD             float64 `json:"day_usd,omitempty"`
	Unpriced           bool    `json:"unpriced,omitempty"`
	LatencyMS          int64   `json:"latency_ms"`
}

func (g *Gate) audit(r auditRecord) {
	b, _ := json.Marshal(r)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	args := make([]any, 0, len(m)*2)
	for k, v := range m {
		args = append(args, k, v)
	}
	g.log.Info("request", args...)
}

// capture writes a body to the role's debug directory, if enabled.
func (g *Gate) capture(role *Role, id, kind string, data []byte) {
	if role.DebugCaptureDir == "" || len(data) == 0 {
		return
	}
	_ = os.WriteFile(filepath.Join(role.DebugCaptureDir, id+"."+kind+".json"), data, 0o600)
}

func (g *Gate) captureWriter(role *Role, id string) io.WriteCloser {
	if role.DebugCaptureDir == "" {
		return nil
	}
	f, err := os.OpenFile(filepath.Join(role.DebugCaptureDir, id+".response.sse"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil
	}
	return f
}

func writeAPIError(w http.ResponseWriter, status int, msg string) {
	typ := map[int]string{
		400: "invalid_request_error", 403: "permission_error", 404: "not_found_error",
		405: "invalid_request_error", 413: "request_too_large", 429: "rate_limit_error",
	}[status]
	if typ == "" {
		typ = "api_error"
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Model-Gate", "rejected")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": typ, "message": msg},
	})
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func contains(list []string, s string) bool { return slices.Contains(list, s) }

func hasPrefixIn(prefixes []string, s string) bool {
	for _, p := range prefixes {
		if p != "" && strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

func nonEmptyJSON(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) > 0 && !bytes.Equal(t, []byte("null")) && !bytes.Equal(t, []byte("[]")) && !bytes.Equal(t, []byte("{}")) && !bytes.Equal(t, []byte(`""`))
}
