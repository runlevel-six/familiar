// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Getter is the subset of kube.Client used here.
type Getter interface {
	Get(ctx context.Context, path string, out any) error
}

// familiar's own namespaces are invisible to the agent (design §3 T3), as
// in cluster-read: refused as a parameter, and their reports dropped before
// anything is kept.
const hiddenNamespacePrefix = "familiar-"

func hiddenNamespace(ns string) bool { return strings.HasPrefix(ns, hiddenNamespacePrefix) }

const (
	reportsPath = "/apis/aquasecurity.github.io/v1alpha1/vulnerabilityreports"
	podsPath    = "/api/v1/pods"
)

// pageSize keeps each API response small: one report can approach 1 MB.
const pageSize = 5

// Report fields as the Trivy Operator writes them. Only what the tools
// return is decoded; title, description and links never are.
type rawReport struct {
	Metadata struct {
		Namespace string            `json:"namespace"`
		Labels    map[string]string `json:"labels"`
	} `json:"metadata"`
	Report struct {
		UpdateTimestamp time.Time `json:"updateTimestamp"`
		Artifact        struct {
			Repository string `json:"repository"`
			Tag        string `json:"tag"`
			Digest     string `json:"digest"`
		} `json:"artifact"`
		OS struct {
			Family string `json:"family"`
			Name   string `json:"name"`
		} `json:"os"`
		Vulnerabilities []struct {
			ID        string   `json:"vulnerabilityID"`
			Severity  string   `json:"severity"`
			Score     *float64 `json:"score"`
			Resource  string   `json:"resource"`
			Installed string   `json:"installedVersion"`
			Fixed     string   `json:"fixedVersion"`
			Published string   `json:"publishedDate"`
		} `json:"vulnerabilities"`
	} `json:"report"`
}

type list struct {
	Metadata struct {
		Continue string `json:"continue"`
	} `json:"metadata"`
	Items []rawReport `json:"items"`
}

// vuln is one finding, validated. severity is the effective severity
// (effectiveSeverity); vendorSeverity is what the scanner reported.
type vuln struct {
	ID             string   `json:"id"`
	Severity       string   `json:"severity"`
	VendorSeverity string   `json:"vendor_severity"`
	CVSS           *float64 `json:"cvss,omitempty"`
	Package        string   `json:"package"`
	Installed      string   `json:"installed"`
	Fixed          string   `json:"fixed,omitempty"`
	Published      string   `json:"published,omitempty"`
}

// target is one scanned container of one workload.
type target struct {
	Namespace string
	Workload  string // e.g. deployment/web, statefulset/db, cronjob/backup
	Container string
	Image     string
	OS        string
	ScannedAt time.Time
	Vulns     []vuln
}

// running is one container of a running pod, for coverage: a container
// without a report was not scanned (yet), or its report was too large to
// store, and must never read as clean.
type running struct {
	Namespace string `json:"namespace"`
	Workload  string `json:"workload"`
	Container string `json:"container"`
	Image     string `json:"image"`
}

// Only what coverage needs is decoded from pods: never env, args, volumes or
// annotations.
type podList struct {
	Metadata struct {
		Continue string `json:"continue"`
	} `json:"metadata"`
	Items []struct {
		Metadata struct {
			Name            string `json:"name"`
			Namespace       string `json:"namespace"`
			OwnerReferences []struct {
				Kind string `json:"kind"`
				Name string `json:"name"`
			} `json:"ownerReferences"`
		} `json:"metadata"`
		Spec struct {
			Containers []struct {
				Name  string `json:"name"`
				Image string `json:"image"`
			} `json:"containers"`
		} `json:"spec"`
		Status struct {
			Phase string `json:"phase"`
		} `json:"status"`
	} `json:"items"`
}

// index caches every visible report, compacted, and the running containers,
// for ttl.
type index struct {
	g       Getter
	now     func() time.Time
	ttl     time.Duration
	mu      sync.Mutex
	at      time.Time
	targets []target
	running []running
}

func newIndex(g Getter, now func() time.Time, ttl time.Duration) *index {
	return &index{g: g, now: now, ttl: ttl}
}

