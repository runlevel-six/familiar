// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/runlevel-six/familiar/tools/internal/kube"
	"github.com/runlevel-six/familiar/tools/internal/mcp"
)

var now = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

type fakeAPI struct {
	objects map[string]string // path prefix (without query) -> JSON
	paths   []string
}

func (f *fakeAPI) Get(_ context.Context, path string, out any) error {
	f.paths = append(f.paths, path)
	p, _, _ := strings.Cut(path, "?")
	body, ok := f.objects[p]
	if !ok {
		return kube.ErrNotFound
	}
	return json.Unmarshal([]byte(body), out)
}

const podsJSON = `{"items":[
 {"metadata":{"name":"web-7d9-abc","namespace":"shop","creationTimestamp":"2026-11-22T10:00:00Z","ownerReferences":[{"kind":"ReplicaSet","name":"web-7d9"}]},
  "spec":{"nodeName":"node-0","containers":[{"name":"web","image":"shop/web:1.2","env":[{"name":"DB_PASSWORD","value":"hunter2"}]}]},
  "status":{"phase":"Running","containerStatuses":[{"name":"web","ready":false,"restartCount":7,
    "state":{"waiting":{"reason":"CrashLoopBackOff"}},
    "lastState":{"terminated":{"reason":"Error","exitCode":1,"finishedAt":"2026-11-22T11:58:00Z"}}}]}},
 {"metadata":{"name":"db-0","namespace":"shop","creationTimestamp":"2026-11-01T00:00:00Z"},
  "spec":{"containers":[{"name":"pg","image":"postgres:16"}]},
  "status":{"phase":"Running","containerStatuses":[{"name":"pg","ready":true,"restartCount":0,"state":{}}]}},
 {"metadata":{"name":"job-x","namespace":"ops","creationTimestamp":"2026-11-22T00:00:00Z"},
  "spec":{"containers":[{"name":"j","image":"busybox"}]},
  "status":{"phase":"Succeeded","containerStatuses":[{"name":"j","ready":false,"restartCount":0,"state":{"terminated":{"reason":"Completed","exitCode":0}}}]}}
]}`

const eventsJSON = `{"items":[
 {"type":"Warning","reason":"BackOff","message":"Back-off restarting failed container; probe GET http://172.16.0.9:8080 token=abc123",
  "count":12,"lastTimestamp":"2026-11-22T11:59:00Z","involvedObject":{"kind":"Pod","namespace":"shop","name":"web-7d9-abc"}},
 {"type":"Normal","reason":"Pulled","message":"pulled","lastTimestamp":"2026-11-22T11:50:00Z","involvedObject":{"kind":"Pod","namespace":"shop","name":"db-0"}},
 {"type":"Warning","reason":"Old","message":"old","lastTimestamp":"2026-11-20T00:00:00Z","involvedObject":{"kind":"Pod","namespace":"shop","name":"db-0"}}
]}`

