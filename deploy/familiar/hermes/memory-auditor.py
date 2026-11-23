# SPDX-License-Identifier: Apache-2.0
"""memory-auditor: records changes to GIDEON's persistent state in git (design §4.1).

Detection, not prevention. Every INTERVAL seconds it reads the Hermes data
volume (mounted read-only) and writes a normalised snapshot into a git repo on
its own volume, which the agent can't reach, then commits if anything changed.
Areas:

  memories/  MEMORY.md, USER.md: what the agent remembers (expected to change)
  cron/      job definitions from cron/jobs.json, run state stripped
  config/    config.yaml, which the pod seeds from the ConfigMap at each start;
             any difference from the ConfigMap copy is drift
  files/     an inventory (path, size, sha256) of everything else in the
             volume except exactly-named noisy files, to spot unexpected
             files; inside noisy directories only executables and symlinks

Never read or copied: .env, platforms/matrix (E2EE keys, recovery key),
auth and install IDs, SQLite databases, sessions/ and logs/. The volume is
data written by the agent: symlinks are recorded, never followed, files over
MAX_BYTES are hashed but not copied, and nothing from it is executed.

Prometheus metrics on :9100/metrics:
  memory_auditor_last_run_timestamp_seconds
  memory_auditor_last_change_timestamp_seconds{area}
  memory_auditor_changes_total{area}
  memory_auditor_errors_total
Review: kubectl -n familiar-agents exec deploy/memory-auditor -- git -C /audit/repo log -p
Restore: see docs/how-to/review-and-restore-memory.md.
"""

import fnmatch
import hashlib
import http.server
import json
import os
import shutil
import subprocess
import sys
import threading
import time

SRC = os.environ.get("SRC", "/hermes")
REPO = os.environ.get("REPO", "/audit/repo")
BASELINE = os.environ.get("BASELINE_CONFIG", "/baseline/config.yaml")
INTERVAL = int(os.environ.get("INTERVAL", "300"))
MAX_BYTES = 1 << 20
AREAS = ("memories", "cron", "config", "files")

# Path patterns, matched against the whole relative path one segment at a time
# (fnmatch per segment, so "*" never crosses a "/"). A pattern ending in "/"
# names a directory. Names are exact: "gateway/" doesn't cover "gateway-hook.sh".
# The lists come from a real Hermes v0.23.3 data volume (tests/hermes-local);
# anything Hermes writes that isn't listed shows up in files/ and alerts, which
# is the safe direction: add it here once it's known to be Hermes' own.
#
# Never read: secrets and the agent's private state. Skipped entirely.
SECRET = (".env", "auth.json", "auth.lock", "install_id", "log_hash_salt", "platforms/matrix/")
# Change constantly by design. Directories: only executables and symlinks in
# them are inventoried (Hermes writes neither there). Files: not inventoried.
NOISY = (
    "sessions/", "logs/", "cache/", "audio_cache/", "image_cache/", "state/", "runtime/",
    "pending_messages/", ".local/", "gateway/", "kanban/", "plugin-update-checks/", "installs/", "backups/",
    "gateway-starts.log", "gateway.lock", "gateway.pid", "gateway.sock", "gateway_state.json",
    "spawn-ledger.json", "spawn-ledger.json.lock", "channel_directory.json", ".install_id.lock", ".mcp-discovery.lock",
    # SQLite databases and their side files, top level only
    *(f"{db}{suffix}" for db in ("state.db", "kanban.db", "response_store.db", "runs_idempotency.db", "projects.db")
      for suffix in ("", "-shm", "-wal", "-journal", ".*.lock")),
    # cron/: the scheduler's own state (jobs.json is the cron area)
    "cron/output/", "cron/admissions/", "cron/.fire-*.lock", "cron/.jobs.lock", "cron/.tick.lock",
    "cron/ticker_heartbeat", "cron/ticker_last_success", "cron/usage_audit.jsonl",
    "cron/alert-gate-state.json", "cron/alert-gate-test-state.json",
    *(f"cron/{db}{suffix}" for db in ("deliveries.db", "executions.db", "notepad.db")
      for suffix in ("", "-shm", "-wal", "-journal")),
)
# Recorded by another area. Only these exact files: anything else in memories/
# or cron/ (a subdirectory, a script) is inventoried in files/.
COVERED = ("config.yaml", "memories/*", "cron/jobs.json")
# cron/jobs.json: keep the definition, drop the run state the scheduler rewrites every tick.
JOB_KEYS = ("id", "name", "prompt", "schedule", "schedule_display", "repeat", "model", "provider", "base_url",
            "script", "no_agent", "skills", "skill", "deliver", "enabled", "enabled_toolsets", "origin", "workdir",
            "context_from", "monitor_script", "monitor_url", "paused_at", "paused_reason", "created_at")

