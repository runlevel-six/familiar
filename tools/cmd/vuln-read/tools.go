// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/runlevel-six/familiar/tools/internal/mcp"
)

type handlers struct {
	ix  *index
	now func() time.Time
}

func newHandlers(ix *index, now func() time.Time) *handlers { return &handlers{ix: ix, now: now} }

var (
	reNamespace = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	reWorkload  = regexp.MustCompile(`^([a-z]+/)?[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`)
)

// scope is said in every result, so the agent can't mistake what the scan
// covers.
const scope = "From the Trivy Operator's scans of running images (rescanned daily and on every rollout). " +
	"Only MEDIUM and above, plus findings the distribution hasn't rated, are recorded; LOW is not. " +
	"severity is the scanner's rating, or for unrated findings the CVSS band (vendor_severity UNKNOWN). " +
	"Findings say a vulnerable package is present, not that it is reachable: combine with exposure " +
	"(metrics-query port_forwards, cluster-read ingresses) before calling one urgent. " +
	"A running container with no report (containers_not_scanned) has unknown vulnerabilities: not scanned yet, " +
	"or its report was too large to store. Never describe one as clean."

func (h *handlers) tools() []mcp.Tool {
	nsProp := mcp.Str("Namespace (familiar-* namespaces are not visible)")
	sevProp := mcp.Enum("Lowest effective severity to include (default high)", "critical", "high", "medium")
	return []mcp.Tool{
		{
			Name: "vulnerability_summary", Title: "Vulnerability summary",
			Description: "Per scanned container (namespace, workload, container, image): counts of CRITICAL, HIGH and MEDIUM findings, how many CRITICAL and HIGH have a fixed version available, how many of those are unrated by the distribution and ranked by CVSS instead, and when it was scanned. Worst first. Also every running container that has no report (containers_not_scanned). Start here. " + scope,
			InputSchema: mcp.Object(map[string]any{
				"namespace": nsProp,
				"limit":     mcp.Int("Maximum containers (default 30)", 1, 100),
			}),
			Handler: h.summary,
		},
		{
			Name: "list_vulnerabilities", Title: "List vulnerabilities",
			Description: "The findings for one namespace, or one workload in it: vulnerability ID, effective and vendor severity, CVSS score, package, installed version, fixed version (empty: no fix released) and publication date; worst first. Also the running containers in that scope with no report (containers_not_scanned). " + scope,
			InputSchema: mcp.Object(map[string]any{
				"namespace":    nsProp,
				"workload":     mcp.Str("Workload, as vulnerability_summary names it (deployment/web) or just its name (web); omit for the whole namespace"),
				"min_severity": sevProp,
				"fixable_only": mcp.Bool("Only findings with a fixed version available (default false)"),
				"limit":        mcp.Int("Maximum findings (default 40)", 1, 200),
			}, "namespace"),
			Handler: h.list,
		},
		{
			Name: "find_vulnerability", Title: "Find a vulnerability",
			Description: "Which scanned containers have one vulnerability (by ID, e.g. CVE-2026-12345 or GHSA-xxxx-xxxx-xxxx), with the package, installed and fixed versions in each. " + scope,
			InputSchema: mcp.Object(map[string]any{
				"id": mcp.Str("Vulnerability ID"),
			}, "id"),
			Handler: h.find,
		},
	}
}

func checkNamespace(ns string, optional bool) error {
	if ns == "" && optional {
		return nil
	}
	if !reNamespace.MatchString(ns) {
		return mcp.Errorf("invalid namespace %q", ns)
	}
	if hiddenNamespace(ns) {
		return mcp.Errorf("namespace %q is not visible to this tool (familiar's own namespaces are hidden)", ns)
	}
	return nil
}

// latest keeps one target per namespace/workload/container: the newest
// scan (a CronJob's jobs, or a rollout's old and new ReplicaSets, can each
// leave a report).
func latest(ts []target) []target {
	byKey := map[string]target{}
	for _, t := range ts {
		k := t.Namespace + "/" + t.Workload + "/" + t.Container
		if old, ok := byKey[k]; !ok || t.ScannedAt.After(old.ScannedAt) {
			byKey[k] = t
		}
	}
	out := make([]target, 0, len(byKey))
	for _, t := range byKey {
		out = append(out, t)
	}
	return out
}

