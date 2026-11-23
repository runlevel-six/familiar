// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/runlevel-six/familiar/tools/internal/mcp"
)

// hiddenNamespacePrefix marks familiar's own namespaces (familiar-system,
// familiar-agents, familiar-tools). The agent doesn't get to inspect its own
// guardrails: model-gate, the tool servers, memory-auditor, or its own pod
// (design §3 T3). RBAC can't subtract namespaces from a ClusterRole, so the
// tool server enforces it (decided 2026-11-23; may be relaxed later).
const hiddenNamespacePrefix = "familiar-"

func hiddenNamespace(ns string) bool { return strings.HasPrefix(ns, hiddenNamespacePrefix) }

// scopedGetter wraps the API client so no handler can see familiar's
// namespaces: a request for a path inside one fails, and every list response
// has their objects removed before it is decoded. Doing this below the
// handlers means a new tool is covered without remembering to filter.
type scopedGetter struct{ g Getter }

func (s scopedGetter) Get(ctx context.Context, path string, out any) error {
	p, _, _ := strings.Cut(path, "?")
	if ns := namespaceInPath(p); hiddenNamespace(ns) {
		return mcp.Errorf("namespace %q is not visible to this tool", ns)
	}
	var raw json.RawMessage
	if err := s.g.Get(ctx, path, &raw); err != nil {
		return err
	}
	filtered, err := dropHidden(raw, p == "/api/v1/namespaces")
	if err != nil {
		return err
	}
	return json.Unmarshal(filtered, out)
}

// namespaceInPath returns the namespace of a namespaced API path
// (".../namespaces/<ns>/..."), or of a single Namespace object
// ("/api/v1/namespaces/<ns>"), or "".
func namespaceInPath(p string) string {
	segs := strings.Split(strings.Trim(p, "/"), "/")
	for i := 0; i+1 < len(segs); i++ {
		if segs[i] == "namespaces" {
			return segs[i+1]
		}
	}
	return ""
}

// dropHidden removes list items in a hidden namespace: by metadata.namespace,
// by spec.claimRef.namespace (PersistentVolumes, which are cluster-scoped),
// by involvedObject.namespace (events), and, for a list of Namespaces, by
// name. Anything that isn't a list passes through unchanged.
func dropHidden(raw json.RawMessage, namespaceList bool) (json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil || obj["items"] == nil {
		return raw, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(obj["items"], &items); err != nil {
		return nil, err
	}
	kept := items[:0]
	for _, it := range items {
		var ref struct {
			Metadata struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
			Spec struct {
				ClaimRef struct {
					Namespace string `json:"namespace"`
				} `json:"claimRef"`
			} `json:"spec"`
			InvolvedObject struct {
				Namespace string `json:"namespace"`
			} `json:"involvedObject"`
		}
		_ = json.Unmarshal(it, &ref)
		if hiddenNamespace(ref.Metadata.Namespace) || hiddenNamespace(ref.Spec.ClaimRef.Namespace) ||
			hiddenNamespace(ref.InvolvedObject.Namespace) || (namespaceList && hiddenNamespace(ref.Metadata.Name)) {
			continue
		}
		kept = append(kept, it)
	}
	b, err := json.Marshal(kept)
	if err != nil {
		return nil, err
	}
	obj["items"] = b
	return json.Marshal(obj)
}
