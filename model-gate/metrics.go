// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

// Metrics is a tiny Prometheus text-format registry (stdlib only).
type Metrics struct {
	mu       sync.Mutex
	counters map[string]float64 // "name{labels}" -> value
	help     map[string]string
}

func newMetrics() *Metrics {
	return &Metrics{counters: map[string]float64{}, help: map[string]string{
		"model_gate_requests_total":   "Requests handled, by role, outcome and upstream status class.",
		"model_gate_rejections_total": "Requests rejected by model-gate before reaching the API, by reason.",
		"model_gate_tokens_total":     "Tokens reported by the API, by role, model and kind.",
		"model_gate_cost_usd_total":   "Estimated spend in USD, by role.",
		"model_gate_unpriced_total":   "Responses that named a model with no configured price (cost overestimated).",
	}}
}

func labels(kv ...string) string {
	var b strings.Builder
	for i := 0; i+1 < len(kv); i += 2 {
		if i > 0 {
			b.WriteByte(',')
		}
		v := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(kv[i+1])
		fmt.Fprintf(&b, `%s="%s"`, kv[i], v)
	}
	return b.String()
}

func (m *Metrics) Add(name string, v float64, kv ...string) {
	m.mu.Lock()
	m.counters[name+"{"+labels(kv...)+"}"] += v
	m.mu.Unlock()
}

// Write renders counters plus budget gauges.
func (m *Metrics) Write(w io.Writer, cfg *Config, b *Budgets, now func() RoleSpendFor) {
	m.mu.Lock()
	keys := make([]string, 0, len(m.counters))
	for k := range m.counters {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	lastName := ""
	for _, k := range keys {
		name := k[:strings.IndexByte(k, '{')]
		if name != lastName {
			fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", name, m.help[name], name)
			lastName = name
		}
		fmt.Fprintf(w, "%s %g\n", k, m.counters[k])
	}
	m.mu.Unlock()

	fmt.Fprint(w, "# HELP model_gate_budget_spent_usd Spend in the current UTC window.\n# TYPE model_gate_budget_spent_usd gauge\n")
	fmt.Fprint(w, "# HELP model_gate_budget_limit_usd Configured budget for the window (0 = unlimited).\n# TYPE model_gate_budget_limit_usd gauge\n")
	for i := range cfg.Roles {
		r := &cfg.Roles[i]
		s := now()(r.Name)
		fmt.Fprintf(w, "model_gate_budget_spent_usd{%s} %g\n", labels("role", r.Name, "window", "hour"), s.HourUSD)
		fmt.Fprintf(w, "model_gate_budget_spent_usd{%s} %g\n", labels("role", r.Name, "window", "day"), s.DayUSD)
		fmt.Fprintf(w, "model_gate_budget_limit_usd{%s} %g\n", labels("role", r.Name, "window", "hour"), r.HourlyUSD)
		fmt.Fprintf(w, "model_gate_budget_limit_usd{%s} %g\n", labels("role", r.Name, "window", "day"), r.DailyUSD)
	}
}

// RoleSpendFor looks up a role's current spend.
type RoleSpendFor func(role string) RoleSpend
