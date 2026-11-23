# familiar Phase 1 manifests (DRAFT, not applied)

Phase 1 starts after the Phase 0 exit criteria (design §5.5). These files are drafts and a generic kustomize base (`kustomization.yaml`); site values come from an overlay (`../overlays/example/`). All of them pass `kubectl apply --dry-run=server --validate=strict`, and the Deployments are restricted-PSS compliant (dry-run in a restricted namespace, 2026-11-22). **None has been applied.**

| File | What |
|---|---|
| `00-namespaces.yaml` | `familiar-system`, `familiar-agents`, `familiar-tools`, each enforcing the restricted Pod Security Standard |
| `01-priority-and-quotas.yaml` | `familiar-low` PriorityClass (-100, never preempts), ResourceQuota and LimitRange per namespace |
| `02-default-deny.yaml` | Default-deny ingress and egress CNP in each namespace |
| `10-model-gate.yaml` | model-gate: Deployment, config, state PVC, Service; ingress (watch officer to port 8081, Prometheus to 9090) and egress (`api.anthropic.com:443` only) policies; ServiceMonitor and alert rules in `monitoring` |
| `20-tool-servers.yaml` | The four tool servers, cluster-read's ClusterRole, per-server egress policies, ingress from the watch officer only, and the watch officer's own egress policy |
| `hermes/` (kustomize) | Also **memory-auditor** (`memory-auditor.yaml`, `memory-auditor.py`): snapshots GIDEON's memory, cron job definitions, config drift and an inventory of unexpected files into git on its own volume every 5 min; alerts `GideonControlStateChanged` (warning), `GideonMemoryChanged` (info), `MemoryAuditorStale`, `MemoryAuditorErrors`. Tested locally against the harness volume (2026-11-23); unit tests: `python3 -I deploy/familiar/hermes/memory_auditor_test.py` |
| `hermes/` (kustomize) | Hermes, the watch officer: Deployment, PVC, ServiceAccount (no token), ConfigMaps generated from `config.yaml`, `SOUL.md` and `scripts/alert-gate.py`. Settings from `tests/hermes-local` (uid 10000, read-only root, read-only persistence points). Server-side dry run passes in a restricted namespace (2026-11-22). Matrix IDs: `@gideon` (bot), `@jase` (only allowed user). Liveness alerts in `alerts.yaml` (warning): `HermesDown` (no available replica for 10 min, or the Deployment is gone), `HermesRestarting` (more than 2 restarts an hour), `HermesIdle` (no model call from watch-officer in 5 hours, so the 4-hourly sweep has stopped) |

Not here yet: the Kata RuntimeClass (A6; `runtimeClassName` is commented out in `hermes/deployment.yaml`).

## What each policy allows

| From | To | Port |
|---|---|---|
| watch officer (`familiar-agents`, label `familiar/role: watch-officer`) | model-gate | 8081 |
| watch officer | tool servers | 8080 |
| watch officer | kube-dns, **only** six names: model-gate, the four tool servers and `ess-synapse.matrix` (the `watch-officer-egress` CNP) | 53 |
| watch officer | ESS HAProxy (`matrix`, in front of Synapse) | 8008 |
| model-gate | `api.anthropic.com` (DNS + FQDN) | 443 |
| cluster-read | kube-apiserver | 6443 |
| metrics-query, ceph-health | Prometheus pods (never Alertmanager) | 9090 |
| cert-probe | Traefik pods | 8443 (Service 443) |
| Prometheus | model-gate metrics | 9090 |
| Prometheus | memory-auditor metrics | 9100 |
| memory-auditor | nothing (no egress) | |

Everything else is denied. Names are fully qualified (`*.svc.cluster.local`), and every pod under a strict-DNS policy uses `ndots: 1` (lessons from A5).

## Apply order (when Phase 1 starts)

