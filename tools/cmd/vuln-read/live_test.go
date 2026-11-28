// SPDX-License-Identifier: Apache-2.0

//go:build live

package main

// Live read-only test against a real cluster through `kubectl proxy`:
//
//	kubectl proxy --port=18001 &
//	LIVE_KUBE_PROXY=http://127.0.0.1:18001 LIVE_NAMESPACE=<ns> LIVE_WORKLOAD=<name> \
//	  go test -tags live -run Live -v ./cmd/vuln-read/

import (
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/runlevel-six/familiar/tools/internal/kube"
	"github.com/runlevel-six/familiar/tools/internal/mcp"
	"github.com/runlevel-six/familiar/tools/internal/redact"
)

func TestLive(t *testing.T) {
	base := os.Getenv("LIVE_KUBE_PROXY")
	if base == "" {
		t.Skip("LIVE_KUBE_PROXY not set")
	}
	k := &kube.Client{BaseURL: base, HTTP: &http.Client{Timeout: 60 * time.Second}}
	h := newHandlers(newIndex(k, time.Now, time.Minute), time.Now)
	s := &mcp.Server{Name: "vuln-read", Version: "live", Tools: h.tools(), Redact: redact.Options{IPs: true}}
	for _, c := range []struct{ tool, args string }{
		{"vulnerability_summary", `{"limit":12}`},
		{"list_vulnerabilities", `{"namespace":"` + os.Getenv("LIVE_NAMESPACE") + `","limit":5}`},
		{"list_vulnerabilities", `{"namespace":"` + os.Getenv("LIVE_NAMESPACE") + `","workload":"` + os.Getenv("LIVE_WORKLOAD") + `","min_severity":"critical"}`},
		{"find_vulnerability", `{"id":"CVE-2026-109518"}`},
	} {
		start := time.Now()
		out, isErr := call(t, s, c.tool, c.args)
		b, _ := json.Marshal(out)
		if n := 2500; len(b) > n {
			b = append(b[:n], "…"...)
		}
		t.Logf("%s %s (%v, error=%v) -> %s", c.tool, c.args, time.Since(start).Round(time.Millisecond), isErr, b)
	}
}