// get returns the cached targets, refreshing them when older than ttl.
func (ix *index) get(ctx context.Context) ([]target, time.Time, error) {
	ts, _, at, err := ix.getAll(ctx)
	return ts, at, err
}

// getAll returns the cached targets and running containers.
func (ix *index) getAll(ctx context.Context) ([]target, []running, time.Time, error) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if ix.targets != nil && ix.now().Sub(ix.at) < ix.ttl {
		return ix.targets, ix.running, ix.at, nil
	}
	var all []target
	cont := ""
	for {
		path := reportsPath + "?limit=" + strconv.Itoa(pageSize)
		if cont != "" {
			path += "&continue=" + url.QueryEscape(cont)
		}
		var l list
		if err := ix.g.Get(ctx, path, &l); err != nil {
			return nil, nil, time.Time{}, err
		}
		for _, r := range l.Items {
			if hiddenNamespace(r.Metadata.Namespace) || hiddenNamespace(r.Metadata.Labels["trivy-operator.resource.namespace"]) {
				continue
			}
			all = append(all, compact(r))
		}
		if cont = l.Metadata.Continue; cont == "" {
			break
		}
	}
	if all == nil {
		all = []target{}
	}
	run, err := ix.runningContainers(ctx)
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	ix.targets, ix.running, ix.at = all, run, ix.now()
	return all, run, ix.at, nil
}

// runningContainers lists the main containers of running pods, keyed the
// way reports are: pods owned by Jobs are skipped (short-lived; their
// CronJob or Job is what gets scanned), and static pods are owned by their
// Node, so they're named pod/<name> as the scanner names them.
func (ix *index) runningContainers(ctx context.Context) ([]running, error) {
	var out []running
	cont := ""
	for {
		path := podsPath + "?limit=200"
		if cont != "" {
			path += "&continue=" + url.QueryEscape(cont)
		}
		var l podList
		if err := ix.g.Get(ctx, path, &l); err != nil {
			return nil, err
		}
		for _, p := range l.Items {
			if p.Status.Phase != "Running" || hiddenNamespace(p.Metadata.Namespace) {
				continue
			}
			kind, name := "Pod", p.Metadata.Name
			if len(p.Metadata.OwnerReferences) > 0 {
				o := p.Metadata.OwnerReferences[0]
				switch o.Kind {
				case "Job":
					continue
				case "Node":
				default:
					kind, name = o.Kind, o.Name
				}
			}
			w := workloadName(kind, name)
			for _, c := range p.Spec.Containers {
				out = append(out, running{Namespace: p.Metadata.Namespace, Workload: w,
					Container: clean(c.Name, reToken, 63), Image: clean(c.Image, rePackage, 200)})
			}
		}
		if cont = l.Metadata.Continue; cont == "" {
			break
		}
	}
	if out == nil {
		out = []running{}
	}
	return out, nil
}

// notScanned returns the running containers no report covers, deduplicated
// (a Deployment's replicas are one entry).
func notScanned(ts []target, run []running) []running {
	have := map[string]bool{}
	for _, t := range ts {
		have[t.Namespace+"/"+t.Workload+"/"+t.Container] = true
	}
	seen := map[string]bool{}
	out := []running{}
	for _, r := range run {
		k := r.Namespace + "/" + r.Workload + "/" + r.Container
		if have[k] || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Namespace+out[i].Workload+out[i].Container < out[j].Namespace+out[j].Workload+out[j].Container
	})
	return out
}

