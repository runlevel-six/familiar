// SPDX-License-Identifier: Apache-2.0

//go:build live

// Live read-only test against a real cluster through `kubectl proxy`:
//
//	kubectl proxy --port=18001 &
//	LIVE_KUBE_PROXY=http://127.0.0.1:18001 go test -tags live -run Live -v ./cmd/cluster-read/
//
// The proxy uses whatever kubeconfig is active (admin on the author's cluster), so this
// tests the tools' behaviour, not the narrow ClusterRole; check that with
// `kubectl auth can-i --as=system:serviceaccount:familiar-tools:cluster-read`.
package main

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/runlevel-six/familiar/tools/internal/kube"
	"github.com/runlevel-six/familiar/tools/internal/mcp"
)

func TestLiveAllTools(t *testing.T) {
	base := os.Getenv("LIVE_KUBE_PROXY")
	if base == "" {
		t.Skip("LIVE_KUBE_PROXY not set")
	}
	k := &kube.Client{BaseURL: base, HTTP: &http.Client{Timeout: 30 * time.Second}}
	h := newHandlers(k, time.Now)
	s := &mcp.Server{Name: "cluster-read", Version: "live", Tools: h.tools()}
	s.Redact.IPs = true
	for _, c := range []struct{ tool, args string }{
		{"cluster_overview", `{}`},
		{"list_pods", `{"only_unhealthy":true}`},
		{"list_pods", `{"namespace":"monitoring"}`},
		{"describe_workload", `{"namespace":"monitoring","kind":"deployment","name":"kps-operator"}`},
		{"recent_events", `{"since_minutes":1440}`},
		{"storage_status", `{}`},
		{"certificates", `{}`},
		{"ingresses", `{"namespace":"monitoring"}`},
	} {
		out, isErr := callTool(t, s, c.tool, c.args)
		b, _ := json.MarshalIndent(out, "", "  ")
		if isErr {
			t.Errorf("%s %s: %s", c.tool, c.args, b)
			continue
		}
		for _, bad := range []string{"BEGIN PRIVATE KEY", "sk-ant-", "password=", "192.168."} {
			if strings.Contains(string(b), bad) {
				t.Errorf("%s output contains %q", c.tool, bad)
			}
		}
		t.Logf("%s %s -> %d bytes\n%s", c.tool, c.args, len(b), truncate(string(b), 1500))
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n…"
}
