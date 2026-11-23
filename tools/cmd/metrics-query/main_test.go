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
