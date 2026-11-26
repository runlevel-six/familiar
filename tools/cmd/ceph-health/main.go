// SPDX-License-Identifier: Apache-2.0

// ceph-health is a read-only MCP tool server for Ceph status, built only on
// the Ceph metrics already in Prometheus (design §4.1): it never holds a
// Ceph keyring or touches the Rook toolbox.
package main

import (
	"context"
	"encoding/json"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/runlevel-six/familiar/tools/internal/mcp"
	"github.com/runlevel-six/familiar/tools/internal/prom"
	"github.com/runlevel-six/familiar/tools/internal/redact"
)

var version = "dev"

// Querier is the subset of prom.Client used here.
type Querier interface {
	Query(ctx context.Context, q string) ([]prom.Sample, error)
}

type handlers struct {
	q   Querier
	now func() time.Time
}

var healthNames = map[float64]string{0: "HEALTH_OK", 1: "HEALTH_WARN", 2: "HEALTH_ERR"}

func (h *handlers) scalar(ctx context.Context, q string) (float64, bool, error) {
	s, err := h.q.Query(ctx, q)
	if err != nil || len(s) == 0 {
		return 0, false, err
	}
	return s[0].Value, true, nil
}

func (h *handlers) status(ctx context.Context, args json.RawMessage) (any, error) {
	var in struct{}
	if err := mcp.Decode(args, &in); err != nil {
		return nil, err
	}
	out := map[string]any{"at": h.now().UTC().Format(time.RFC3339)}

	v, ok, err := h.scalar(ctx, `max(ceph_health_status)`)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, mcp.Errorf("no Ceph metrics in Prometheus (is the rook-ceph-mgr ServiceMonitor scraping?)")
	}
	out["health"] = healthNames[v]

	// ceph_health_detail keeps a series for every check ever seen; 1 = active.
	checks, err := h.q.Query(ctx, `max by (name, severity) (ceph_health_detail == 1)`)
	if err != nil {
		return nil, err
	}
	var active []map[string]string
	for _, c := range checks {
		active = append(active, map[string]string{"check": c.Labels["name"], "severity": c.Labels["severity"]})
	}
	sort.Slice(active, func(i, j int) bool { return active[i]["check"] < active[j]["check"] })
	out["active_checks"] = active

	for key, q := range map[string]string{
		"mons_in_quorum": `sum(ceph_mon_quorum_status)`, "mons_total": `count(ceph_mon_metadata)`,
		"osds_up": `sum(ceph_osd_up)`, "osds_in": `sum(ceph_osd_in)`, "osds_total": `count(ceph_osd_metadata)`,
		"pgs_total": `sum(ceph_pg_total)`, "pgs_active": `sum(ceph_pg_active)`, "pgs_clean": `sum(ceph_pg_clean)`,
		"pgs_degraded": `sum(ceph_pg_degraded)`, "pgs_undersized": `sum(ceph_pg_undersized)`, "pgs_inconsistent": `sum(ceph_pg_inconsistent)`,
		"pgs_down": `sum(ceph_pg_down)`, "pgs_incomplete": `sum(ceph_pg_incomplete)`, "pgs_stale": `sum(ceph_pg_stale)`,
		"capacity_bytes": `sum(ceph_cluster_total_bytes)`, "used_bytes": `sum(ceph_cluster_total_used_bytes)`,
	} {
		if v, ok, err := h.scalar(ctx, q); err != nil {
			return nil, err
		} else if ok {
			out[key] = v
		}
	}
	if c, ok := out["capacity_bytes"].(float64); ok && c > 0 {
		out["used_fraction"] = out["used_bytes"].(float64) / c
	}
	return out, nil
}

