// SPDX-License-Identifier: Apache-2.0

//go:build live

package main

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/runlevel-six/familiar/tools/internal/prom"
)

// LIVE_PROM_URL=http://127.0.0.1:18001/api/v1/namespaces/monitoring/services/kps-prometheus:9090/proxy
func TestLive(t *testing.T) {
	base := os.Getenv("LIVE_PROM_URL")
	if base == "" {
		t.Skip("LIVE_PROM_URL not set")
	}
	if err := setFirewallZones(os.Getenv("LIVE_FIREWALL_ZONES")); err != nil {
		t.Fatal(err)
	}
	h := &handlers{q: prom.New(base), now: time.Now}
	for _, tl := range h.tools() {
		for _, args := range liveArgs[tl.Name] {
			out, err := tl.Handler(context.Background(), json.RawMessage(args))
			if err != nil {
				t.Errorf("%s %s: %v", tl.Name, args, err)
				continue
			}
			b, _ := json.Marshal(out)
			if n := liveLogBytes(); len(b) > n {
				b = append(b[:n], "…"...)
			}
			t.Logf("%s %s -> %s", tl.Name, args, b)
		}
	}
}

var liveArgs = map[string][]string{
	"query_metric": {`{"template":"node_cpu_cores_used"}`, `{"template":"cpu_requests_vs_allocatable"}`, `{"template":"firing_alerts"}`,
		`{"template":"backup_age_hours"}`, `{"template":"pod_restarts","window":"24h"}`, `{"template":"top_container_memory","limit":3}`,
		`{"template":"pvc_usage","namespace":"monitoring"}`, `{"template":"kubelet_restarts","window":"6h"}`, `{"template":"node_filesystems"}`,
		`{"template":"cpu_throttling"}`, `{"template":"targets_down"}`, `{"template":"node_memory"}`,
		`{"template":"port_forwards"}`, `{"template":"inbound_firewall_rules"}`},
	"metric_history": {`{"template":"node_cpu_cores_used","hours":2}`},
}

// liveLogBytes is how much of each result to log (LIVE_LOG_BYTES, default 700).
func liveLogBytes() int {
	if n, err := strconv.Atoi(os.Getenv("LIVE_LOG_BYTES")); err == nil && n > 0 {
		return n
	}
	return 700
}