1. Images are built and pushed (multi-arch amd64/arm64, pinned by digest in the files): `ghcr.io/runlevel-six/model-gate:0.2.0-86b0d42` and `ghcr.io/runlevel-six/familiar-tools:0.2.1-f0562ac` (2026-11-23, with the review fixes and the public/private split). They must be **public** on GHCR (or add a pull secret). To rebuild: `podman build --platform linux/amd64,linux/arm64 --manifest <ref> <dir>`, then `podman manifest push --all`, and update the digests here.
2. Create the key Secret out of band: `kubectl -n familiar-system create secret generic model-gate-keys --from-file=watch-officer=…`
3. After ESS is running and the bot account and its token exist (D21): create the `hermes-matrix` Secret in `familiar-agents` (`--from-literal`, no trailing newline). The namespaces come from `00-namespaces.yaml`; create them first if needed (`kubectl apply -f 00-namespaces.yaml`).
4. Apply through the site overlay, never the base on its own: the base holds placeholders for the cert-probe host list and the Matrix IDs. Copy `../overlays/example/` (this repo keeps its real one in `local/deploy/familiar/`), then `kubectl apply -k <overlay>`.
4b. Recreate the Hermes cron jobs (`kubectl -n familiar-agents exec deploy/hermes -- hermes cron create …`, see `tests/hermes-local/README.md`) with `--no-wrap-response`.
5. **Verify**, not just apply:
   - Every CNP is `Valid=True` (`kubectl get cnp -A`); `CiliumPolicyInvalid` stays quiet.
   - `kubectl auth can-i --as=system:serviceaccount:familiar-tools:cluster-read` gives **no** for `get secrets`, `get configmaps`, `get pods/log`, `create pods`, and **yes** for `list pods`.
   - From a throwaway pod labelled as the watch officer: it reaches model-gate:8081 and the tool servers, and **cannot** reach `api.anthropic.com`, Alertmanager, Prometheus or any other name (the `deploy/tests/a5-fqdn` method).
   - model-gate reaches `api.anthropic.com` and nothing else.
   - Its ServiceMonitor target is up.
   - **memory-auditor sees what Hermes writes** (it reads the same Ceph RBD volume from a second pod, read-only; only tested on a Podman volume so far). Its log shows `baseline recorded`. Then plant a canary through Hermes' own mount: `kubectl -n familiar-agents exec deploy/hermes -- sh -c 'echo canary > /opt/data/home/auditor-canary'`. Within one interval (5 min), `kubectl -n familiar-agents exec deploy/memory-auditor -- git -C /audit/repo log -1 --stat` lists `files/inventory.tsv` and `GideonControlStateChanged` fires. Remove the file (`exec deploy/hermes -- rm /opt/data/home/auditor-canary`) and check the next commit drops it. End to end: ask GIDEON in Matrix to remember something, approve it, and check a `memories` commit appears (`GideonMemoryChanged`).
   - **Repeat the memory-auditor check after Hermes moves to Kata** (`runtimeClassName: kata`, A6). Hermes then sees the volume through the VM while the auditor reads the host mount; see `local/docs/runbooks/reboot-window.md` §5.4b first.

## memory-auditor: review and restore

Review what changed: `kubectl -n familiar-agents exec deploy/memory-auditor -- git -C /audit/repo log -p -5`
(memories only: `... log -p -- memories`). The repo holds normalised copies, not the raw files.

Restore GIDEON's memory to a known-good state (the design's reset procedure):

1. Find the commit: `... git -C /audit/repo log --oneline -- memories`.
2. Stop Hermes: `kubectl -n familiar-agents scale deploy/hermes --replicas=0`.
3. Copy the files back. The auditor mounts the volume read-only, so use a one-off pod that mounts `hermes-data`
   read-write as uid 10000 and receives the files from `git show <commit>:memories/MEMORY.md` (and `USER.md`) via
   `kubectl exec -i ... -- sh -c 'cat > /opt/data/memories/MEMORY.md'`.
4. Cron jobs: delete or recreate them with `hermes cron` (see `tests/hermes-local/README.md`); `cron/jobs.json`
   in the repo shows the definitions to compare against.
5. Start Hermes again: `--replicas=1`.
