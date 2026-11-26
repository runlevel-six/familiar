// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"strings"
	"sync"
	"time"
)

// Getter is the subset of kube.Client the server uses.
type Getter interface {
	Get(ctx context.Context, path string, out any) error
}

// hiddenNamespacePrefix: familiar's own namespaces stay invisible (design §3
// T3), including the processes of their pods on the host, such as the
// agent's Kata VM and the guardrails.
const hiddenNamespacePrefix = "familiar-"

type podRef struct{ Namespace, Name string }

// podIndex maps pod UIDs, as they appear in cgroup paths, to pods. Static
// pods appear under their config hash, which their mirror pod carries as an
// annotation.
type podIndex struct {
	g   Getter
	ttl time.Duration
	now func() time.Time

	mu      sync.Mutex
	byUID   map[string]podRef
	fetched time.Time
}

func (x *podIndex) lookup(ctx context.Context) (map[string]podRef, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.byUID != nil && x.now().Sub(x.fetched) < x.ttl {
		return x.byUID, nil
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name        string            `json:"name"`
				Namespace   string            `json:"namespace"`
				UID         string            `json:"uid"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := x.g.Get(ctx, "/api/v1/pods?limit=1000", &list); err != nil {
		return nil, err
	}
	m := make(map[string]podRef, len(list.Items))
	for _, it := range list.Items {
		ref := podRef{it.Metadata.Namespace, it.Metadata.Name}
		m[it.Metadata.UID] = ref
		for _, k := range []string{"kubernetes.io/config.mirror", "kubernetes.io/config.hash"} {
			if h := it.Metadata.Annotations[k]; h != "" {
				m[h] = ref
			}
		}
	}
	x.byUID, x.fetched = m, x.now()
	return m, nil
}

// classify names a process's group and says whether it must be hidden. With
// no pod index (the API was unreachable), every pod process is hidden: a
// process that might be familiar's is never shown (fail closed).
func classify(p proc, pods map[string]podRef) (group string, hidden bool) {
	if p.PID == 2 || p.PPID == 2 {
		return "kernel", false
	}
	uid := podUID(p.Cgroup)
	if uid == "" {
		if strings.Contains(p.Cgroup, "kubepods") {
			return "", true // a pod-level cgroup without a UID we can read
		}
		return systemdGroup(p.Cgroup), false
	}
	if pods == nil {
		return "", true
	}
	ref, ok := pods[uid]
	if !ok {
		return "", true // pod just created or deleted; can't rule out familiar
	}
	if strings.HasPrefix(ref.Namespace, hiddenNamespacePrefix) {
		return "", true
	}
	return "pod " + ref.Namespace + "/" + ref.Name, false
}
