// SPDX-License-Identifier: Apache-2.0

// metrics-query is a read-only MCP tool server over Prometheus (design
// §4.1). The agent picks from allowlisted query templates with validated
// parameters; it cannot send arbitrary PromQL.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/runlevel-six/familiar/tools/internal/mcp"
	"github.com/runlevel-six/familiar/tools/internal/prom"
	"github.com/runlevel-six/familiar/tools/internal/redact"
)

var version = "dev"

// Querier is the subset of prom.Client the tools use.
type Querier interface {
	Query(ctx context.Context, q string) ([]prom.Sample, error)
	QueryRange(ctx context.Context, q string, start, end time.Time, step time.Duration) ([]prom.Series, error)
}

// template parameters, all validated before substitution
type params struct {
	Namespace string // "" = all
	Window    string
	Limit     int
}

type tmpl struct {
	Description string
	Uses        []string // which params apply
	Rangeable   bool
	Build       func(p params) string
}

var (
	reNS     = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	windows  = []string{"5m", "15m", "1h", "6h", "24h"}
	nsSel    = func(p params) string { return nsMatcher(p.Namespace) }
	topLimit = func(p params) int { return p.Limit }
)

func nsMatcher(ns string) string {
	if ns == "" {
		return `namespace!=""`
	}
	return `namespace="` + ns + `"`
}

// backupMetric is the gauge the host backup jobs write (a Unix timestamp per
// backup kind). Site-specific, so it's set by BACKUP_METRIC; validated as a
// bare Prometheus metric name so the setting can't inject PromQL.
var (
	backupMetric = "backup_last_success_timestamp_seconds"
	reMetricName = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
)

func setBackupMetric(name string) error {
	if !reMetricName.MatchString(name) {
		return fmt.Errorf("BACKUP_METRIC %q is not a metric name", name)
	}
	backupMetric = name
	return nil
}

var templates = map[string]tmpl{
	"node_cpu_cores_used": {Description: "CPU cores in use on the node (all non-idle modes).", Rangeable: true,
		Build: func(params) string { return `sum(rate(node_cpu_seconds_total{mode!="idle"}[5m]))` }},
	"node_memory": {Description: "Node memory: total, available and used fraction.",
		Build: func(params) string {
			return `label_replace(node_memory_MemTotal_bytes, "metric", "total_bytes", "", "") or label_replace(node_memory_MemAvailable_bytes, "metric", "available_bytes", "", "") or label_replace(1 - node_memory_MemAvailable_bytes / node_memory_MemTotal_bytes, "metric", "used_fraction", "", "")`
		}},
	"node_memory_used_fraction": {Description: "Fraction of node memory in use.", Rangeable: true,
		Build: func(params) string { return `1 - node_memory_MemAvailable_bytes / node_memory_MemTotal_bytes` }},
	"node_filesystems": {Description: "Free fraction of each node filesystem (/storage is excluded).",
		Build: func(params) string {
			return `node_filesystem_avail_bytes{fstype!~"tmpfs|overlay"} / node_filesystem_size_bytes{fstype!~"tmpfs|overlay"}`
		}},
	"cpu_requests_vs_allocatable": {Description: "Requested CPU cores (running pods) and the node's allocatable CPU.",
		Build: func(params) string {
			return `label_replace(sum(kube_pod_container_resource_requests{resource="cpu"} * on(namespace,pod) group_left() (kube_pod_status_phase{phase="Running"} == 1)), "metric", "requested_cores", "", "") or label_replace(sum(kube_node_status_allocatable{resource="cpu"}), "metric", "allocatable_cores", "", "")`
		}},
	"pod_restarts": {Description: "Containers that restarted within the window.", Uses: []string{"namespace", "window"},
		Build: func(p params) string {
			return fmt.Sprintf(`sum by (namespace, pod, container) (increase(kube_pod_container_status_restarts_total{%s}[%s])) > 0`, nsSel(p), p.Window)
		}},
	"top_container_cpu": {Description: "Pods using the most CPU (cores, 5m rate).", Uses: []string{"namespace", "limit"},
		Build: func(p params) string {
			return fmt.Sprintf(`topk(%d, sum by (namespace, pod) (rate(container_cpu_usage_seconds_total{container!="",%s}[5m])))`, topLimit(p), nsSel(p))
		}},
	"top_container_memory": {Description: "Pods using the most memory (working set bytes).", Uses: []string{"namespace", "limit"},
		Build: func(p params) string {
			return fmt.Sprintf(`topk(%d, sum by (namespace, pod) (container_memory_working_set_bytes{container!="",%s}))`, topLimit(p), nsSel(p))
		}},
	"cpu_throttling": {Description: "Containers throttled more than 25% of CFS periods over 5m.", Uses: []string{"namespace"},
		Build: func(p params) string {
			return fmt.Sprintf(`sum by (namespace, pod, container) (rate(container_cpu_cfs_throttled_periods_total{%[1]s}[5m])) / sum by (namespace, pod, container) (rate(container_cpu_cfs_periods_total{%[1]s}[5m])) > 0.25`, nsSel(p))
		}},
	"pvc_usage": {Description: "Used fraction of each mounted PVC.", Uses: []string{"namespace"},
		Build: func(p params) string {
			return fmt.Sprintf(`kubelet_volume_stats_used_bytes{%[1]s} / kubelet_volume_stats_capacity_bytes{%[1]s}`, nsSel(p))
		}},
	"firing_alerts": {Description: "Alerts currently firing in Prometheus (Watchdog and InfoInhibitor excluded).",
		Build: func(params) string {
			return `ALERTS{alertstate="firing", alertname!~"Watchdog|InfoInhibitor"}`
		}},
	"targets_down": {Description: "Scrape targets that are down.",
		Build: func(params) string { return `up == 0` }},
	"backup_age_hours": {Description: "Hours since the last successful etcd snapshot and PVC backup.",
		Build: func(params) string { return fmt.Sprintf(`(time() - %s) / 3600`, backupMetric) }},
	"kubelet_restarts": {Description: "kubelet process restarts within the window.", Uses: []string{"window"},
		Build: func(p params) string {
			return fmt.Sprintf(`changes(process_start_time_seconds{job="kubelet", metrics_path="/metrics"}[%s])`, p.Window)
		}},
}

