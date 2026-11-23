// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/runlevel-six/familiar/tools/internal/mcp"
)

// callTool sends a legacy-style tools/call through the server's HTTP handler
// and returns structuredContent (or the error text) and isError.
func callTool(t *testing.T, s *mcp.Server, tool, args string) (map[string]any, bool) {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + tool + `","arguments":` + args + `}}`
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("POST", "/mcp", strings.NewReader(body)))
	var resp struct {
		Result struct {
			IsError           bool              `json:"isError"`
			StructuredContent map[string]any    `json:"structuredContent"`
			Content           []json.RawMessage `json:"content"`
		} `json:"result"`
		Error any `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil || resp.Error != nil {
		t.Fatalf("bad response %s: %v", rr.Body, resp.Error)
	}
	if resp.Result.IsError {
		return map[string]any{"error": string(resp.Result.Content[0])}, true
	}
	return resp.Result.StructuredContent, false
}