func fixture() *fakeAPI {
	return &fakeAPI{objects: map[string]string{
		"/api/v1/nodes": `{"items":[{"metadata":{"name":"node-0"},"status":{"conditions":[{"type":"Ready","status":"True"},{"type":"MemoryPressure","status":"False"}],
			"allocatable":{"cpu":"12","memory":"31Gi","pods":"500"},"nodeInfo":{"kubeletVersion":"v1.37.2"}}}]}`,
		"/api/v1/pods":                   podsJSON,
		"/api/v1/namespaces/shop/pods":   podsJSON,
		"/api/v1/events":                 eventsJSON,
		"/api/v1/namespaces/shop/events": eventsJSON,
		"/apis/apps/v1/deployments":      `{"items":[{"metadata":{"name":"web","namespace":"shop"},"spec":{"replicas":1},"status":{"readyReplicas":0}}]}`,
		"/apis/apps/v1/statefulsets":     `{"items":[{"metadata":{"name":"db","namespace":"shop"},"spec":{"replicas":1},"status":{"readyReplicas":1}}]}`,
		"/apis/apps/v1/daemonsets":       `{"items":[]}`,
		"/apis/apps/v1/namespaces/shop/deployments/web": `{"metadata":{"name":"web","namespace":"shop","creationTimestamp":"2026-11-01T00:00:00Z"},
			"spec":{"replicas":1,"template":{"spec":{"containers":[{"name":"web","image":"shop/web:1.2","env":[{"name":"SECRET","value":"x"}],"args":["--password=hunter2"],"resources":{"requests":{"cpu":"100m","memory":"128Mi"},"limits":{"cpu":"150m","memory":"192Mi"}}}]}}},
			"status":{"readyReplicas":0,"conditions":[{"type":"Available","status":"False","reason":"MinimumReplicasUnavailable"}]}}`,
		"/apis/batch/v1/namespaces/shop/cronjobs/nightly": `{"metadata":{"name":"nightly","namespace":"shop","creationTimestamp":"2026-11-01T00:00:00Z"},
			"spec":{"schedule":"40 3 * * *","jobTemplate":{"spec":{"template":{"spec":{"containers":[{"name":"dump","image":"db/dump:1","resources":{"limits":{"memory":"256Mi"}}}]}}}}}}`,
		"/api/v1/persistentvolumeclaims":         `{"items":[{"metadata":{"name":"data","namespace":"shop"},"spec":{"storageClassName":"ceph-block","volumeName":"pv-1"},"status":{"phase":"Bound","capacity":{"storage":"5Gi"}}}]}`,
		"/api/v1/persistentvolumes":              `{"items":[{"metadata":{"name":"pv-1"},"status":{"phase":"Bound"}},{"metadata":{"name":"pv-old"},"spec":{"persistentVolumeReclaimPolicy":"Retain","claimRef":{"namespace":"gone","name":"x"}},"status":{"phase":"Released"}}]}`,
		"/apis/storage.k8s.io/v1/storageclasses": `{"items":[{"metadata":{"name":"ceph-block"},"provisioner":"rook-ceph.rbd.csi.ceph.com"}]}`,
		"/apis/networking.k8s.io/v1/ingresses": `{"items":[{"metadata":{"name":"web","namespace":"shop","annotations":{"secret":"x"}},"spec":{"ingressClassName":"nginx","tls":[{"hosts":["*.example.com"]}],"rules":[{"host":"shop.example.com","http":{"paths":[{"path":"/","backend":{"service":{"name":"web","port":{"number":80}}}}]}}]}},
			{"metadata":{"name":"admin","namespace":"shop","annotations":{"nginx.ingress.kubernetes.io/auth-url":"http://auth.internal.example/verify?token=abc123","nginx.ingress.kubernetes.io/auth-response-headers":"Remote-User"}},"spec":{"rules":[{"host":"admin.example.com"}]}},
			{"metadata":{"name":"api","namespace":"shop","annotations":{"traefik.ingress.kubernetes.io/router.middlewares":"shop-sso@kubernetescrd, shop-ratelimit@kubernetescrd","traefik.ingress.kubernetes.io/router.entrypoints":"websecure"}},"spec":{"rules":[{"host":"api.example.com"}]}},
			{"metadata":{"name":"lan","namespace":"shop","annotations":{"nginx.ingress.kubernetes.io/whitelist-source-range":"10.0.0.0/8"}},"spec":{"rules":[{"host":"lan.example.com"}]}}]}`,
	}}
}

// runTool calls a tool through a real MCP server so redaction is included.
func runTool(t *testing.T, api Getter, tool, args string) (map[string]any, bool) {
	t.Helper()
	h := newHandlers(api, func() time.Time { return now })
	s := &mcp.Server{Name: "cluster-read", Version: "test", Tools: h.tools()}
	s.Redact.IPs = true
	return callTool(t, s, tool, args)
}

func TestOverview(t *testing.T) {
	out, isErr := runTool(t, fixture(), "cluster_overview", `{}`)
	if isErr {
		t.Fatal(out)
	}
	if out["pods_total"].(float64) != 3 || out["warning_events_last_hour"].(float64) != 1 {
		t.Fatalf("counts: %v", out)
	}
	un := out["unhealthy_pods"].([]any)
	if len(un) != 1 || un[0].(map[string]any)["problem"] != "web: CrashLoopBackOff" {
		t.Fatalf("unhealthy: %v", un)
	}
	na := out["workloads_not_available"].([]any)
	if len(na) != 1 || na[0].(map[string]any)["ready"] != "0/1" {
		t.Fatalf("not available: %v", na)
	}
}

func TestListPodsNeverLeaksEnv(t *testing.T) {
	out, _ := runTool(t, fixture(), "list_pods", `{"namespace":"shop"}`)
	b, _ := json.Marshal(out)
	if strings.Contains(string(b), "hunter2") || strings.Contains(string(b), "DB_PASSWORD") {
		t.Fatalf("env leaked: %s", b)
	}
	if out["total"].(float64) != 3 {
		t.Fatalf("total: %v", out)
	}
	out, _ = runTool(t, fixture(), "list_pods", `{"only_unhealthy":true}`)
	if out["total"].(float64) != 1 {
		t.Fatalf("unhealthy filter: %v", out)
	}
	p := out["pods"].([]any)[0].(map[string]any)
	if p["restarts"].(float64) != 7 || !strings.Contains(p["last_restart"].(string), "Error (exit 1)") {
		t.Fatalf("pod summary: %v", p)
	}
}