type handlers struct {
	q   Querier
	now func() time.Time
}

func (h *handlers) tools() []mcp.Tool {
	names := make([]string, 0, len(templates))
	for n := range templates {
		names = append(names, n)
	}
	sort.Strings(names)
	var lines []string
	var rangeable []string
	for _, n := range names {
		t := templates[n]
		uses := ""
		if len(t.Uses) > 0 {
			uses = " (params: " + strings.Join(t.Uses, ", ") + ")"
		}
		lines = append(lines, "- "+n+": "+t.Description+uses)
		if t.Rangeable {
			rangeable = append(rangeable, n)
		}
	}
	return []mcp.Tool{
		{
			Name: "query_metric", Title: "Query a metric template",
			Description: "Run one allowlisted Prometheus query (instant value). Arbitrary PromQL is not accepted. Templates:\n" + strings.Join(lines, "\n"),
			InputSchema: mcp.Object(map[string]any{
				"template":  mcp.Enum("Template name", names...),
				"namespace": mcp.Str("Namespace filter (templates that take it; omit for all)"),
				"window":    mcp.Enum("Look-back window (templates that take it; default 1h)", windows...),
				"limit":     mcp.Int("Top-N size (templates that take it; default 10)", 1, 20),
			}, "template"),
			Handler: h.query,
		},
		{
			Name: "metric_history", Title: "Metric history",
			Description: "Values over the last N hours (at most 120 points) for: " + strings.Join(rangeable, ", ") + ".",
			InputSchema: mcp.Object(map[string]any{
				"template": mcp.Enum("Template name", rangeable...),
				"hours":    mcp.Int("How many hours back (default 6)", 1, 168),
			}, "template"),
			Handler: h.history,
		},
	}
}

func (h *handlers) query(ctx context.Context, args json.RawMessage) (any, error) {
	var in struct {
		Template  string `json:"template"`
		Namespace string `json:"namespace"`
		Window    string `json:"window"`
		Limit     int    `json:"limit"`
	}
	if err := mcp.Decode(args, &in); err != nil {
		return nil, err
	}
	t, ok := templates[in.Template]
	if !ok {
		return nil, mcp.Errorf("unknown template %q", in.Template)
	}
	p := params{Namespace: in.Namespace, Window: in.Window, Limit: in.Limit}
	if p.Namespace != "" && !reNS.MatchString(p.Namespace) {
		return nil, mcp.Errorf("invalid namespace %q", p.Namespace)
	}
	if p.Window == "" {
		p.Window = "1h"
	}
	if !contains(windows, p.Window) {
		return nil, mcp.Errorf("window must be one of %v", windows)
	}
	if p.Limit <= 0 || p.Limit > 20 {
		p.Limit = 10
	}
	samples, err := h.q.Query(ctx, t.Build(p))
	if err != nil {
		return nil, err
	}
	if len(samples) > 200 {
		samples = samples[:200]
	}
	for i := range samples {
		for _, l := range []string{"__name__", "endpoint", "service", "instance", "job", "metrics_path", "prometheus"} {
			delete(samples[i].Labels, l)
		}
	}
	return map[string]any{"template": in.Template, "description": t.Description, "at": h.now().UTC().Format(time.RFC3339), "results": samples, "count": len(samples)}, nil
}

func (h *handlers) history(ctx context.Context, args json.RawMessage) (any, error) {
	var in struct {
		Template string `json:"template"`
		Hours    int    `json:"hours"`
	}
	if err := mcp.Decode(args, &in); err != nil {
		return nil, err
	}
	t, ok := templates[in.Template]
	if !ok || !t.Rangeable {
		return nil, mcp.Errorf("template %q has no history", in.Template)
	}
	if in.Hours <= 0 || in.Hours > 168 {
		in.Hours = 6
	}
	end := h.now()
	start := end.Add(-time.Duration(in.Hours) * time.Hour)
	step := max(time.Duration(in.Hours)*time.Hour/120, time.Minute)
	series, err := h.q.QueryRange(ctx, t.Build(params{}), start, end, step)
	if err != nil {
		return nil, err
	}
	return map[string]any{"template": in.Template, "hours": in.Hours, "step_seconds": int(step.Seconds()), "series": series}, nil
}

func contains(l []string, s string) bool {
	return slices.Contains(l, s)
}

func main() {
	log := mcp.NewLogger()
	if err := setBackupMetric(mcp.Env("BACKUP_METRIC", backupMetric)); err != nil {
		log.Error("fatal", "err", err.Error())
		os.Exit(1)
	}
	h := &handlers{q: prom.New(mcp.Env("PROMETHEUS_URL", "http://kps-prometheus.monitoring.svc:9090")), now: time.Now}
	s := &mcp.Server{
		Name: "metrics-query", Version: version, Log: log, RatePerMinute: 30,
		Instructions: "Read-only Prometheus metrics for the cluster through fixed query templates.",
		Tools:        h.tools(),
		Redact:       redact.Options{IPs: true},
	}
	if err := mcp.Serve(s, mcp.Env("LISTEN", ":8080")); err != nil {
		log.Error("fatal", "err", err.Error())
		os.Exit(1)
	}
}
