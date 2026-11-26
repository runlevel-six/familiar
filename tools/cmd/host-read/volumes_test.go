// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeRBD maps image as rbd<id> with the given cumulative counters.
func fakeRBD(t *testing.T, sys, id, image string, writes, writeSectors, writeMS, busyMS uint64) {
	t.Helper()
	dev := filepath.Join(sys, "bus/rbd/devices", id)
	must(t, os.MkdirAll(dev, 0o755))
	must(t, os.WriteFile(filepath.Join(dev, "pool"), []byte("blockpool\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(dev, "name"), []byte(image+"\n"), 0o644))
	blk := filepath.Join(sys, "block", "rbd"+id)
	must(t, os.MkdirAll(blk, 0o755))
	stat := fmt.Sprintf("10 0 80 5 %d 0 %d %d 0 %d %d 0 0 0 0 0 0\n", writes, writeSectors, writeMS, busyMS, writeMS)
	must(t, os.WriteFile(filepath.Join(blk, "stat"), []byte(stat), 0o644))
}

// setupVolumes: during 2 s of sampling, the shop volume does 80 writes of
// 4 KiB at 20 ms each; the agent's volume and an unknown image also write.
func setupVolumes(t *testing.T, kerr error) *handlers {
	sys := t.TempDir()
	fakeRBD(t, sys, "0", "csi-vol-shop", 100, 800, 500, 1000)
	fakeRBD(t, sys, "1", "csi-vol-hermes", 100, 800, 500, 1000)
	fakeRBD(t, sys, "2", "csi-vol-unknown", 100, 800, 500, 1000)
	fakeRBD(t, sys, "3", "csi-vol-old", 0, 0, 0, 0)
	calls := 0
	t0 := time.Unix(1_800_000_000, 0)
	return &handlers{
		sys:  sys,
		pods: &podIndex{g: fakeKube{kerr}, ttl: time.Minute, now: time.Now},
		pvs:  &pvIndex{g: fakeKube{kerr}, ttl: time.Minute, now: time.Now},
		sleep: func(time.Duration) {
			calls++
			if calls == samples-1 {
				fakeRBD(t, sys, "0", "csi-vol-shop", 180, 800+80*8, 500+80*20, 1000+1200)
				fakeRBD(t, sys, "1", "csi-vol-hermes", 900, 9000, 900, 2000)
				fakeRBD(t, sys, "2", "csi-vol-unknown", 900, 9000, 900, 2000)
				fakeRBD(t, sys, "3", "csi-vol-shop", 50, 400, 100, 100) // remapped mid-sample
			}
		},
		now: func() time.Time { return t0.Add(time.Duration(calls) * 500 * time.Millisecond) },
	}
}

func volCall(t *testing.T, h *handlers, args string) map[string]any {
	t.Helper()
	out, err := h.volumeIO(context.Background(), json.RawMessage(args))
	if err != nil {
		t.Fatal(err)
	}
	return out.(map[string]any)
}

func TestVolumeIO(t *testing.T) {
	rows := volCall(t, setupVolumes(t, nil), `{}`)["rows"].([]volRow)
	if len(rows) != 1 {
		t.Fatalf("want only the shop volume (familiar's, unknown and remapped hidden), got %+v", rows)
	}
	r := rows[0]
	if r.Volume != "shop/data-web-1" || r.Device != "rbd0" || len(r.Pods) != 1 || r.Pods[0] != "web-1" {
		t.Errorf("mapping: %+v", r)
	}
	// 80 writes / 2 s; 640 sectors * 512 B / 2 s; 1600 ms / 80 writes; 1.2 s busy / 2 s
	if r.WriteOpsPerS != 40 || r.WriteMBPerS != 0.164 || r.AvgWriteWaitMS != 20 || r.BusyFraction != 0.6 {
		t.Errorf("rates: %+v", r)
	}
}

func TestVolumeIOLookupFailureHidesAll(t *testing.T) {
	out := volCall(t, setupVolumes(t, errors.New("apiserver down")), `{}`)
	if rows := out["rows"].([]volRow); len(rows) != 0 || out["note"] == nil {
		t.Fatalf("shown without a volume index: %+v %v", rows, out["note"])
	}
}

func TestVolumeIORejectsBadArgs(t *testing.T) {
	h := setupVolumes(t, nil)
	for _, a := range []string{`{"sort":"name"}`, `{"device":"sdb"}`} {
		if _, err := h.volumeIO(context.Background(), json.RawMessage(a)); err == nil {
			t.Errorf("accepted %s", a)
		}
	}
}

func TestNoRBDIsEmpty(t *testing.T) {
	h := setupVolumes(t, nil)
	h.sys = t.TempDir()
	if rows := volCall(t, h, `{}`)["rows"].([]volRow); len(rows) != 0 {
		t.Fatalf("rows without rbd: %+v", rows)
	}
}

func TestVolumeInfoMetric(t *testing.T) {
	h := setupVolumes(t, nil)
	rec := httptest.NewRecorder()
	h.serveVolumeInfo(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	if !strings.Contains(body, `familiar_rbd_volume_info{device="rbd0",claim_namespace="shop",claim="data-web-1"} 1`) ||
		!strings.Contains(body, "familiar_rbd_volume_lookup_ok 1") {
		t.Errorf("metrics:\n%s", body)
	}
	for _, bad := range []string{"familiar-agents", "hermes", "rbd1", "rbd2"} { // familiar's, unknown
		if strings.Contains(body, bad) {
			t.Errorf("%q published:\n%s", bad, body)
		}
	}
	h = setupVolumes(t, errors.New("apiserver down"))
	rec = httptest.NewRecorder()
	h.serveVolumeInfo(rec, httptest.NewRequest("GET", "/metrics", nil))
	if b := rec.Body.String(); strings.Contains(b, "familiar_rbd_volume_info{") || !strings.Contains(b, "familiar_rbd_volume_lookup_ok 0") {
		t.Errorf("published without a volume index:\n%s", b)
	}
}