func (h *handlers) osds(ctx context.Context, args json.RawMessage) (any, error) {
	var in struct{}
	if err := mcp.Decode(args, &in); err != nil {
		return nil, err
	}
	rows := map[string]map[string]any{}
	row := func(d string) map[string]any {
		if rows[d] == nil {
			rows[d] = map[string]any{"osd": d}
		}
		return rows[d]
	}
	for key, q := range map[string]string{
		"up": `ceph_osd_up`, "in": `ceph_osd_in`,
		"used_fraction":    `ceph_osd_stat_bytes_used / ceph_osd_stat_bytes`,
		"size_bytes":       `ceph_osd_stat_bytes`,
		"apply_latency_ms": `ceph_osd_apply_latency_ms`, "commit_latency_ms": `ceph_osd_commit_latency_ms`,
	} {
		s, err := h.q.Query(ctx, q)
		if err != nil {
			return nil, err
		}
		for _, x := range s {
			row(x.Labels["ceph_daemon"])[key] = x.Value
		}
	}
	// The disk each OSD sits on, as Ceph itself reports it, so the OSD can be
	// matched with the host's per-disk metrics (metrics-query disk_io).
	occ, err := h.q.Query(ctx, `ceph_disk_occupation_human`)
	if err != nil {
		return nil, err
	}
	for _, x := range occ {
		if d := strings.TrimPrefix(x.Labels["device"], "/dev/"); d != "" && rows[x.Labels["ceph_daemon"]] != nil {
			rows[x.Labels["ceph_daemon"]]["device"] = d
		}
	}
	var out []map[string]any
	for _, r := range rows {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["osd"].(string) < out[j]["osd"].(string) })
	return map[string]any{"osds": out}, nil
}

func (h *handlers) pools(ctx context.Context, args json.RawMessage) (any, error) {
	var in struct{}
	if err := mcp.Decode(args, &in); err != nil {
		return nil, err
	}
	names := map[string]string{}
	meta, err := h.q.Query(ctx, `ceph_pool_metadata`)
	if err != nil {
		return nil, err
	}
	for _, m := range meta {
		names[m.Labels["pool_id"]] = m.Labels["name"]
	}
	rows := map[string]map[string]any{}
	for key, q := range map[string]string{
		"percent_used": `ceph_pool_percent_used`, "stored_bytes": `ceph_pool_stored`, "max_avail_bytes": `ceph_pool_max_avail`,
		// Client I/O over 5 minutes, from Ceph's own per-pool counters.
		"write_ops_per_s": `rate(ceph_pool_wr[5m])`, "write_mb_per_s": `rate(ceph_pool_wr_bytes[5m]) / 1e6`,
		"read_ops_per_s": `rate(ceph_pool_rd[5m])`, "read_mb_per_s": `rate(ceph_pool_rd_bytes[5m]) / 1e6`,
	} {
		s, err := h.q.Query(ctx, q)
		if err != nil {
			return nil, err
		}
		for _, x := range s {
			id := x.Labels["pool_id"]
			if rows[id] == nil {
				rows[id] = map[string]any{"pool_id": id, "name": names[id]}
			}
			rows[id][key] = x.Value
		}
	}
	var out []map[string]any
	for _, r := range rows {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["pool_id"].(string) < out[j]["pool_id"].(string) })
	return map[string]any{"pools": out}, nil
}

func (h *handlers) tools() []mcp.Tool {
	empty := mcp.Object(map[string]any{})
	return []mcp.Tool{
		{Name: "ceph_status", Title: "Ceph status", InputSchema: empty, Handler: h.status,
			Description: "Overall Ceph health, active health checks, mon quorum, OSD up/in counts, placement group states and capacity. On a single-host cluster, replicas are spread across OSDs rather than hosts, so the host itself is a single point of failure."},
		{Name: "ceph_osds", Title: "Ceph OSDs", InputSchema: empty, Handler: h.osds,
			Description: "Per-OSD up/in state, fullness, size, apply/commit latency, and the host disk it sits on (device, e.g. sdc: the name metrics-query disk_io uses)."},
		{Name: "ceph_pools", Title: "Ceph pools", InputSchema: empty, Handler: h.pools,
			Description: "Per pool: used fraction, stored bytes, space still available, and client reads and writes over the last 5 minutes (ops/s and MB/s). For which volume or pod does the I/O, see host-read volume_io."},
	}
}

func main() {
	log := mcp.NewLogger()
	h := &handlers{q: prom.New(mcp.Env("PROMETHEUS_URL", "http://kps-prometheus.monitoring.svc:9090")), now: time.Now}
	s := &mcp.Server{
		Name: "ceph-health", Version: version, Log: log, RatePerMinute: 30,
		Instructions: "Read-only Ceph health from Prometheus metrics. No keyring; cannot change anything.",
		Tools:        h.tools(),
		Redact:       redact.Options{IPs: true},
	}
	if err := mcp.Serve(s, mcp.Env("LISTEN", ":8080")); err != nil {
		log.Error("fatal", "err", err.Error())
		os.Exit(1)
	}
}
