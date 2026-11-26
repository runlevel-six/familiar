// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/runlevel-six/familiar/tools/internal/mcp"
)

// rbdDev is one mapped Ceph RBD image as sysfs shows it. All of these files
// are world-readable: /sys/bus/rbd/devices/<id>/{pool,name} and
// /sys/block/rbd<id>/stat.
type rbdDev struct {
	ID, Pool, Image string
	Stat            blockStat
}

// blockStat holds the /sys/block/<dev>/stat counters used here.
type blockStat struct {
	Reads, ReadSectors, ReadMS, Writes, WriteSectors, WriteMS, BusyMS uint64
}

func parseBlockStat(b []byte) (blockStat, bool) {
	f := strings.Fields(string(b))
	if len(f) < 10 {
		return blockStat{}, false
	}
	n := func(i int) uint64 { v, _ := strconv.ParseUint(f[i], 10, 64); return v }
	return blockStat{Reads: n(0), ReadSectors: n(2), ReadMS: n(3), Writes: n(4), WriteSectors: n(6), WriteMS: n(7), BusyMS: n(9)}, true
}

func readRBD(sys string) (map[string]rbdDev, error) {
	ents, err := os.ReadDir(filepath.Join(sys, "bus/rbd/devices"))
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]rbdDev{}, nil // no RBD mapped
		}
		return nil, err
	}
	out := map[string]rbdDev{}
	for _, e := range ents {
		id := e.Name()
		dir := filepath.Join(sys, "bus/rbd/devices", id)
		pool, err1 := os.ReadFile(filepath.Join(dir, "pool"))
		name, err2 := os.ReadFile(filepath.Join(dir, "name"))
		st, err3 := os.ReadFile(filepath.Join(sys, "block", "rbd"+id, "stat"))
		if err1 != nil || err2 != nil || err3 != nil {
			continue // unmapped while reading
		}
		bs, ok := parseBlockStat(st)
		if !ok {
			continue
		}
		out[id] = rbdDev{ID: id, Pool: strings.TrimSpace(string(pool)), Image: strings.TrimSpace(string(name)), Stat: bs}
	}
	return out, nil
}

// pvIndex maps Ceph images (pool/image) to the claims bound to them, from
// the CSI attributes of each PersistentVolume.
type pvIndex struct {
	g   Getter
	ttl time.Duration
	now func() time.Time

	mu      sync.Mutex
	byImage map[string]podRef
	fetched time.Time
}

