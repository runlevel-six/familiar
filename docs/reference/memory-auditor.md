# memory-auditor reference

A small Python program (standard library only) that runs next to Hermes, reads
its data volume read-only, and commits a normalised snapshot to a git repository
on its own volume whenever something changed. It has no network access except
for Prometheus scraping its metrics. Source:
[`deploy/familiar/hermes/memory-auditor.py`](../../deploy/familiar/hermes/memory-auditor.py).
How to use what it records: [Review and restore GIDEON's memory](../how-to/review-and-restore-memory.md).

## Environment variables

| Variable | Default | |
|---|---|---|
| `SRC` | `/hermes` | Hermes' data volume, mounted read-only |
| `REPO` | `/audit/repo` | The git repository it writes (created if missing) |
| `BASELINE_CONFIG` | `/baseline/config.yaml` | The ConfigMap copy of Hermes' `config.yaml`, to detect drift |
| `INTERVAL` | `300` | Seconds between runs |
| `ONCE` | | Run once and exit (for tests) |

## What it records

| Area | Contents |
|---|---|
| `memories/` | Every file directly in `memories/` (`MEMORY.md`, `USER.md`) |
| `cron/jobs.json` | Job definitions, keeping only definition fields (id, name, prompt, schedule, model, provider, script, delivery, enabled toolsets, …) and dropping the run state the scheduler rewrites every tick |
| `config/` | `config.yaml` as it is on the volume, and `drift.txt` saying whether it matches the ConfigMap |
| `files/inventory.tsv` | Path, size and SHA-256 of every other file; symlinks as their target |

Files over 1 MiB are hashed, not copied. Symlinks are recorded, never followed,
and nothing from the volume is executed.

## What it skips

Patterns are matched against the whole path, one segment at a time; a pattern
ending in `/` is a directory. Names are exact: `gateway/` doesn't cover
`gateway-hook.sh`. The lists come from a real Hermes v0.23.3 volume. Anything
Hermes writes that isn't listed shows up in `files/` and raises an alert, which is
the safe direction.

- **Never read:** `.env`, `auth.json`, `auth.lock`, `install_id`,
  `log_hash_salt`, and everything under `platforms/matrix/` (the end-to-end
  encryption keys and recovery key).
- **Noisy directories:** sessions, logs, caches, runtime state, pending messages,
  gateway and kanban state, plugin-update checks, installs, config backups, cron
  output and admissions. Inside them, only **executables and symlinks** are
  inventoried; Hermes writes neither there.
- **Noisy files:** gateway pid, lock, socket and state files; the spawn ledger;
  the channel directory; lock files; Hermes' SQLite databases (`state.db`,
  `kanban.db`, `response_store.db`, `runs_idempotency.db`, `projects.db`, and in
  `cron/` its delivery, execution and notepad databases) with their `-shm`, `-wal`
  and lock files; the scheduler's heartbeat and usage files.
- **Covered by another area:** `config.yaml`, files directly in `memories/`, and
  `cron/jobs.json`. Anything else in `memories/` or `cron/` is inventoried.

The exact lists are at the top of the source.

## Metrics

On port 9100, at `/metrics` (and `/healthz`):

| Metric | Labels | |
|---|---|---|
| `memory_auditor_last_run_timestamp_seconds` | | When the last run finished |
| `memory_auditor_last_change_timestamp_seconds` | `area` | When each area last changed |
| `memory_auditor_changes_total` | `area` | Commits that changed each area |
| `memory_auditor_errors_total` | | Failed runs |

The first snapshot is committed as the baseline and doesn't count as a change.
The alert rules are in [Alerts](alerts.md).

## Tests

```sh
python3 -I deploy/familiar/hermes/memory_auditor_test.py
```
