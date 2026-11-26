// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/runlevel-six/familiar/tools/internal/prom"
)

type fakeProm struct{ queries []string }

func (f *fakeProm) Query(_ context.Context, q string) ([]prom.Sample, error) {
	f.queries = append(f.queries, q)
	return []prom.Sample{{Labels: map[string]string{"__name__": "x", "namespace": "shop", "pod": "web", "instance": "172.16.0.4:9100"}, Value: 3}}, nil
}
func (f *fakeProm) QueryRange(_ context.Context, q string, s, e time.Time, step time.Duration) ([]prom.Series, error) {
	f.queries = append(f.queries, q)
	return []prom.Series{{Labels: map[string]string{}, Points: [][2]float64{{1, 2}}}}, nil
}

func run(t *testing.T, f *fakeProm, tool string, args string) (any, error) {
	h := &handlers{q: f, now: time.Now}
	for _, tl := range h.tools() {
		if tl.Name == tool {
			return tl.Handler(context.Background(), json.RawMessage(args))
		}
	}
	t.Fatalf("no tool %s", tool)
	return nil, nil
}

func TestTemplatesBuildValidPromQL(t *testing.T) {
	f := &fakeProm{}
	for name := range templates {
		if _, err := run(t, f, "query_metric", `{"template":"`+name+`","namespace":"shop","window":"6h","limit":5}`); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	joined := strings.Join(f.queries, "\n")
	for _, want := range []string{`namespace="shop"`, `[6h]`, `topk(5,`} {
		if !strings.Contains(joined, want) {
			t.Errorf("queries missing %q", want)
		}
	}
}

func TestRejectsInjectionAndBadParams(t *testing.T) {
	f := &fakeProm{}
	for _, args := range []string{
		`{"template":"pod_restarts","namespace":"x\"} or up{a=\""}`,
		`{"template":"pod_restarts","namespace":"shop","window":"1h] or up[5m"}`,
		`{"template":"pod_restarts","window":"999d"}`,
		`{"template":"raw","query":"up"}`,
		`{"template":"up == 0"}`,
		`{"template":"firing_alerts","promql":"up"}`,
	} {
		if _, err := run(t, f, "query_metric", args); err == nil {
			t.Errorf("accepted %s", args)
		}
	}
	if len(f.queries) != 0 {
		t.Fatalf("rejected input reached Prometheus: %v", f.queries)
	}
	if _, err := run(t, f, "metric_history", `{"template":"pod_restarts"}`); err == nil {
		t.Error("history allowed for non-rangeable template")
	}
}

func TestResultShaping(t *testing.T) {
	out, err := run(t, &fakeProm{}, "query_metric", `{"template":"targets_down"}`)
	if err != nil {
		t.Fatal(err)
	}
	s := out.(map[string]any)["results"].([]prom.Sample)[0]
	if _, has := s.Labels["__name__"]; has {
		t.Error("__name__ not stripped")
	}
	out, err = run(t, &fakeProm{}, "metric_history", `{"template":"node_cpu_cores_used","hours":24}`)
	if err != nil || out.(map[string]any)["step_seconds"].(int) != 720 {
		t.Fatalf("history: %v %v", out, err)
	}
}

func TestBackupMetricSetting(t *testing.T) {
	defer func(old string) { backupMetric = old }(backupMetric)
	if got := templates["backup_age_hours"].Build(params{}); got != `(time() - backup_last_success_timestamp_seconds) / 3600` {
		t.Fatalf("default: %s", got)
	}
	if err := setBackupMetric("site_backup_last_success_timestamp_seconds"); err != nil {
		t.Fatal(err)
	}
	if got := templates["backup_age_hours"].Build(params{}); got != `(time() - site_backup_last_success_timestamp_seconds) / 3600` {
		t.Fatalf("override: %s", got)
	}
	for _, bad := range []string{"", "up) or vector(1", "x{a=\"b\"}", "1abc"} {
		if setBackupMetric(bad) == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

// familiar's own namespaces and alerts stay invisible (design §3 T3), as in cluster-read.
func TestFamiliarHidden(t *testing.T) {
	f := &fakeProm{}
	for _, args := range []string{
		`{"template":"pod_restarts","namespace":"familiar-agents"}`,
		`{"template":"network_drops","namespace":"familiar-system"}`,
		`{"template":"top_container_memory","namespace":"familiar-tools"}`,
	} {
		_, err := run(t, f, "query_metric", args)
		if err == nil || !strings.Contains(err.Error(), "not visible") {
			t.Errorf("%s: err %v", args, err)
		}
	}
	if len(f.queries) != 0 {
		t.Fatalf("refused namespace reached Prometheus: %v", f.queries)
	}
	// Every template that takes a namespace excludes familiar's when it isn't given one.
	for name, tm := range templates {
		if !contains(tm.Uses, "namespace") {
			continue
		}
		q := tm.Build(params{Window: "1h", Limit: 10})
		if !strings.Contains(q, `namespace!~"familiar-.*"`) {
			t.Errorf("%s doesn't exclude familiar namespaces: %s", name, q)
		}
	}
	for _, name := range []string{"firing_alerts", "targets_down"} {
		if q := templates[name].Build(params{}); !strings.Contains(q, `namespace!~"familiar-.*"`) {
			t.Errorf("%s: %s", name, q)
		}
	}
	if q := templates["firing_alerts"].Build(params{}); !strings.Contains(q, "Gideon.*") || !strings.Contains(q, "ModelGate.*") {
		t.Errorf("firing_alerts keeps familiar's alerts: %s", q)
	}
}

// self_usage is the one window into familiar's namespaces: only the Hermes
// pod, always summed so no familiar-* label comes back.
func TestSelfUsageScope(t *testing.T) {
	for name, tm := range templates {
		q := tm.Build(params{Window: "1h", Limit: 10})
		n := strings.Count(q, `namespace="familiar-`)
		if name != "self_usage" {
			if n != 0 {
				t.Errorf("%s selects a familiar namespace: %s", name, q)
			}
			continue
		}
		if n != 3 || strings.Count(q, selfPod) != 2 || strings.Count(q, selfContainer) != 1 {
			t.Errorf("self_usage selects more than the Hermes container: %s", q)
		}
		if strings.Contains(q, "sum by") {
			t.Errorf("self_usage keeps labels: %s", q)
		}
	}
}

type leakyProm struct{ fakeProm }

func (l *leakyProm) Query(_ context.Context, q string) ([]prom.Sample, error) {
	return []prom.Sample{
		{Labels: map[string]string{"namespace": "shop", "pod": "web"}, Value: 1},
		{Labels: map[string]string{"namespace": "familiar-agents", "pod": "hermes-1"}, Value: 1},
		{Labels: map[string]string{"source_namespace": "familiar-agents", "destination_namespace": "kube-system"}, Value: 1},
		{Labels: map[string]string{"source_namespace": "shop", "destination_namespace": "familiar-system"}, Value: 1},
		{Labels: map[string]string{"alertname": "GideonMemoryChanged"}, Value: 1},
		{Labels: map[string]string{"alertname": "ModelGatePolicyRejections", "role": "watch-officer"}, Value: 1},
		{Labels: map[string]string{"alertname": "KubePodCrashLooping", "namespace": "shop"}, Value: 1},
		// Operational alerts about familiar are visible, unless they mention the auditor.
		{Labels: map[string]string{"alertname": "KubeQuotaFullyUsed", "namespace": "familiar-agents", "resourcequota": "quota"}, Value: 1},
		{Labels: map[string]string{"alertname": "HermesRestarting", "namespace": "familiar-agents"}, Value: 1},
		{Labels: map[string]string{"alertname": "KubePodCrashLooping", "namespace": "familiar-agents", "pod": "memory-auditor-7d-x"}, Value: 1},
		{Labels: map[string]string{"alertname": "KubePodNotReady", "namespace": "familiar-agents", "container": "Auditor"}, Value: 1},
		{Labels: map[string]string{"alertname": "MemoryAuditorStale", "namespace": "familiar-agents"}, Value: 1},
		{Labels: map[string]string{"alertname": "ModelGatePolicyRejections", "namespace": "familiar-system"}, Value: 1},
	}, nil
}

// Results are filtered too, so a template that forgets the exclusion can't leak.
func TestFamiliarFilteredFromResults(t *testing.T) {
	h := &handlers{q: &leakyProm{}, now: time.Now}
	var tool func(context.Context, json.RawMessage) (any, error)
	for _, tl := range h.tools() {
		if tl.Name == "query_metric" {
			tool = tl.Handler
		}
	}
	out, err := tool(context.Background(), json.RawMessage(`{"template":"firing_alerts"}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(out)
	var got struct {
		Results []struct{ Labels map[string]string } `json:"results"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	var seen []string
	for _, r := range got.Results {
		seen = append(seen, r.Labels["alertname"]+"@"+r.Labels["namespace"]+r.Labels["source_namespace"])
	}
	sort.Strings(seen)
	want := []string{"@shop", "HermesRestarting@familiar-agents", "KubePodCrashLooping@shop", "KubeQuotaFullyUsed@familiar-agents"}
	if strings.Join(seen, " ") != strings.Join(want, " ") {
		t.Errorf("visible series:\n got %v\nwant %v", seen, want)
	}
	if strings.Contains(strings.ToLower(string(b)), "auditor") || strings.Contains(string(b), "Gideon") || strings.Contains(string(b), "ModelGatePolicy") {
		t.Errorf("result leaks detection state: %s", b)
	}
}

type emptyProm struct{}

func (emptyProm) Query(context.Context, string) ([]prom.Sample, error) { return nil, nil }
func (emptyProm) QueryRange(context.Context, string, time.Time, time.Time, time.Duration) ([]prom.Series, error) {
	return nil, nil
}

// alert-gate.py iterates "results"; null crashed it when nothing was firing.
func TestEmptyResultsAreArrays(t *testing.T) {
	h := &handlers{q: emptyProm{}, now: time.Now}
	for _, tl := range h.tools() {
		args := `{"template":"firing_alerts"}`
		if tl.Name == "metric_history" {
			args = `{"template":"node_cpu_cores_used"}`
		}
		out, err := tl.Handler(context.Background(), json.RawMessage(args))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(out)
		if strings.Contains(string(b), "null") {
			t.Errorf("%s: empty result has null: %s", tl.Name, b)
		}
	}
}