metrics = {"last_run": 0.0, "errors": 0, "changes": {a: 0 for a in AREAS}, "last_change": {a: 0.0 for a in AREAS}}
lock = threading.Lock()


def log(msg, **kw):
    print(json.dumps({"time": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), "msg": msg, **kw}), flush=True)


def matches(rel, patterns, is_dir=False):
    """True if rel (a path relative to SRC, no trailing slash) matches one of
    patterns: a directory pattern matches that directory and anything under
    it; a file pattern matches only a file at exactly that path."""
    segs = rel.split("/")
    for p in patterns:
        dir_pat = p.endswith("/")
        psegs = p.rstrip("/").split("/")
        if dir_pat:
            # the directory itself, or anything below it
            if len(segs) < len(psegs) or (len(segs) == len(psegs) and not is_dir):
                continue
        elif is_dir or len(segs) != len(psegs):
            continue
        if all(fnmatch.fnmatchcase(s, ps) for s, ps in zip(segs, psegs)):
            return True
    return False


def inventory_line(rel, path):
    if os.path.islink(path):
        return f"{rel}\tsymlink -> {os.readlink(path)}"
    if not os.path.isfile(path):
        return None
    try:
        return f"{rel}\t{os.path.getsize(path)}\t{sha256(path)}"
    except PermissionError:
        return f"{rel}\t{os.path.getsize(path)}\tunreadable"


def is_executable(path):
    try:
        return os.lstat(path).st_mode & 0o111 != 0
    except OSError:
        return False


def sha256(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 16), b""):
            h.update(chunk)
    return h.hexdigest()


def read_baseline(path):
    """The ConfigMap copy of config.yaml. Unlike files on Hermes' volume, this
    is ours and mounted read-only, and Kubernetes mounts ConfigMap keys as
    symlinks (config.yaml -> ..data/config.yaml), so follow them here. If it
    can't be read, return None: drift is then reported as unknown, never as
    a match or a difference."""
    try:
        with open(path, encoding="utf-8", errors="replace") as f:
            return f.read()
    except OSError:
        return None


def read_text(path):
    if os.path.islink(path) or not os.path.isfile(path):
        return None
    if os.path.getsize(path) > MAX_BYTES:
        return f"<{os.path.getsize(path)} bytes, sha256 {sha256(path)}, too large to copy>\n"
    with open(path, encoding="utf-8", errors="replace") as f:
        return f.read()


def snapshot(out):
    """Write the normalised snapshot of SRC into out/."""
    # memories/
    os.makedirs(os.path.join(out, "memories"))
    mdir = os.path.join(SRC, "memories")
    if os.path.isdir(mdir):
        for name in sorted(os.listdir(mdir)):
            text = read_text(os.path.join(mdir, name))
            if text is not None:
                with open(os.path.join(out, "memories", name), "w") as f:
                    f.write(text)
    # cron/
    os.makedirs(os.path.join(out, "cron"))
    jobs = []
    jpath = os.path.join(SRC, "cron", "jobs.json")
    if os.path.isfile(jpath) and not os.path.islink(jpath):
        with open(jpath) as f:
            raw = json.load(f)
        for j in raw.get("jobs", raw if isinstance(raw, list) else []):
            jobs.append({k: j.get(k) for k in JOB_KEYS if k in j})
    jobs.sort(key=lambda j: str(j.get("id")))
    with open(os.path.join(out, "cron", "jobs.json"), "w") as f:
        json.dump(jobs, f, indent=2, sort_keys=True)
        f.write("\n")
    # config/
    os.makedirs(os.path.join(out, "config"))
    live = read_text(os.path.join(SRC, "config.yaml")) or ""
    base = read_baseline(BASELINE)
    with open(os.path.join(out, "config", "config.yaml"), "w") as f:
        f.write(live)
    with open(os.path.join(out, "config", "drift.txt"), "w") as f:
        if base is None:
            f.write("UNKNOWN: the ConfigMap copy can't be read (%s)\n" % BASELINE)
        else:
            f.write("matches the ConfigMap\n" if live == base else "DIFFERS from the ConfigMap (see config.yaml)\n")
    # files/
    os.makedirs(os.path.join(out, "files"))
    lines = []
    for root, dirs, files in os.walk(SRC, followlinks=False):
        rel_root = os.path.relpath(root, SRC)
        rel_root = "" if rel_root == "." else rel_root + "/"
        in_noisy = rel_root != "" and matches(rel_root.rstrip("/"), NOISY, is_dir=True)
        dirs[:] = sorted(d for d in dirs if not matches(rel_root + d, SECRET, is_dir=True))
        # os.walk lists symlinks to directories under dirs; record them as links.
        for d in dirs:
            if os.path.islink(os.path.join(root, d)):
                lines.append(f"{rel_root + d}\tsymlink -> {os.readlink(os.path.join(root, d))}")
        for name in sorted(files):
            rel, path = rel_root + name, os.path.join(root, name)
            if matches(rel, SECRET + COVERED):
                continue
            if in_noisy:
                if not (os.path.islink(path) or is_executable(path)):
                    continue
            elif matches(rel, NOISY):  # an exact known file (gateway.pid is written 0755)
                continue
            line = inventory_line(rel, path)
            if line:
                lines.append(line)
    with open(os.path.join(out, "files", "inventory.tsv"), "w") as f:
        f.write("\n".join(lines) + ("\n" if lines else ""))


