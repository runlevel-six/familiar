// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/runlevel-six/familiar/tools/internal/prom"
)

type fake map[string][]prom.Sample

func (f fake) Query(_ context.Context, q string) ([]prom.Sample, error) { return f[q], nil }

func s(v float64, kv ...string) prom.Sample {
	l := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		l[kv[i]] = kv[i+1]
	}
	return prom.Sample{Labels: l, Value: v}
}

func TestStatus(t *testing.T) {
	f := fake{
		`max(ceph_health_status)`:                           {s(1)},
		`max by (name, severity) (ceph_health_detail == 1)`: {s(1, "name", "RECENT_CRASH", "severity", "HEALTH_WARN")},
		`sum(ceph_osd_up)`:                                  {s(2)}, `count(ceph_osd_metadata)`: {s(2)},
		`sum(ceph_cluster_total_bytes)`: {s(100)}, `sum(ceph_cluster_total_used_bytes)`: {s(25)},
	}
	h := &handlers{q: f, now: time.Now}
	out, err := h.status(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	m := out.(map[string]any)
	if m["health"] != "HEALTH_WARN" || m["used_fraction"] != 0.25 || m["osds_up"] != 2.0 {
		t.Fatalf("status: %v", m)
	}
	if c := m["active_checks"].([]map[string]string); len(c) != 1 || c[0]["check"] != "RECENT_CRASH" {
		t.Fatalf("checks: %v", c)
	}
	if _, err := (&handlers{q: fake{}, now: time.Now}).status(context.Background(), json.RawMessage(`{}`)); err == nil || !strings.Contains(err.Error(), "no Ceph metrics") {
		t.Fatalf("missing metrics not reported: %v", err)
	}
}

func TestPoolsJoinNames(t *testing.T) {
	f := fake{
		`ceph_pool_metadata`:     {s(1, "pool_id", "1", "name", "ceph-blockpool")},
		`ceph_pool_percent_used`: {s(0.02, "pool_id", "1")},
	}
	out, _ := (&handlers{q: f, now: time.Now}).pools(context.Background(), json.RawMessage(`{}`))
	p := out.(map[string]any)["pools"].([]map[string]any)[0]
	if p["name"] != "ceph-blockpool" || p["percent_used"] != 0.02 {
		t.Fatalf("pool: %v", p)
	}
}

func TestOSDsCarryTheirDisk(t *testing.T) {
	f := fake{
		`ceph_osd_up`:                {s(1, "ceph_daemon", "osd.0"), s(1, "ceph_daemon", "osd.1")},
		`ceph_disk_occupation_human`: {s(1, "ceph_daemon", "osd.1", "device", "/dev/sdc"), s(1, "ceph_daemon", "osd.9", "device", "/dev/sdz")},
	}
	out, err := (&handlers{q: f, now: time.Now}).osds(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	o := out.(map[string]any)["osds"].([]map[string]any)
	if len(o) != 2 || o[1]["device"] != "sdc" || o[0]["device"] != nil {
		t.Fatalf("osds: %v", o)
	}
}
