// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// proc is one process as /proc/<pid>/stat and /cgroup describe it. Only files
// any user may read: never environ, cmdline, fd, io or root, which need
// ptrace rights this server doesn't have.
type proc struct {
	PID      int
	PPID     int
	Comm     string // chosen by the process itself: untrusted
	State    byte   // R, S, D, Z, ...
	Ticks    uint64 // utime + stime, in clock ticks
	Threads  int
	RSSPages int64
	Cgroup   string // cgroup v2 path
}

// readProcs reads every process under root (normally /proc). A process that
// exits while being read is skipped.
func readProcs(root string) (map[int]proc, error) {
	ents, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	out := make(map[int]proc, len(ents))
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		p, err := readProc(root, pid)
		if err != nil {
			continue
		}
		out[pid] = p
	}
	return out, nil
}

func readProc(root string, pid int) (proc, error) {
	dir := filepath.Join(root, strconv.Itoa(pid))
	stat, err := os.ReadFile(filepath.Join(dir, "stat"))
	if err != nil {
		return proc{}, err
	}
	p, err := parseStat(stat)
	if err != nil {
		return proc{}, err
	}
	p.PID = pid
	if cg, err := os.ReadFile(filepath.Join(dir, "cgroup")); err == nil {
		p.Cgroup = parseCgroup(cg)
	}
	return p, nil
}

var errBadStat = errors.New("malformed stat")

// parseStat parses /proc/<pid>/stat. The comm field is in parentheses and
// may itself contain spaces and parentheses, so split at the last ')'.
func parseStat(b []byte) (proc, error) {
	open := bytes.IndexByte(b, '(')
	end := bytes.LastIndexByte(b, ')')
	if open < 0 || end < open {
		return proc{}, errBadStat
	}
	f := strings.Fields(string(b[end+1:]))
	// f[0] is field 3 (state); field N is f[N-3].
	if len(f) < 22 {
		return proc{}, errBadStat
	}
	num := func(n int) uint64 { v, _ := strconv.ParseUint(f[n-3], 10, 64); return v }
	snum := func(n int) int64 { v, _ := strconv.ParseInt(f[n-3], 10, 64); return v }
	return proc{
		Comm:     string(b[open+1 : end]),
		State:    f[0][0],
		PPID:     int(snum(4)),
		Ticks:    num(14) + num(15),
		Threads:  int(snum(20)),
		RSSPages: snum(24),
	}, nil
}

// ownCgroup marks a process at the root of this server's own cgroup
// namespace: the server itself (see parseCgroup).
const ownCgroup = "(own)"

// parseCgroup returns the cgroup v2 path ("0::/path"). The pod has its own
// cgroup namespace, so the kernel shows other processes' paths relative to
// it, with a "/.." for each level above (cgroup_namespaces(7)):
// "/../../system.slice/kubelet.service". Those are stripped back to the
// host path. A path of exactly "/" is the namespace's own root, which only
// this server's container uses: ownCgroup.
func parseCgroup(b []byte) string {
	for _, line := range strings.Split(string(b), "\n") {
		p, ok := strings.CutPrefix(line, "0::")
		if !ok {
			continue
		}
		if p == "/" {
			return ownCgroup
		}
		for strings.HasPrefix(p, "/../") {
			p = p[3:]
		}
		if p == "/.." {
			p = "/"
		}
		return p
	}
	return ""
}

var rePodSlice = regexp.MustCompile(`pod([0-9a-f_]+)\.slice`)

// podUID returns the pod UID in a kubepods cgroup path, with the dashes
// systemd turned into underscores restored, or "".
func podUID(cgroup string) string {
	m := rePodSlice.FindStringSubmatch(cgroup)
	if m == nil || !strings.Contains(cgroup, "kubepods") {
		return ""
	}
	return strings.ReplaceAll(m[1], "_", "-")
}

// systemdGroup names a non-pod process by its cgroup: a systemd service, a
// login session, or "other".
func systemdGroup(cgroup string) string {
	switch {
	case strings.HasPrefix(cgroup, "/user.slice/"):
		return "user session"
	case cgroup == "/init.scope":
		return "systemd (init)"
	case strings.HasPrefix(cgroup, "/system.slice/"):
		unit := strings.TrimPrefix(cgroup, "/system.slice/")
		if i := strings.IndexByte(unit, '/'); i >= 0 {
			unit = unit[:i]
		}
		return "systemd " + unit
	}
	return "other"
}
