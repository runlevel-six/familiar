// SPDX-License-Identifier: Apache-2.0

//go:build live

package main

import (
	"context"
	"encoding/json"
	"os"
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
	h := &handlers{q: prom.New(base), now: time.Now}
	for _, tl := range h.tools() {
		for _, args := range liveArgs[tl.Name] {
			out, err := tl.Handler(context.Background(), json.RawMessage(args))
			if err != nil {
				t.Errorf("%s %s: %v", tl.Name, args, err)
				continue
			}
			b, _ := json.Marshal(out)
			if len(b) > 700 {
				b = append(b[:700], "…"...)
			}
			t.Logf("%s %s -> %s", tl.Name, args, b)
		}
	}
}

var liveArgs = map[string][]string{"ceph_status": {`{}`}, "ceph_osds": {`{}`}, "ceph_pools": {`{}`}}