type summaryRow struct {
	Namespace       string `json:"namespace"`
	Workload        string `json:"workload"`
	Container       string `json:"container"`
	Image           string `json:"image"`
	OS              string `json:"os,omitempty"`
	ScannedAt       string `json:"scanned_at"`
	Critical        int    `json:"critical"`
	High            int    `json:"high"`
	Medium          int    `json:"medium"`
	FixableCritical int    `json:"fixable_critical"`
	FixableHigh     int    `json:"fixable_high"`
	UnratedSerious  int    `json:"unrated_critical_or_high"`
}

func (h *handlers) summary(ctx context.Context, args json.RawMessage) (any, error) {
	var in struct {
		Namespace string `json:"namespace"`
		Limit     int    `json:"limit"`
	}
	if err := mcp.Decode(args, &in); err != nil {
		return nil, err
	}
	if err := checkNamespace(in.Namespace, true); err != nil {
		return nil, err
	}
	if in.Limit <= 0 || in.Limit > 100 {
		in.Limit = 30
	}
	ts, run, at, err := h.ix.getAll(ctx)
	if err != nil {
		return nil, err
	}
	missing := []running{}
	for _, r := range notScanned(latest(ts), run) {
		if in.Namespace == "" || r.Namespace == in.Namespace {
			missing = append(missing, r)
		}
	}
	rows := []summaryRow{}
	clean := 0
	for _, t := range latest(ts) {
		if in.Namespace != "" && t.Namespace != in.Namespace {
			continue
		}
		r := summaryRow{Namespace: t.Namespace, Workload: t.Workload, Container: t.Container, Image: t.Image, OS: t.OS, ScannedAt: t.ScannedAt.UTC().Format(time.RFC3339)}
		for _, v := range t.Vulns {
			switch v.Severity {
			case "CRITICAL":
				r.Critical++
				if v.Fixed != "" {
					r.FixableCritical++
				}
			case "HIGH":
				r.High++
				if v.Fixed != "" {
					r.FixableHigh++
				}
			case "MEDIUM":
				r.Medium++
			}
			if v.VendorSeverity == "UNKNOWN" && (v.Severity == "CRITICAL" || v.Severity == "HIGH") {
				r.UnratedSerious++
			}
		}
		if r.Critical+r.High+r.Medium == 0 {
			clean++
		}
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.Critical != b.Critical {
			return a.Critical > b.Critical
		}
		if a.High != b.High {
			return a.High > b.High
		}
		if a.Medium != b.Medium {
			return a.Medium > b.Medium
		}
		return a.Namespace+a.Workload+a.Container < b.Namespace+b.Workload+b.Container
	})
	total := len(rows)
	if len(rows) > in.Limit {
		rows = rows[:in.Limit]
	}
	return map[string]any{
		"index_built": at.UTC().Format(time.RFC3339), "containers_scanned": total, "containers_without_findings": clean,
		"containers": rows, "truncated": total > len(rows), "containers_not_scanned": missing, "scope": scope,
	}, nil
}

type finding struct {
	Namespace string `json:"namespace"`
	Workload  string `json:"workload"`
	Container string `json:"container"`
	Image     string `json:"image"`
	vuln
}

func matchesWorkload(t target, w string) bool {
	if w == "" {
		return true
	}
	if strings.Contains(w, "/") {
		return t.Workload == w
	}
	_, name, _ := strings.Cut(t.Workload, "/")
	return name == w
}

func sortFindings(fs []finding) {
	sort.SliceStable(fs, func(i, j int) bool {
		a, b := fs[i], fs[j]
		if sevRank[a.Severity] != sevRank[b.Severity] {
			return sevRank[a.Severity] > sevRank[b.Severity]
		}
		as, bs := 0.0, 0.0
		if a.CVSS != nil {
			as = *a.CVSS
		}
		if b.CVSS != nil {
			bs = *b.CVSS
		}
		if as != bs {
			return as > bs
		}
		return a.ID < b.ID
	})
}