func (x *pvIndex) lookup(ctx context.Context) (map[string]podRef, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.byImage != nil && x.now().Sub(x.fetched) < x.ttl {
		return x.byImage, nil
	}
	var list struct {
		Items []struct {
			Spec struct {
				CSI *struct {
					VolumeAttributes map[string]string `json:"volumeAttributes"`
				} `json:"csi"`
				ClaimRef *struct {
					Namespace string `json:"namespace"`
					Name      string `json:"name"`
				} `json:"claimRef"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := x.g.Get(ctx, "/api/v1/persistentvolumes?limit=1000", &list); err != nil {
		return nil, err
	}
	m := map[string]podRef{}
	for _, it := range list.Items {
		if it.Spec.CSI == nil || it.Spec.ClaimRef == nil {
			continue
		}
		a := it.Spec.CSI.VolumeAttributes
		if a["imageName"] == "" {
			continue
		}
		m[a["pool"]+"/"+a["imageName"]] = podRef{it.Spec.ClaimRef.Namespace, it.Spec.ClaimRef.Name}
	}
	x.byImage, x.fetched = m, x.now()
	return m, nil
}

type volRow struct {
	Volume         string   `json:"volume"` // namespace/claim
	Pods           []string `json:"pods"`
	Device         string   `json:"device"`
	WriteOpsPerS   float64  `json:"write_ops_per_s"`
	WriteMBPerS    float64  `json:"write_mb_per_s"`
	ReadOpsPerS    float64  `json:"read_ops_per_s"`
	ReadMBPerS     float64  `json:"read_mb_per_s"`
	AvgWriteWaitMS float64  `json:"avg_write_wait_ms"`
	BusyFraction   float64  `json:"busy_fraction"`
}

func (h *handlers) volumeIO(ctx context.Context, args json.RawMessage) (any, error) {
	var in struct {
		Sort  string `json:"sort"`
		Limit int    `json:"limit"`
	}
	if err := mcp.Decode(args, &in); err != nil {
		return nil, err
	}
	switch in.Sort {
	case "":
		in.Sort = "writes"
	case "writes", "reads", "busy":
	default:
		return nil, mcp.Errorf("sort must be writes, reads or busy")
	}
	if in.Limit <= 0 || in.Limit > 40 {
		in.Limit = 15
	}
	images, pvErr := h.pvs.lookup(ctx)
	claims, podErr := h.pods.claims(ctx)

	start := h.now()
	first, err := readRBD(h.sys)
	if err != nil {
		return nil, err
	}
	for i := 1; i < samples; i++ {
		h.sleep(sampleEvery)
	}
	last, err := readRBD(h.sys)
	if err != nil {
		return nil, err
	}
	secs := h.now().Sub(start).Seconds()
	if secs <= 0 {
		secs = float64(samples-1) * sampleEvery.Seconds()
	}

	var rows []volRow
	for id, d := range last {
		f, ok := first[id]
		if !ok || f.Image != d.Image {
			continue // mapped or remapped during sampling
		}
		claim, known := podRef{}, false
		if pvErr == nil {
			claim, known = images[d.Pool+"/"+d.Image]
		}
		if !known || strings.HasPrefix(claim.Namespace, hiddenNamespacePrefix) {
			continue // fail closed: a volume that might be familiar's is never shown
		}
		dw, dr := d.Stat.Writes-f.Stat.Writes, d.Stat.Reads-f.Stat.Reads
		r := volRow{
			Volume: claim.Namespace + "/" + claim.Name, Pods: []string{}, Device: "rbd" + id,
			WriteOpsPerS: float64(dw) / secs, ReadOpsPerS: float64(dr) / secs,
			WriteMBPerS:  float64(d.Stat.WriteSectors-f.Stat.WriteSectors) * 512 / 1e6 / secs,
			ReadMBPerS:   float64(d.Stat.ReadSectors-f.Stat.ReadSectors) * 512 / 1e6 / secs,
			BusyFraction: float64(d.Stat.BusyMS-f.Stat.BusyMS) / 1000 / secs,
		}
		if dw > 0 {
			r.AvgWriteWaitMS = float64(d.Stat.WriteMS-f.Stat.WriteMS) / float64(dw)
		}
		if podErr == nil {
			r.Pods = append(r.Pods, claims[claim]...)
			sort.Strings(r.Pods)
		}
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		switch in.Sort {
		case "reads":
			return a.ReadOpsPerS+a.ReadMBPerS > b.ReadOpsPerS+b.ReadMBPerS
		case "busy":
			return a.BusyFraction > b.BusyFraction
		}
		if a.WriteOpsPerS != b.WriteOpsPerS {
			return a.WriteOpsPerS > b.WriteOpsPerS
		}
		return a.Volume < b.Volume
	})
	if len(rows) > in.Limit {
		rows = rows[:in.Limit]
	}
	for i := range rows {
		r := &rows[i]
		r.WriteOpsPerS, r.ReadOpsPerS = round(r.WriteOpsPerS, 1), round(r.ReadOpsPerS, 1)
		r.WriteMBPerS, r.ReadMBPerS = round(r.WriteMBPerS, 3), round(r.ReadMBPerS, 3)
		r.AvgWriteWaitMS, r.BusyFraction = round(r.AvgWriteWaitMS, 1), round(min(r.BusyFraction, 1), 3)
	}
	if rows == nil {
		rows = []volRow{}
	}
	out := map[string]any{"at": start.UTC().Format(time.RFC3339), "sampled_seconds": round(secs, 2), "sort": in.Sort, "rows": rows}
	switch {
	case pvErr != nil:
		out["note"] = "volume lookup failed, so every volume is hidden"
	case podErr != nil:
		out["note"] = "pod lookup failed, so pods are not named"
	}
	return out, nil
}

// serveVolumeInfo publishes, for Prometheus, which claim each mapped RBD
// device belongs to, so node-exporter's per-device counters (rbdN) can be
// joined to claims over time (metrics-query volume templates). The same
// rule as volume_io: familiar's volumes and images it can't map are left
// out, and if listing PersistentVolumes fails nothing is published.
func (h *handlers) serveVolumeInfo(w http.ResponseWriter, r *http.Request) {
	images, err := h.pvs.lookup(r.Context())
	devs, derr := readRBD(h.sys)
	ok := err == nil && derr == nil
	var b strings.Builder
	b.WriteString("# HELP familiar_rbd_volume_info The PersistentVolumeClaim each mapped RBD device belongs to (familiar's own omitted).\n")
	b.WriteString("# TYPE familiar_rbd_volume_info gauge\n")
	if ok {
		ids := make([]string, 0, len(devs))
		for id := range devs {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			d := devs[id]
			claim, known := images[d.Pool+"/"+d.Image]
			if !known || strings.HasPrefix(claim.Namespace, hiddenNamespacePrefix) {
				continue
			}
			fmt.Fprintf(&b, "familiar_rbd_volume_info{device=%q,claim_namespace=%q,claim=%q} 1\n", "rbd"+id, claim.Namespace, claim.Name)
		}
	}
	b.WriteString("# HELP familiar_rbd_volume_lookup_ok 1 if the RBD-to-claim mapping could be built.\n# TYPE familiar_rbd_volume_lookup_ok gauge\n")
	if ok {
		b.WriteString("familiar_rbd_volume_lookup_ok 1\n")
	} else {
		b.WriteString("familiar_rbd_volume_lookup_ok 0\n")
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_, _ = io.WriteString(w, b.String())
}
