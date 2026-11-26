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
	byClaim map[podRef][]string // namespace/claim -> names of pods mounting it
	fetched time.Time
}

func (x *podIndex) lookup(ctx context.Context) (map[string]podRef, error) {
	byUID, _, err := x.load(ctx)
	return byUID, err
}

// claims maps each PersistentVolumeClaim (as namespace/name) to the pods
// that mount it.
func (x *podIndex) claims(ctx context.Context) (map[podRef][]string, error) {
	_, byClaim, err := x.load(ctx)
	return byClaim, err
}

func (x *podIndex) load(ctx context.Context) (map[string]podRef, map[podRef][]string, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.byUID != nil && x.now().Sub(x.fetched) < x.ttl {
		return x.byUID, x.byClaim, nil
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name        string            `json:"name"`
				Namespace   string            `json:"namespace"`
				UID         string            `json:"uid"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
			Spec struct {
				Volumes []struct {
					PVC *struct {
						ClaimName string `json:"claimName"`
					} `json:"persistentVolumeClaim"`
				} `json:"volumes"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := x.g.Get(ctx, "/api/v1/pods?limit=1000", &list); err != nil {
		return nil, nil, err
	}
	m := make(map[string]podRef, len(list.Items))
	c := map[podRef][]string{}
	for _, it := range list.Items {
		ref := podRef{it.Metadata.Namespace, it.Metadata.Name}
		m[it.Metadata.UID] = ref
		for _, k := range []string{"kubernetes.io/config.mirror", "kubernetes.io/config.hash"} {
			if h := it.Metadata.Annotations[k]; h != "" {
				m[h] = ref
			}
		}
		for _, v := range it.Spec.Volumes {
			if v.PVC != nil {
				claim := podRef{it.Metadata.Namespace, v.PVC.ClaimName}
				c[claim] = append(c[claim], it.Metadata.Name)
			}
		}
	}
	x.byUID, x.byClaim, x.fetched = m, c, x.now()
	return m, c, nil
}

// classify names a process's group and says whether it must be hidden. With
// no pod index (the API was unreachable), every pod process is hidden: a
// process that might be familiar's is never shown (fail closed).
func classify(p proc, pods map[string]podRef) (group string, hidden bool) {
	if p.PID == 2 || p.PPID == 2 {
		return "kernel", false
	}
	if p.Cgroup == ownCgroup {
		return "", true // this server, in familiar-host
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
