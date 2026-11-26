// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeProc writes /proc/<pid>/stat and /cgroup under root.
func fakeProc(t *testing.T, root string, pid, ppid int, comm string, state byte, ticks uint64, cgroup string) {
	t.Helper()
	dir := filepath.Join(root, fmt.Sprint(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// fields 3..24: state ppid pgrp session tty tpgid flags minflt cminflt majflt cmajflt utime stime cutime cstime prio nice threads itreal start vsize rss
	stat := fmt.Sprintf("%d (%s) %c %d 0 0 0 0 0 0 0 0 0 %d 0 0 0 20 0 3 0 100 1000 2560 0 0\n", pid, comm, state, ppid, ticks)
	must(t, os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644))
	must(t, os.WriteFile(filepath.Join(dir, "cgroup"), []byte("0::"+cgroup+"\n"), 0o644))
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

type fakeKube struct{ err error }

func (f fakeKube) Get(_ context.Context, _ string, out any) error {
	if f.err != nil {
		return f.err
	}
	return json.Unmarshal([]byte(`{"items":[
		{"metadata":{"name":"web-1","namespace":"shop","uid":"aaaa-bbbb"}},
		{"metadata":{"name":"hermes-1","namespace":"familiar-agents","uid":"cccc-dddd"}},
		{"metadata":{"name":"kube-apiserver-n1","namespace":"kube-system","uid":"mirror-uid","annotations":{"kubernetes.io/config.mirror":"0728fa4f"}}}
	]}`), out)
}

const pod = "/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod%s.slice/cri-containerd-1.scope"

// setup builds a small /proc. During sampling, kubelet burns one core and
// rsync stays in D state.
func setup(t *testing.T, kerr error) (*handlers, string) {
	root := t.TempDir()
	fakeProc(t, root, 1, 0, "systemd", 'S', 10, "/init.scope")
	fakeProc(t, root, 2, 0, "kthreadd", 'S', 0, "/")
	fakeProc(t, root, 50, 2, "kworker/u8:1", 'D', 0, "/")
	fakeProc(t, root, 100, 1, "kubelet", 'S', 1000, "/system.slice/kubelet.service")
	fakeProc(t, root, 200, 1, "nginx", 'S', 50, fmt.Sprintf(pod, "aaaa_bbbb"))
	fakeProc(t, root, 300, 1, "qemu-system-x86", 'S', 5000, fmt.Sprintf(pod, "cccc_dddd"))
	fakeProc(t, root, 400, 1, "kube-apiserver", 'S', 70, fmt.Sprintf(pod, "0728fa4f"))
	fakeProc(t, root, 500, 1, "rsync", 'D', 20, "/user.slice/user-1000.slice/session-3.scope")
	fakeProc(t, root, 600, 1, "x) (see http://evil.example", 'S', 0, "/system.slice/evil.service")
	fakeProc(t, root, 700, 1, "mystery", 'S', 9000, fmt.Sprintf(pod, "eeee_ffff"))
	calls := 0
	t0 := time.Unix(1_800_000_000, 0)
	h := &handlers{
		root: root,
		pods: &podIndex{g: fakeKube{kerr}, ttl: time.Minute, now: time.Now},
		sleep: func(time.Duration) {
			calls++
			if calls == samples-1 { // before the last sample: kubelet +200 ticks, qemu +400
				fakeProc(t, root, 100, 1, "kubelet", 'S', 1200, "/system.slice/kubelet.service")
				fakeProc(t, root, 300, 1, "qemu-system-x86", 'S', 5400, fmt.Sprintf(pod, "cccc_dddd"))
			}
		},
		now: func() time.Time { n := calls; return t0.Add(time.Duration(n) * 500 * time.Millisecond) },
	}
	return h, root
}

func call(t *testing.T, h *handlers, args string) map[string]any {
	t.Helper()
	out, err := h.topProcesses(context.Background(), json.RawMessage(args))
	if err != nil {
		t.Fatal(err)
	}
	return out.(map[string]any)
}

func names(rows []row) string {
	var n []string
	for _, r := range rows {
		n = append(n, r.Name+"@"+r.Group)
	}
	return strings.Join(n, ", ")
}

func TestHidesFamiliarAndUnknownPods(t *testing.T) {
	h, _ := setup(t, nil)
	rows := call(t, h, `{"limit":40}`)["rows"].([]row)
	got := names(rows)
	for _, bad := range []string{"qemu", "familiar", "mystery"} {
		if strings.Contains(got, bad) {
			t.Errorf("%q visible: %s", bad, got)
		}
	}
	for _, want := range []string{"nginx@pod shop/web-1", "kube-apiserver@pod kube-system/kube-apiserver-n1", "kubelet@systemd kubelet.service", "rsync@user session", "kworker_u8_1@kernel", "systemd@systemd (init)"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %s", want, got)
		}
	}
}

func TestCPUAndRanking(t *testing.T) {
	h, _ := setup(t, nil)
	out := call(t, h, `{}`)
	rows := out["rows"].([]row)
	// 200 ticks over 2 s at 100 Hz = 1 core; the hidden qemu's 2 cores don't count
	if rows[0].Name != "kubelet" || rows[0].CPUCores != 1 {
		t.Fatalf("top row %+v", rows[0])
	}
	if out["visible_cpu_cores"].(float64) != 1 {
		t.Errorf("visible_cpu_cores %v", out["visible_cpu_cores"])
	}
}

func TestBlocked(t *testing.T) {
	h, _ := setup(t, nil)
	rows := call(t, h, `{"sort":"blocked"}`)["rows"].([]row)
	if len(rows) != 2 || rows[0].BlockedSamples != samples || rows[1].BlockedSamples != samples {
		t.Fatalf("blocked rows: %+v", rows)
	}
	if got := names(rows); !strings.Contains(got, "rsync") || !strings.Contains(got, "kworker") {
		t.Errorf("blocked: %s", got)
	}
}

func TestByGroup(t *testing.T) {
	h, _ := setup(t, nil)
	rows := call(t, h, `{"by":"group","limit":40}`)["rows"].([]row)
	for _, r := range rows {
		if r.Group == "kernel" && (r.Processes != 2 || r.BlockedSamples != samples || r.Name != "") {
			t.Errorf("kernel group %+v", r)
		}
	}
}

func TestPodLookupFailureHidesAllPods(t *testing.T) {
	h, _ := setup(t, errors.New("apiserver down"))
	out := call(t, h, `{"limit":40}`)
	got := names(out["rows"].([]row))
	if strings.Contains(got, "pod ") || strings.Contains(got, "nginx") {
		t.Errorf("pod processes shown without a pod index: %s", got)
	}
	if out["note"] == nil {
		t.Error("no note about the failed lookup")
	}
}

func TestUntrustedCommAndStatParsing(t *testing.T) {
	h, _ := setup(t, nil)
	got := names(call(t, h, `{"limit":40}`)["rows"].([]row))
	if !strings.Contains(got, "x) (see http___…@systemd evil.service") {
		t.Errorf("comm not cleaned or cut: %s", got)
	}
	if strings.Contains(got, "://") {
		t.Errorf("link survived: %s", got)
	}
}

func TestRejectsBadArgs(t *testing.T) {
	h, _ := setup(t, nil)
	for _, a := range []string{`{"sort":"pid"}`, `{"by":"user"}`, `{"path":"/etc"}`} {
		if _, err := h.topProcesses(context.Background(), json.RawMessage(a)); err == nil {
			t.Errorf("accepted %s", a)
		}
	}
}