func TestDescribeWorkload(t *testing.T) {
	out, isErr := runTool(t, fixture(), "describe_workload", `{"namespace":"shop","kind":"deployment","name":"web"}`)
	if isErr {
		t.Fatal(out)
	}
	b, _ := json.Marshal(out)
	if strings.Contains(string(b), "hunter2") || strings.Contains(string(b), "SECRET") {
		t.Fatalf("env/args leaked: %s", b)
	}
	if len(out["pods"].([]any)) != 1 || len(out["warning_events_24h"].([]any)) != 1 {
		t.Fatalf("related objects: %v", out)
	}
	c := out["containers"].([]any)[0].(map[string]any)
	if c["image"] != "shop/web:1.2" || c["limits"].(map[string]any)["cpu"] != "150m" || c["requests"].(map[string]any)["memory"] != "128Mi" {
		t.Fatalf("container resources: %v", c)
	}
	cj, isErr := runTool(t, fixture(), "describe_workload", `{"namespace":"shop","kind":"cronjob","name":"nightly"}`)
	if isErr {
		t.Fatal(cj)
	}
	if c := cj["containers"].([]any)[0].(map[string]any); c["name"] != "dump" || c["limits"].(map[string]any)["memory"] != "256Mi" {
		t.Fatalf("cronjob containers: %v", cj["containers"])
	}
	if _, isErr := runTool(t, fixture(), "describe_workload", `{"namespace":"shop","kind":"deployment","name":"nope"}`); !isErr {
		t.Fatal("missing workload not reported as tool error")
	}
}

func TestEventsAreRedactedAndFiltered(t *testing.T) {
	out, _ := runTool(t, fixture(), "recent_events", `{"namespace":"shop"}`)
	evs := out["events"].([]any)
	if len(evs) != 1 {
		t.Fatalf("events: %v", evs)
	}
	msg := evs[0].(map[string]any)["message"].(string)
	if strings.Contains(msg, "172.16") || strings.Contains(msg, "abc123") {
		t.Fatalf("message not redacted: %q", msg)
	}
	out, _ = runTool(t, fixture(), "recent_events", `{"namespace":"shop","warnings_only":false,"since_minutes":1440}`)
	if len(out["events"].([]any)) != 2 {
		t.Fatalf("all events in 24h: %v", out)
	}
}

func TestInputValidation(t *testing.T) {
	api := fixture()
	for _, args := range []string{
		`{"namespace":"../secrets"}`,
		`{"namespace":"kube-system/secrets"}`,
		`{"namespace":"UPPER"}`,
		`{"namespace":"shop","extra":true}`,
	} {
		if _, isErr := runTool(t, api, "list_pods", args); !isErr {
			t.Errorf("accepted %s", args)
		}
	}
	if _, isErr := runTool(t, api, "describe_workload", `{"namespace":"shop","kind":"secret","name":"x"}`); !isErr {
		t.Error("kind secret accepted")
	}
	if _, isErr := runTool(t, api, "describe_workload", `{"namespace":"shop","kind":"deployment","name":"a/../../secrets"}`); !isErr {
		t.Error("path traversal in name accepted")
	}
	for _, p := range api.paths {
		if strings.Contains(p, "secret") || strings.Contains(p, "configmap") || strings.Contains(p, "/log") {
			t.Errorf("forbidden API path requested: %s", p)
		}
	}
}

func TestStorageCertsIngresses(t *testing.T) {
	out, _ := runTool(t, fixture(), "storage_status", `{}`)
	if len(out["claims"].([]any)) != 1 || len(out["volumes_not_bound"].([]any)) != 1 {
		t.Fatalf("storage: %v", out)
	}
	out, _ = runTool(t, fixture(), "certificates", `{}`)
	if out["note"] != "cert-manager CRDs not installed" {
		t.Fatalf("missing CRD handling: %v", out)
	}
	out, _ = runTool(t, fixture(), "ingresses", `{}`)
	ing := out["ingresses"].([]any)[0].(map[string]any)
	b, _ := json.Marshal(out)
	if ing["backends"].([]any)[0] != "shop.example.com/ -> web:80" || strings.Contains(string(b), `"secret"`) {
		t.Fatalf("ingresses: %s", b)
	}
}

// The ingresses tool says how each route is guarded from a fixed set of
// annotations, and returns no annotation value other than middleware and
// entrypoint names.
func TestIngressAccess(t *testing.T) {
	out, _ := runTool(t, fixture(), "ingresses", `{}`)
	got := map[string]map[string]any{}
	for _, i := range out["ingresses"].([]any) {
		m := i.(map[string]any)
		got[m["name"].(string)] = m
	}
	want := map[string]struct {
		auth      string
		allowlist bool
		mw, ep    string
	}{
		"web":   {"none", false, "[]", "[]"},
		"admin": {"forward_auth", false, "[]", "[]"},
		"api":   {"unknown", false, `["shop-sso@kubernetescrd","shop-ratelimit@kubernetescrd"]`, `["websecure"]`},
		"lan":   {"none", true, "[]", "[]"},
	}
	for name, w := range want {
		g := got[name]
		mw, _ := json.Marshal(g["middlewares"])
		ep, _ := json.Marshal(g["entrypoints"])
		if g["auth"] != w.auth || g["source_allowlist"] != w.allowlist || string(mw) != w.mw || string(ep) != w.ep {
			t.Errorf("%s: auth=%v allowlist=%v middlewares=%s entrypoints=%s", name, g["auth"], g["source_allowlist"], mw, ep)
		}
	}
	b, _ := json.Marshal(out)
	for _, leak := range []string{"auth.internal", "abc123", "Remote-User", "10.0.0.0", `"secret"`} {
		if strings.Contains(string(b), leak) {
			t.Errorf("annotation value %q returned: %s", leak, b)
		}
	}
}
