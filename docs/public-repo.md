# Public repo, private details

This repository is meant to be public. It holds familiar's design, code and
generic manifests. Everything specific to one home network or to people lives in
`local/`, which is gitignored and kept as its own git repo with no remote: the
real kustomize overlay, the monitoring and Matrix deployments, host backup
scripts, runbooks, the cluster inventory, incident notes, and the denylist.
Public files may refer to those paths as `local/...`; a reader without them
sees what exists, not what it says.

## What goes where

| Public (tracked) | Private (`local/`) |
|---|---|
| Design reasoning, threat model, decisions | Hostnames, domains, addresses, Matrix IDs |
| model-gate, tool servers, Hermes config and persona | The apps running on the cluster and their data |
| `deploy/familiar` as a base with placeholders, `deploy/overlays/example` | The real overlay (`local/deploy/familiar`) |
| Generic tests and harnesses (site values from the environment) | Node access, disks, hardware identifiers |
| | People and the organisations they work for |

When in doubt, write it generically in the public file and keep the specifics
in `local/`.

## Guardrails

- `scripts/check-private.sh` reads `local/denylist.txt` and fails if the staged
  diff or the commit message contains a listed term (case-insensitive, whole
  word), or if anything under `local/` is tracked. `--all` scans every tracked
  file; `--range A..B` scans a commit range and its messages.
- `.githooks/` runs it on every commit, plus `betterleaks` for secrets when it
  is installed. Enable once per clone: `git config core.hooksPath .githooks`.
- Add a term to the denylist whenever a new private name appears (a new app,
  host or person).

## History (2026-11-23)

The repository's history was squashed into one commit before it went public:
the earlier history contained everything that now lives in `local/`. A force
push wouldn't have been enough, because GitHub keeps unreachable commits
reachable by SHA, and the image tags (`0.2.0-86b0d42`, ...) name some of those
SHAs. So the old GitHub repository was deleted and re-created, and the full
private history is kept only as `local/familiar-history.bundle`
(`git clone local/familiar-history.bundle` restores it) and the local
`history` branch. Image tags may therefore name commits that don't exist here.

Image versions built from that history that contained site names were deleted
from GHCR the same day.

## Made public 2026-11-23

To keep it clean:

1. `scripts/check-private.sh --all` and `betterleaks dir --redact .` both pass.
2. Protect `main`: done 2026-11-23, a ruleset (no deletion, no force push, pull request required; Jase's account bypasses).
3. New source files keep the `SPDX-License-Identifier: Apache-2.0` header
   (`LICENSE`, `NOTICE`).