func compact(r rawReport) target {
	l := r.Metadata.Labels
	t := target{
		Namespace: r.Metadata.Namespace,
		Workload:  workloadName(l["trivy-operator.resource.kind"], l["trivy-operator.resource.name"]),
		Container: clean(l["trivy-operator.container.name"], reToken, 63),
		Image:     imageRef(r.Report.Artifact.Repository, r.Report.Artifact.Tag, r.Report.Artifact.Digest),
		ScannedAt: r.Report.UpdateTimestamp,
	}
	if r.Report.OS.Family != "" {
		t.OS = strings.TrimSpace(clean(r.Report.OS.Family, reToken, 20) + " " + clean(r.Report.OS.Name, reVersion, 20))
	}
	// A package can be listed more than once for one vulnerability (several
	// files in the image); keep the first, so every tool counts it once.
	seen := map[string]bool{}
	for _, v := range r.Report.Vulnerabilities {
		if k := v.ID + "\x00" + v.Resource + "\x00" + v.Installed; seen[k] {
			continue
		} else {
			seen[k] = true
		}
		vs := strings.ToUpper(v.Severity)
		if !validSeverity[vs] {
			vs = "UNKNOWN"
		}
		var score *float64
		if v.Score != nil && *v.Score >= 0 && *v.Score <= 10 {
			s := *v.Score
			score = &s
		}
		t.Vulns = append(t.Vulns, vuln{
			ID:             cleanID(v.ID),
			Severity:       effectiveSeverity(vs, score),
			VendorSeverity: vs,
			CVSS:           score,
			Package:        clean(v.Resource, rePackage, 128),
			Installed:      clean(v.Installed, reVersion, 80),
			Fixed:          clean(v.Fixed, reVersionList, 120),
			Published:      cleanDate(v.Published),
		})
	}
	return t
}

var validSeverity = map[string]bool{"CRITICAL": true, "HIGH": true, "MEDIUM": true, "LOW": true, "UNKNOWN": true}

var sevRank = map[string]int{"CRITICAL": 4, "HIGH": 3, "MEDIUM": 2, "LOW": 1, "UNKNOWN": 0}

// effectiveSeverity is the scanner's severity, or, when that is UNKNOWN
// (the distribution hasn't rated it yet), one derived from the CVSS score
// with the CVSS v3 bands. Unrated findings are common and can be serious:
// an unrated CVSS 8.1 remote code execution in apt was in the first scan.
func effectiveSeverity(vendor string, score *float64) string {
	if vendor != "UNKNOWN" || score == nil {
		return vendor
	}
	switch s := *score; {
	case s >= 9:
		return "CRITICAL"
	case s >= 7:
		return "HIGH"
	case s >= 4:
		return "MEDIUM"
	case s > 0:
		return "LOW"
	}
	return "UNKNOWN"
}

// workloadName turns the report's owner into the object a person manages:
// a ReplicaSet named <deployment>-<pod-template-hash> becomes deployment/<name>.
func workloadName(kind, name string) string {
	kind, name = strings.ToLower(clean(kind, reToken, 40)), clean(name, reToken, 253)
	if kind == "replicaset" {
		if i := strings.LastIndexByte(name, '-'); i > 0 && reTemplateHash.MatchString(name[i+1:]) {
			return "deployment/" + name[:i]
		}
	}
	return kind + "/" + name
}

func imageRef(repo, tag, digest string) string {
	ref := clean(repo, rePackage, 200)
	if tag != "" && !strings.Contains(tag, "/") {
		ref += ":" + clean(tag, reVersion, 128)
	} else if digest != "" {
		ref += "@" + clean(digest, reVersion, 80)
	}
	return ref
}

// Everything returned comes from scanned images, which their authors
// control, so each field must match a narrow pattern; anything else is
// replaced character by character with "_". No spaces survive, so no
// sentence does either.
var (
	reID           = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,15}(-[0-9A-Za-z]{1,16}){1,5}$`)
	reToken        = regexp.MustCompile(`[^a-z0-9A-Z._-]`)
	rePackage      = regexp.MustCompile(`[^A-Za-z0-9._+/@:~-]`)
	reVersion      = regexp.MustCompile(`[^A-Za-z0-9._+:~-]`)
	reVersionList  = regexp.MustCompile(`[^A-Za-z0-9._+:~, -]`)
	reTemplateHash = regexp.MustCompile(`^[bcdfghjklmnpqrstvwxz2456789]{5,10}$`)
)

func clean(s string, bad *regexp.Regexp, max int) string {
	if len(s) > max {
		s = s[:max] + "…"
	}
	return bad.ReplaceAllString(s, "_")
}

func cleanID(s string) string {
	if reID.MatchString(s) {
		return s
	}
	return "INVALID-ID"
}

func cleanDate(s string) string {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC().Format("2006-01-02")
	}
	return ""
}
