// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// hiddenItem looks like an object in a familiar namespace to every filter
// rule: metadata.namespace (most kinds), involvedObject (events) and
// claimRef (PersistentVolumes).
const hiddenItem = `{"metadata":{"name":"hermes-7c9-xyz","namespace":"familiar-agents","creationTimestamp":"2026-11-22T10:00:00Z"},
 "type":"Warning","reason":"BackOff","message":"familiar-agents hermes restarting","count":3,"lastTimestamp":"2026-11-22T11:59:00Z",
 "involvedObject":{"kind":"Pod","namespace":"familiar-agents","name":"hermes-7c9-xyz"},
 "spec":{"claimRef":{"namespace":"familiar-agents","name":"hermes-data"},"storageClassName":"ceph-block","replicas":1,
   "containers":[{"name":"hermes","image":"nousresearch/hermes-agent"}]},
 "status":{"phase":"Running","readyReplicas":0}}`

// fixtureWithFamiliar is fixture() with a familiar-agents object added to
// every list, and namespaced paths for familiar's own namespaces.
func fixtureWithFamiliar(t *testing.T) *fakeAPI {
	t.Helper()
	f := fixture()
	for path, body := range f.objects {
		var obj map[string]json.RawMessage
		if json.Unmarshal([]byte(body), &obj) != nil || obj["items"] == nil {
			continue
		}
		var items []json.RawMessage
		if err := json.Unmarshal(obj["items"], &items); err != nil {
			t.Fatal(path, err)
		}
		items = append(items, json.RawMessage(hiddenItem))
		obj["items"], _ = json.Marshal(items)
		b, _ := json.Marshal(obj)
		f.objects[path] = string(b)
	}
	f.objects["/api/v1/namespaces/familiar-agents/pods"] = `{"items":[` + hiddenItem + `]}`
	f.objects["/apis/apps/v1/namespaces/familiar-system/deployments/model-gate"] = `{"metadata":{"name":"model-gate","namespace":"familiar-system"}}`
	return f
}

func TestFamiliarNamespacesHiddenFromEveryTool(t *testing.T) {
	api := fixtureWithFamiliar(t)
	for _, tool := range []string{"cluster_overview", "list_pods", "recent_events", "storage_status", "certificates", "ingresses"} {
		out, isErr := runTool(t, api, tool, `{}`)
		b, _ := json.Marshal(out)
		if strings.Contains(string(b), "familiar-") || strings.Contains(string(b), "hermes-7c9") {
			t.Errorf("%s leaked a familiar object (isErr=%v): %s", tool, isErr, b)
		}
	}
	// The counts exclude it too: the base fixture has 3 pods.
	if out, _ := runTool(t, api, "cluster_overview", `{}`); out["pods_total"].(float64) != 3 {
		t.Errorf("pods_total %v, want 3", out["pods_total"])
	}
}

func TestFamiliarNamespacesRefusedByName(t *testing.T) {
	api := fixtureWithFamiliar(t)
	for tool, args := range map[string]string{
		"list_pods":         `{"namespace":"familiar-agents"}`,
		"recent_events":     `{"namespace":"familiar-system"}`,
		"storage_status":    `{"namespace":"familiar-agents"}`,
		"certificates":      `{"namespace":"familiar-tools"}`,
		"ingresses":         `{"namespace":"familiar-system"}`,
		"describe_workload": `{"namespace":"familiar-system","kind":"deployment","name":"model-gate"}`,
	} {
		out, isErr := runTool(t, api, tool, args)
		if !isErr || !strings.Contains(out["error"].(string), "not visible") {
			t.Errorf("%s %s: isErr=%v out=%v", tool, args, isErr, out)
		}
	}
	for _, p := range api.paths {
		if strings.Contains(p, "/namespaces/familiar-") {
			t.Errorf("request for a familiar namespace reached the API: %s", p)
		}
	}
}

// The wrapper holds even for a path a handler builds without checkNamespace.
func TestScopedGetterBelowHandlers(t *testing.T) {
	api := fixtureWithFamiliar(t)
	api.objects["/api/v1/namespaces"] = `{"items":[{"metadata":{"name":"shop"}},{"metadata":{"name":"familiar-agents"}},{"metadata":{"name":"familiarity"}}]}`
	api.objects["/api/v1/namespaces/familiar-agents"] = `{"metadata":{"name":"familiar-agents"}}`
	s := scopedGetter{api}
	var out map[string]any
	for _, p := range []string{"/api/v1/namespaces/familiar-agents/pods?limit=10", "/api/v1/namespaces/familiar-agents"} {
		if err := s.Get(context.Background(), p, &out); err == nil {
			t.Errorf("%s: no error", p)
		}
	}
	var nsl list[struct {
		Metadata meta `json:"metadata"`
	}]
	if err := s.Get(context.Background(), "/api/v1/namespaces", &nsl); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, n := range nsl.Items {
		names = append(names, n.Metadata.Name)
	}
	if strings.Join(names, ",") != "shop,familiarity" {
		t.Errorf("namespace list %v", names)
	}
	// non-list objects and lists without hidden items pass through unchanged
	var sc list[struct {
		Metadata meta `json:"metadata"`
	}]
	if err := s.Get(context.Background(), "/apis/storage.k8s.io/v1/storageclasses", &sc); err != nil || len(sc.Items) != 1 {
		t.Errorf("storageclasses: %v %v", sc, err)
	}
}