func (h *handlers) list(ctx context.Context, args json.RawMessage) (any, error) {
	var in struct {
		Namespace   string `json:"namespace"`
		Workload    string `json:"workload"`
		MinSeverity string `json:"min_severity"`
		FixableOnly bool   `json:"fixable_only"`
		Limit       int    `json:"limit"`
	}
	if err := mcp.Decode(args, &in); err != nil {
		return nil, err
	}
	if err := checkNamespace(in.Namespace, false); err != nil {
		return nil, err
	}
	if in.Workload != "" && !reWorkload.MatchString(in.Workload) {
		return nil, mcp.Errorf("invalid workload %q", in.Workload)
	}
	min := map[string]int{"": 3, "high": 3, "critical": 4, "medium": 2}
	minRank, ok := min[in.MinSeverity]
	if !ok {
		return nil, mcp.Errorf("min_severity must be critical, high or medium")
	}
	if in.Limit <= 0 || in.Limit > 200 {
		in.Limit = 40
	}
	ts, run, at, err := h.ix.getAll(ctx)
	if err != nil {
		return nil, err
	}
	fs := []finding{}
	matched := 0
	for _, t := range latest(ts) {
		if t.Namespace != in.Namespace || !matchesWorkload(t, in.Workload) {
			continue
		}
		matched++
		for _, v := range t.Vulns {
			if sevRank[v.Severity] < minRank || (in.FixableOnly && v.Fixed == "") {
				continue
			}
			fs = append(fs, finding{Namespace: t.Namespace, Workload: t.Workload, Container: t.Container, Image: t.Image, vuln: v})
		}
	}
	if matched == 0 {
		for _, r := range notScanned(latest(ts), run) {
			if r.Namespace == in.Namespace && matchesWorkload(target{Workload: r.Workload}, in.Workload) {
				return nil, mcp.Errorf("%s/%s is running but has no vulnerability report (not scanned yet, or its report was too large to store): its vulnerabilities are unknown, not absent", r.Namespace, r.Workload)
			}
		}
		return nil, mcp.Errorf("no scanned workload matches namespace %q workload %q (not scanned yet, not running, or misnamed; vulnerability_summary lists what was scanned)", in.Namespace, in.Workload)
	}
	sortFindings(fs)
	total := len(fs)
	if len(fs) > in.Limit {
		fs = fs[:in.Limit]
	}
	missing := []running{}
	for _, r := range notScanned(latest(ts), run) {
		if r.Namespace == in.Namespace && matchesWorkload(target{Workload: r.Workload}, in.Workload) {
			missing = append(missing, r)
		}
	}
	return map[string]any{"index_built": at.UTC().Format(time.RFC3339), "total": total, "findings": fs, "truncated": total > len(fs),
		"containers_not_scanned": missing, "scope": scope}, nil
}

func (h *handlers) find(ctx context.Context, args json.RawMessage) (any, error) {
	var in struct {
		ID string `json:"id"`
	}
	if err := mcp.Decode(args, &in); err != nil {
		return nil, err
	}
	id := strings.ToUpper(strings.TrimSpace(in.ID))
	if strings.HasPrefix(id, "GHSA-") {
		id = "GHSA-" + strings.ToLower(id[5:])
	}
	if !reID.MatchString(id) {
		return nil, mcp.Errorf("invalid vulnerability ID %q", in.ID)
	}
	ts, at, err := h.ix.get(ctx)
	if err != nil {
		return nil, err
	}
	fs := []finding{}
	for _, t := range latest(ts) {
		for _, v := range t.Vulns {
			if v.ID == id {
				fs = append(fs, finding{Namespace: t.Namespace, Workload: t.Workload, Container: t.Container, Image: t.Image, vuln: v})
			}
		}
	}
	sortFindings(fs)
	return map[string]any{"index_built": at.UTC().Format(time.RFC3339), "id": id, "affected": fs, "count": len(fs), "scope": scope}, nil
}
