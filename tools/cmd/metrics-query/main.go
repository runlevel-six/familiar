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

// familiar's own namespaces are invisible to the agent (design §3 T3, as in
// cluster-read): refused as a parameter, excluded in every namespace-scoped
// query, and filtered out of every result (hideFamiliar) in case a template
// forgets. familiarAlerts are the guardrails' own alerts, which the agent
// mustn't see either: they report on it.
const (
	hiddenNamespacePrefix = "familiar-"
	notFamiliar           = `namespace!~"familiar-.*"`
	familiarAlerts        = `Gideon.*|ModelGate.*|MemoryAuditor.*|Hermes.*`
)

func hiddenNamespace(ns string) bool { return strings.HasPrefix(ns, hiddenNamespacePrefix) }

func nsMatcher(ns string) string {
	if ns == "" {
		return `namespace!="",` + notFamiliar
	}
	return `namespace="` + ns + `"`
}

// flowSel selects Hubble series in namespace ns (as source or destination;
// "" = all) with familiar's namespaces excluded on both sides. Returns one
// selector, or two to be joined with `or` when ns is set.
func flowSel(ns string, extra string) []string {
	base := `source_namespace!~"familiar-.*",destination_namespace!~"familiar-.*"`
	if extra != "" {
		base += "," + extra
	}
	if ns == "" {
		return []string{base}
	}
	return []string{base + `,source_namespace="` + ns + `"`, base + `,destination_namespace="` + ns + `"`}
}

// sumFlows builds sum by (by) (increase(metric{sel}[w])) for each selector,
// joined with or, and keeps the top limit non-zero rows.
func sumFlows(metric, by string, sels []string, window string, limit int) string {
	parts := make([]string, len(sels))
	for i, sel := range sels {
		parts[i] = fmt.Sprintf(`sum by (%s) (increase(%s{%s}[%s]))`, by, metric, sel, window)
	}
	return fmt.Sprintf(`topk(%d, (%s) > 0)`, limit, strings.Join(parts, " or "))
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
	"node_filesystems": {Description: "Free fraction of each node filesystem (tmpfs and overlay excluded).",
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
			return `ALERTS{alertstate="firing", alertname!~"Watchdog|InfoInhibitor|` + familiarAlerts + `", ` + notFamiliar + `}`
		}},
	"targets_down": {Description: "Scrape targets that are down.",
		Build: func(params) string { return `up{` + notFamiliar + `} == 0` }},
	"backup_age_hours": {Description: "Hours since the last successful etcd snapshot and PVC backup.",
		Build: func(params) string { return fmt.Sprintf(`(time() - %s) / 3600`, backupMetric) }},
	"kubelet_restarts": {Description: "kubelet process restarts within the window.", Uses: []string{"window"},
		Build: func(p params) string {
			return fmt.Sprintf(`changes(process_start_time_seconds{job="kubelet", metrics_path="/metrics"}[%s])`, p.Window)
		}},
	// Network (Hubble flow metrics with namespace context, and the Cilium agent).
	"network_drops": {Description: "Packets Cilium dropped within the window, by source namespace, destination namespace and reason (empty namespace = the node or outside the cluster). VLAN_FILTERED on host traffic is usually LAN noise, not a fault.",
		Uses: []string{"namespace", "window", "limit"},
		Build: func(p params) string {
			return sumFlows("hubble_drop_total", "source_namespace, destination_namespace, reason", flowSel(p.Namespace, ""), p.Window, p.Limit)
		}},
	"network_policy_denials": {Description: "Connections a network policy denied within the window, by source and destination namespace.",
		Uses: []string{"namespace", "window", "limit"},
		Build: func(p params) string {
			return sumFlows("hubble_drop_total", "source_namespace, destination_namespace", flowSel(p.Namespace, `reason="POLICY_DENIED"`), p.Window, p.Limit)
		}},
	"network_flow_verdicts": {Description: "Flows Cilium processed within the window, by verdict (FORWARDED, DROPPED, ...); for one namespace, also by source and destination namespace.",
		Uses: []string{"namespace", "window"},
		Build: func(p params) string {
			by := "verdict" // cluster-wide
			if p.Namespace != "" {
				// keep both directions: "or" drops right-hand series whose labels match the left
				by = "source_namespace, destination_namespace, verdict"
			}
			return sumFlows("hubble_flows_processed_total", by, flowSel(p.Namespace, ""), p.Window, 20)
		}},
	"dns_errors": {Description: "DNS responses other than success within the window, by source namespace and response code.",
		Uses: []string{"namespace", "window", "limit"},
		Build: func(p params) string {
			base := `source_namespace!~"familiar-.*",rcode!="No Error"`
			if p.Namespace != "" {
				base += `,source_namespace="` + p.Namespace + `"`
			}
			return sumFlows("hubble_dns_responses_total", "source_namespace, rcode", []string{base}, p.Window, p.Limit)
		}},
	"cilium_health": {Description: "Cilium agent health: endpoints not ready (by state), failing controllers, unreachable nodes and health endpoints, the fullest BPF map (fraction), Hubble events lost and agent errors in the last hour.",
		Build: func(params) string {
			return strings.Join([]string{
				`label_replace(sum by (endpoint_state) (cilium_endpoint_state{endpoint_state!="ready"}), "check", "endpoints_not_ready", "", "")`,
				`label_replace(sum(cilium_controllers_failing), "check", "controllers_failing", "", "")`,
				`label_replace(sum(cilium_unreachable_nodes), "check", "unreachable_nodes", "", "")`,
				`label_replace(sum(cilium_unreachable_health_endpoints), "check", "unreachable_health_endpoints", "", "")`,
				`label_replace(max(cilium_bpf_map_pressure), "check", "bpf_map_pressure_max", "", "")`,
				`label_replace(sum(increase(hubble_lost_events_total[1h])), "check", "hubble_lost_events_1h", "", "")`,
				`label_replace(sum(increase(cilium_errors_warnings_total{level="error"}[1h])), "check", "agent_errors_1h", "", "")`,
			}, " or ")
		}},
}

// hideFamiliar drops results that belong to familiar's own namespaces or
// alerts. The queries already exclude them; this catches a template that
// doesn't.
func hideFamiliar(samples []prom.Sample) []prom.Sample {
	out := samples[:0]
	alerts := regexp.MustCompile(`^(` + familiarAlerts + `)$`)
	for _, s := range samples {
		l := s.Labels
		if hiddenNamespace(l["namespace"]) || hiddenNamespace(l["source_namespace"]) ||
			hiddenNamespace(l["destination_namespace"]) || alerts.MatchString(l["alertname"]) {
			continue
		}
		out = append(out, s)
	}
	return out
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
	if hiddenNamespace(p.Namespace) {
		return nil, mcp.Errorf("namespace %q is not visible to this tool (familiar's own namespaces are hidden)", p.Namespace)
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
	samples = hideFamiliar(samples)
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
