# SPDX-License-Identifier: Apache-2.0
"""Tests for memory-auditor.py's file inventory (stdlib only).

Run: python3 -I deploy/familiar/hermes/memory_auditor_test.py
Not shipped: the kustomization's configMapGenerator lists only memory-auditor.py.
"""

import importlib.util
import os
import stat
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
spec = importlib.util.spec_from_file_location("memory_auditor", os.path.join(HERE, "memory-auditor.py"))
ma = importlib.util.module_from_spec(spec)
spec.loader.exec_module(ma)

# Files a real Hermes v0.23.3 volume holds (tests/hermes-local, 2026-11-23)
# that are noisy, secret or covered by another area: none may be inventoried.
EXPECTED_QUIET = [
    ".env", "auth.lock", "install_id", "log_hash_salt", ".install_id.lock", ".mcp-discovery.lock",
    "platforms/matrix/store/keys.json", "config.yaml", "memories/MEMORY.md", "memories/USER.md",
    "cron/jobs.json", "cron/.fire-3b2f727268135f7386b89e32d4d53a19.lock", "cron/.jobs.lock", "cron/.tick.lock",
    "cron/alert-gate-state.json", "cron/deliveries.db", "cron/executions.db", "cron/notepad.db",
    "cron/ticker_heartbeat", "cron/ticker_last_success", "cron/usage_audit.jsonl", "cron/output/job/1.md",
    "gateway-starts.log", "gateway.lock", "gateway.pid", "gateway.sock", "gateway_state.json",
    "gateway/lifecycle_notice_loop.json", "kanban.db", "kanban.db-shm", "kanban.db-wal",
    "kanban.db.dispatch.lock", "kanban.db.init.lock", "kanban/.dispatcher.lock",
    "state.db", "state.db-shm", "state.db-wal", "state.db.fts_rebuild.lock", "state.db.quarantine.lock",
    "response_store.db", "runs_idempotency.db-wal", "projects.db", "spawn-ledger.json", "spawn-ledger.json.lock",
    "channel_directory.json", "logs/agent.log", "sessions/sessions.json", "cache/mcp_schema_cache.json",
    "state/gateway.heartbeat", "runtime/active_sessions.json", "plugin-update-checks/results.json",
    "installs/351f81cc06284494/bootstrap/default.json", "backups/config/config.yaml.good.20261122-212014",
]

# Files the agent could plant; every one must be inventoried.
PLANTED = [
    "gatewayx.sh", "gateway-hook.sh", "kanban-evil/run.sh", "kanban_payload/x", "spawn-ledger.json.bak",
    "channel_directory.json.d/x", "home/x.db", "home/x.lock", "home/x.pid", "notes.db", "state.dbx",
    "config.yaml.bak", ".env.example", "install_id2", "memories/sub/hidden.md", "cron/evil.sh",
    "cron/output.sh", "logsx/a.log", "platforms/other/keys.json", "SOUL.md", "scripts/alert-gate.py",
]


def touch(root, rel, mode=0o644):
    path = os.path.join(root, rel)
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w") as f:
        f.write('{"jobs": []}' if rel.endswith(".json") else rel)
    os.chmod(path, mode)


class InventoryTest(unittest.TestCase):
    def inventory(self, setup):
        with tempfile.TemporaryDirectory() as src, tempfile.TemporaryDirectory() as out:
            setup(src)
            ma.SRC, ma.BASELINE = src, os.path.join(src, "config.yaml")
            ma.snapshot(out)
            with open(os.path.join(out, "files", "inventory.tsv")) as f:
                return {line.split("\t")[0]: line for line in f.read().splitlines()}

    def test_known_noise_is_quiet_and_planted_files_are_recorded(self):
        def setup(src):
            for rel in EXPECTED_QUIET + PLANTED:
                touch(src, rel)
            os.chmod(os.path.join(src, "gateway.pid"), 0o755)  # as Hermes writes it
        inv = self.inventory(setup)
        for rel in EXPECTED_QUIET:
            self.assertNotIn(rel, inv, f"noisy/secret/covered file inventoried: {rel}")
        for rel in PLANTED:
            self.assertIn(rel, inv, f"planted file missed: {rel}")

    def test_executables_and_symlinks_in_noisy_dirs_are_recorded(self):
        def setup(src):
            touch(src, "logs/agent.log")
            touch(src, "logs/run.sh", 0o755)
            touch(src, "cache/scratch/payload", 0o700)
            os.symlink("/etc/passwd", os.path.join(src, "sessions-link"))
            os.makedirs(os.path.join(src, "sessions"))
            os.symlink("/", os.path.join(src, "sessions", "root"))
            os.symlink("/tmp", os.path.join(src, "dirlink"))
        inv = self.inventory(setup)
        self.assertNotIn("logs/agent.log", inv)
        self.assertIn("logs/run.sh", inv)
        self.assertIn("cache/scratch/payload", inv)
        self.assertIn("sessions-link\tsymlink -> /etc/passwd", inv.values())
        self.assertIn("sessions/root\tsymlink -> /", inv.values())
        self.assertIn("dirlink\tsymlink -> /tmp", inv.values())

    def test_secret_dirs_are_never_read(self):
        def setup(src):
            touch(src, "platforms/matrix/recovery_key", 0o755)
            os.symlink("/", os.path.join(src, "platforms", "matrix", "link"))
        inv = self.inventory(setup)
        self.assertFalse([k for k in inv if k.startswith("platforms/matrix")], inv)

    def test_drift_follows_a_symlinked_baseline(self):
        # Kubernetes mounts ConfigMap keys as symlinks (config.yaml -> ..data/config.yaml);
        # found 2026-11-23 when drift always said DIFFERS in the cluster.
        with tempfile.TemporaryDirectory() as src, tempfile.TemporaryDirectory() as cm, \
                tempfile.TemporaryDirectory() as out:
            touch(src, "config.yaml")
            os.makedirs(os.path.join(cm, "..data"))
            with open(os.path.join(cm, "..data", "config.yaml"), "w") as f:
                f.write("config.yaml")
            os.symlink("..data/config.yaml", os.path.join(cm, "config.yaml"))
            ma.SRC, ma.BASELINE = src, os.path.join(cm, "config.yaml")
            ma.snapshot(out)
            with open(os.path.join(out, "config", "drift.txt")) as f:
                self.assertEqual(f.read(), "matches the ConfigMap\n")
        with tempfile.TemporaryDirectory() as src, tempfile.TemporaryDirectory() as out:
            touch(src, "config.yaml")
            ma.SRC, ma.BASELINE = src, os.path.join(src, "missing.yaml")
            ma.snapshot(out)
            with open(os.path.join(out, "config", "drift.txt")) as f:
                self.assertTrue(f.read().startswith("UNKNOWN"))

    def test_matches(self):
        m = ma.matches
        self.assertTrue(m("gateway", ("gateway/",), is_dir=True))
        self.assertFalse(m("gateway", ("gateway/",)))  # a *file* named gateway isn't the dir
        self.assertTrue(m("gateway/x/y", ("gateway/",)))
        self.assertFalse(m("gateway-hook.sh", ("gateway/",)))
        self.assertTrue(m("cron/.fire-abc.lock", ("cron/.fire-*.lock",)))
        self.assertFalse(m("cron/.fire-a/b.lock", ("cron/.fire-*.lock",)))  # "*" stays in one segment
        self.assertTrue(m("memories/MEMORY.md", ("memories/*",)))
        self.assertFalse(m("memories/sub/x.md", ("memories/*",)))


if __name__ == "__main__":
    unittest.main()