def git(*args):
    return subprocess.run(["git", "-C", REPO, *args], check=True, capture_output=True, text=True).stdout


def run_once():
    tmp = REPO + ".new"
    shutil.rmtree(tmp, ignore_errors=True)
    os.makedirs(tmp)
    snapshot(tmp)
    for area in AREAS:
        shutil.rmtree(os.path.join(REPO, area), ignore_errors=True)
        shutil.move(os.path.join(tmp, area), os.path.join(REPO, area))
    os.rmdir(tmp)
    first = subprocess.run(["git", "-C", REPO, "rev-parse", "--verify", "-q", "HEAD"], capture_output=True).returncode != 0
    git("add", "-A")
    changed = [line.split("\t", 1)[1] for line in git("diff", "--cached", "--name-status").splitlines() if "\t" in line]
    now = time.time()
    if changed:
        areas = sorted({p.split("/")[0] for p in changed})
        title = "baseline snapshot" if first else f"{', '.join(areas)}: {len(changed)} file(s) changed"
        git("commit", "-q", "-m", title, "-m", "\n".join(changed))
        if first:  # the baseline isn't a change; don't alert on deploy
            log("baseline recorded", files=changed)
            with lock:
                metrics["last_run"] = now
            return
        with lock:
            for a in areas:
                if a in metrics["changes"]:
                    metrics["changes"][a] += 1
                    metrics["last_change"][a] = now
        log("change recorded", areas=areas, files=changed)
    with lock:
        metrics["last_run"] = now


class Metrics(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path not in ("/metrics", "/healthz"):
            self.send_error(404)
            return
        with lock:
            m = json.loads(json.dumps(metrics))
        body = "ok\n" if self.path == "/healthz" else "".join([
            f"memory_auditor_last_run_timestamp_seconds {m['last_run']}\n",
            f"memory_auditor_errors_total {m['errors']}\n",
            *[f'memory_auditor_changes_total{{area="{a}"}} {m["changes"][a]}\n' for a in AREAS],
            *[f'memory_auditor_last_change_timestamp_seconds{{area="{a}"}} {m["last_change"][a]}\n' for a in AREAS],
        ])
        self.send_response(200)
        self.send_header("Content-Type", "text/plain; version=0.0.4")
        self.end_headers()
        self.wfile.write(body.encode())

    def log_message(self, *_):
        pass


def main():
    os.makedirs(REPO, exist_ok=True)
    if not os.path.isdir(os.path.join(REPO, ".git")):
        subprocess.run(["git", "init", "-q", REPO], check=True)
    git("config", "user.name", "memory-auditor")
    git("config", "user.email", "memory-auditor@familiar.invalid")
    git("config", "commit.gpgsign", "false")
    threading.Thread(target=http.server.ThreadingHTTPServer(("", 9100), Metrics).serve_forever, daemon=True).start()
    log("started", src=SRC, repo=REPO, interval=INTERVAL)
    while True:
        try:
            run_once()
        except Exception as e:  # keep running; the error count is alerted on
            with lock:
                metrics["errors"] += 1
            log("run failed", error=f"{type(e).__name__}: {e}")
        if os.environ.get("ONCE"):
            return
        time.sleep(INTERVAL)


if __name__ == "__main__":
    sys.exit(main())
