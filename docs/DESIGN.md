# familiar — design document

> **Working title:** `familiar` (a wizard's always-present companion that watches and reports; rename freely)
> **Status:** Draft v0.2. Phase 0 done (2026-11-23); Phase 1 deployed 2026-11-23, in its two-week run against the exit criteria
> **Owner:** Jase
> **Implementer:** Claude Code (phase by phase; each phase is a separate hand-off)
> **Last updated:** 2026-11-23

---

## 1. Summary

`familiar` is a pilot of an always-on AI agent running in my single-node home Kubernetes/Ceph cluster. It does two things at once:

1. **Useful work:** helps keep the home cluster healthy (triage, diagnosis, proposed fixes), and eventually builds the self-hosted apps I design, instead of me running coding agents on my laptop.
2. **Deliberate learning:** practices the operational and security patterns for long-running agents (persistent memory, standing credentials, untrusted input, no human present at the moment of action) before I deploy an admin assistant that reads email for a small business.

Both goals count equally. **The phases are ordered by risk, not by priority.** Each phase widens one dimension (write access, untrusted input, or code execution) and only after the previous phase's exit criteria are met.

## 2. Goals and non-goals

### Goals

- G1. Run a persistent agent (Hermes Agent + Claude via the API) in the home cluster with structural guardrails, not just cooperative ones.
- G2. The agent holds **no real credentials**: no model API key, no Kubernetes ServiceAccount token, no forge token. Every privileged action goes through a broker that holds the credential and enforces policy.
- G3. Every agent action, model call and memory change is auditable after the fact.
- G4. Have a kill switch for the whole system that I can trigger in one command.
- G5. Rebuild, in cluster form, the agent-sandboxes protections the builder phase needs (§9).
- G6. Produce evidence for that email assistant's design: what worked, what leaked, what the agent tried to do.

### Non-goals

- Local model inference (deferred). All phases use hosted models only. Claude via the API is the default throughout; the one planned exception is Spike S2, which switches one role to another *hosted* provider for about a week.
- Any work or org systems. This is personal infrastructure only.
- Replacing deterministic monitoring. **Prometheus/Alertmanager rules stay the primary way problems are detected** (D5, revised by D17: no external delivery; alerts are read from Alertmanager and by the laptop dead-man's switch). The agent adds triage and diagnosis; it is never load-bearing for finding out that something is broken.
- Multi-node HA. The cluster is single-node, and the pilot accepts that.
- Adopting Agent Substrate (or a similar runtime) as the foundation. It's evaluated in a separate spike (§10).

## 3. Threat model

Assume the agent can become adversarial at any time, through prompt injection, memory poisoning, a malicious skill or plugin, or model misbehaviour. The design must hold even if the agent actively tries to:

| # | Threat | Primary control | Secondary control |
|---|---|---|---|
| T1 | Exfiltrate the model API key | Key exists only in `model-gate`; never in agent env | Egress deny; workspace spend cap |
| T2 | Exfiltrate cluster data | Tools return redacted summaries; egress allowlist | Hubble flow audit; model-gate rejects server-side tools that fetch from Anthropic's side |
| T3 | Escalate in the cluster (create pods, read Secrets) | Agent pod has no SA token; tool servers hold narrow roles | RBAC excludes policy objects; cluster-read hides familiar's own namespaces (`familiar-*`), enforced in the tool server because RBAC can't subtract namespaces from a ClusterRole (2026-11-23). Relaxed 2026-11-26 for one thing: metrics-query's `self_usage` reports the agent's own pod's CPU, memory and restarts, summed so no familiar label comes back |
| T4 | Disable or modify its own guardrails | No write path to `familiar-*` namespaces, RBAC, CNPs | GitOps: guardrails live in a repo the agent can't merge to |
| T5 | Persist an injection via memory | Memory snapshotted to git by a sidecar the agent doesn't control; diffs reviewed | Memory reset procedure |
| T6 | Runaway spend / loop | Per-run and per-day token budgets in `model-gate` | Monthly workspace spend limit in Console |
| T7 | Starve the node (OOM, I/O) | ResourceQuota, LimitRange, low PriorityClass | External dead-man's switch |
| T8 | Supply-chain compromise of agent or broker | Pin images by digest; minimal broker deps; vendor skills | Egress deny limits callback |
| T9 | Untrusted input (logs, feeds, messages) steers privileged actions | Phase 3 quarantined reader with no tools; structured output only | Messaging gateway restricted to my user ID |
| T10 | The agent hides activity | Tool servers log every call independently of the agent | Falco (optional, later) |

**Design rule (lethal trifecta):** no single agent instance holds all three of (a) private data, (b) untrusted input, and (c) an external communication channel. Each phase records which of the three it grants.

## 4. Architecture

```
                    ┌─────────────────────────── off-cluster ───────────────────────────┐
                    │  api.anthropic.com      messaging API (me only)   laptop dead-man  │
                    └────────▲───────────────────────▲──────────────────────────▲────────┘
                             │ (toFQDNs)             │ (toFQDNs)                │
┌──────────────── familiar-system ─────────────────┐ │                          │
│  model-gate  (holds API key, budgets, audit log)  │ │           (polls Alertmanager)
│  memory-auditor (snapshots agent memory → git)    │ │
└────────▲──────────────────────────────────────────┘ │
         │ HTTP (in-cluster only)                     │
┌──────── familiar-agents (RuntimeClass: kata) ──────┴──┐
│  hermes (watch officer)  — no SA token, no secrets     │
│  [Phase 3] hermes-reader (quarantined, no tools)       │
└────────┬──────────────────────────────────────────────┘
         │ MCP/HTTP
┌──────── familiar-tools ──────────────────────────────┐
│  cluster-read   (narrow ClusterRole, redacts output)  │
│  metrics-query  (PromQL, allowlisted)                 │
│  cert-probe     (TLS probe of ingress hosts)          │
│  [Phase 2] propose-change (forge token, path allow)   │
└──────────────────────────────────────────────────────┘
```

*This diagram predates D19: messaging is now an in-cluster Matrix homeserver with federation off, and the agent has no internet destination. The current picture is in [How familiar is built](explanation/architecture.md).*

### 4.1 Components

**hermes (watch officer).** Hermes Agent, official image pinned by digest, single replica, persistent volume for `/opt/data`. Uses Hermes heartbeat/scheduling for periodic checks, plus the messaging gateway so I can talk to it. Runs under the Kata RuntimeClass (gVisor as fallback, see §5). `automountServiceAccountToken: false`. Restricted Pod Security Standard, non-root, read-only root filesystem except the data volume. Hermes' **Docker terminal backend is disabled**: there is no container socket in the pod, and the pod itself is the sandbox.

**model-gate.** A small, purpose-built reverse proxy (Go, standard library `httputil.ReverseProxy`, minimal dependencies). Responsibilities:
- Accepts unauthenticated requests from `familiar-agents` only (enforced by CNP), injects `x-api-key`, forwards to `api.anthropic.com`.
- Separate API key per agent role, selected by source identity (pod label via a header that the CNP guarantees, or a separate listener port per role).
- Parses `usage` from responses (including streamed SSE `message_delta` events) and enforces per-request, per-hour and per-day token budgets. Over budget → HTTP 429 and a notification.
- Writes a structured audit log line per request: role, model, input/output tokens, latency, status. No prompt bodies by default; there's a debug flag to capture them to a local volume.
- Kill switch: `kubectl scale deploy/model-gate --replicas=0`.
- **Blocks Anthropic's server-side tools** (web search, web fetch, code execution, MCP connector/toolset, `container`) by default: they make Anthropic reach the network on the agent's behalf, bypassing the cluster egress policy (T2, T9). Found while writing model-gate, 2026-11-22.
- **Status (2026-11-22):** implemented and tested locally in `model-gate/` (Go stdlib only; unit tests plus an end-to-end run against a fake API). Budgets are in USD from a per-model price table, so cached reads aren't over-counted. Not yet deployed (Phase 1).

*Why not LiteLLM or another off-the-shelf gateway:* this component holds the most valuable credential in the system, and LiteLLM's release pipeline was compromised twice in 2026 (March and October). A few hundred lines of auditable Go with no third-party dependencies is the conservative choice. Revisit if model-agnostic routing becomes a hard requirement. (Also evaluate: Envoy with a credential-injection filter; budget enforcement is the hard part there.)

**Tool servers.** Each is a separate Deployment with its own ServiceAccount, exposing tools over MCP (or HTTP, depending on what Hermes supports; see open questions). Tool servers **hold the credentials, enforce the policy, log every call**, and return summarised, redacted output.

| Tool | Credential | Returns | Explicitly not |
|---|---|---|---|
| `cluster-read` | Custom ClusterRole: get/list on pods, deployments, statefulsets, daemonsets, jobs, cronjobs, events, nodes, PVCs, PVs, storageclasses, ingresses, cert-manager Certificates (no Traefik IngressRoutes exist on the cluster, checked 2026-11-23) | Status summaries, conditions, recent events, restart counts | Secrets, ConfigMap data, pod env, raw `pods/log` |
| `log-summary` (optional) | `pods/log` on allowlisted namespaces | Error-pattern counts, last N lines **after** redaction (IPs, tokens, emails) | Raw log streams; logs from Authelia |
| `metrics-query` | Prometheus read | PromQL results; allowlisted query templates + rate limit | Arbitrary PromQL at first |
| `cert-probe` | None (network only) | Served cert subject/issuer/notAfter per ingress host vs. cert-manager's view | — |
| `ceph-health` | None (reads Ceph exporter metrics via Prometheus) | Health status, OSD fullness, PG states | Ceph admin keyring / Rook toolbox |
| `host-read` | List pods and PersistentVolumes; host PID namespace as non-root, no capabilities (2026-11-26) | The node's processes: CPU, memory, state, blocked-on-I/O, grouped by pod or systemd unit; disk I/O per Ceph volume by claim and pod; familiar's own pods and volumes hidden | Command lines, environments, open files, per-process I/O, root, hostPath |

**Tool server status (2026-11-22):** the four read-only tool servers (`cluster-read`, `metrics-query`, `ceph-health`, `cert-probe`) are implemented in `tools/` as MCP servers speaking both the current spec (2026-07-28, stateless, per-request `_meta`) and the older `initialize` handshake, so either Hermes era works. Tested with unit tests, live read-only runs against the cluster, and an MCP-over-HTTP run. Phase 1 manifests for them and for model-gate are drafted in `deploy/familiar/` (server-side dry-run validated, restricted-PSS compliant, not applied). `log-summary` isn't built (Tier B).

**memory-auditor.** A sidecar or CronJob (not controlled by the agent) that copies the Hermes state that can change its behaviour from the data volume (`memories/`, `skills/`, `SOUL.md`, `hooks/`, `cron/`, `config.yaml`; also `sessions/` and `state.db` at lower priority, since they are large and change constantly; never `.env`) into a git repo the agent cannot write to, commits on change, and notifies me with a diff summary. This is detection, not prevention: I review diffs, and the reset procedure is "restore `/opt/data` memory from a known-good commit". **Implemented 2026-11-23** (`deploy/familiar/hermes/memory-auditor.*`, not applied): a separate pod in `familiar-agents` (it must mount the Hermes PVC, and PVCs can't cross namespaces), uid 10000 (Hermes' files are 600), read-only mount, no egress, git on its own PVC. Since `SOUL.md`, `hooks/`, `plugins/`, `scripts/` and `skills/` are now read-only mounts from git, it records `memories/`, cron job definitions (run state stripped), `config.yaml` drift from the ConfigMap, and an inventory of unexpected files; secrets and `sessions/`/`state.db` are excluded. Alerts: control-state changes are warnings (reach the dead-man's switch), memory changes are info (weekly review). Tested locally: a job running made no commit; a memory write, a prompt edit and a planted `home/.persist.sh` were each recorded. Skip lists use exact names taken from a real Hermes volume (2026-11-23, after an external review found prefix matches such as `gateway` also hid `gateway-hook.sh`). Inside noisy directories, executables and symlinks are still recorded, and so are the PVC's own copies of the read-only mount points.

**Dead-man's switch (revised 2026-11-22, D17).** No cloud service: a small checker on Jase's always-on laptop (later the Raspberry Pi) polls Alertmanager's API on the LAN every few minutes through the API server, with a read-only token. If the API can't be reached or the always-firing `Watchdog` alert is missing, it notifies locally. It also reports newly firing alerts. Hermes liveness is an alert rule (`deploy/familiar/hermes/alerts.yaml`: Deployment down or gone, repeated restarts, no model call in 5 hours), so it reaches the laptop the same way. If the node dies, the agent can't report it, but the laptop will.

### 4.2 Namespaces and RBAC boundaries

- `familiar-system`: model-gate. (memory-auditor moved to `familiar-agents`, see §4.1.) Agent has no network or API access here except model-gate's port.
- `familiar-agents`: Hermes instances. No ServiceAccount tokens mounted.
- `familiar-tools`: tool servers.
- `familiar-host`: `host-read` only, because it needs the host's PID namespace (Pod Security `privileged`, warn `restricted`; the pod itself is non-root with no capabilities). Added 2026-11-26.
- **No familiar component can modify** `familiar-*` namespaces, RBAC objects, CiliumNetworkPolicies, RuntimeClasses, Authelia config, Rook/Ceph CRDs, backup config, storage and resources (§5.6), or the `monitoring` namespace and its rules (A4). The backup storage is also unreachable from familiar pods (no hostPath mounts; CNP-denied), because it contains every Secret. All guardrail config lives in a GitOps repo that only I can merge.

### 4.3 Network policy (Cilium)

Default deny ingress and egress in all three namespaces, then:
**Policy-writing rules (learned in A5, 2026-11-22):** (1) DNS rules list exact names with `matchName`, never `matchPattern: "*"` (DNS would stay open for exfiltration); (2) pods under such policies set `dnsConfig.options ndots: "1"` and use fully qualified in-cluster names (`svc.cluster.local`), each listed in the DNS rule; (3) after applying, check the policy's `Valid` condition is `True`, because an invalid policy enforces nothing; (4) after any Cilium config change, restart `cilium-operator` too, or policy status goes stale; (5) re-run the A5 test after every Cilium upgrade.

- `familiar-agents` → model-gate :port, tool servers, kube-dns, and the in-cluster Matrix homeserver (Synapse, through the ESS HAProxy pods in `matrix`; D19, D21). No internet destination.
- `familiar-system/model-gate` → `api.anthropic.com` (toFQDNs) only.
- `familiar-tools/*` → kube-apiserver (cluster-read only), Prometheus, ingress hosts (cert-probe only).
- **No familiar namespace may reach Alertmanager** (its API is unauthenticated and can create silences, which would let the agent hide alerts: T4, D5). Only `metrics-query` and `ceph-health` reach Prometheus, which runs with the admin API disabled.
- (No heartbeat egress: the dead-man's switch pulls from the LAN, D17.)

Hubble flow logs for all three namespaces are retained for audit.

### 4.4 Resource envelope (single node)

All `familiar-*` workloads run at a PriorityClass **lower** than every app on the node, so the kubelet evicts them first under pressure. ResourceQuota per namespace. Kata VM sizing must be measured in Phase 0: Kata adds per-pod VM memory overhead, and Ceph OSDs need their own memory headroom.

## 5. Phase 0 — Cluster discovery and prerequisites

**Purpose:** find out what the cluster actually has, decide what to install, and install it safely. **No agent runs in this phase.** Claude Code can do discovery with a **read-only** kubeconfig. Installs are changes I review and apply.

### 5.0 Scope tiers (revised 2026-11-19, after discovery)

Discovery (`local/docs/cluster-inventory.md`) showed the cluster lacks monitoring, GitOps, a sandboxed runtime and verified backups, so Phase 0 is split. **Tier A** must be finished before any agent runs. **Tier B** is deferred but tracked.

**Tier A: must-have**

| # | Item | Why | Done when |
|---|---|---|---|
| A1 | Hermes capability checks (Q4–Q7) | Could change the architecture; no cluster impact; no dependencies | **Done 2026-11-19**: Q4–Q7 answered in §12 (Q7 partly); follow-ups below |
| A2 | Backups: etcd snapshot plus PVC/Ceph data to the NVMe OS disk (automated), copied to the 1 TB external SSD before each node-level window (manual) | Gates every node-level change. Ceph replicas sit on one host, so Ceph is not a backup. See §5.6. | **Mostly done 2026-11-21:** nightly backups running; etcd and PVC restores tested; size measured (47 GiB) and capped (300 GiB); SSD procedure written (`local/docs/runbooks/backups.md`). **Done 2026-11-22:** first SSD sync done and verified; one-off dumps of the two databases without dump jobs taken and on the SSD |
| A3 | Infrastructure maintenance: containerd, kubelet, Cilium, OS packages | The stack was behind; the containerd version affects the Kata install path. | **Done 2026-11-22.** Upgraded outside this project on 2026-11-20 (Kubernetes v1.37.2, containerd 2.4.2, Cilium 1.20.5); host rechecked and inventory updated. OS packages not applied (57 pending, none security; no reboots, D14). **Found a cgroup driver mismatch** (containerd `SystemdCgroup = false`, kubelet `systemd`), folded into D15 |
| A4 | Prometheus + Alertmanager (node, Ceph exporter, kube-state, Cilium/Hubble metrics) | Primary detection (D5/D17); backend for `metrics-query` and `ceph-health` | **Done 2026-11-22** (`local/deploy/monitoring/`): all scrape targets up; `Watchdog` firing; our rules for kubelet restarts, backup freshness and Ceph; Ceph `TOO_FEW_OSDS` fixed (HEALTH_OK). Alerts reaching Jase is covered by A8 |
| A5 | FQDN egress works: test whether `toFQDNs` is enforced with `enable-l7-proxy=false`; enable the L7 proxy if not | Core Phase 1 containment (T1, T2) | **Done 2026-11-22** (`deploy/tests/a5-fqdn/`): with the L7 proxy off the policy was rejected and **nothing was enforced (fails open)**. L7 proxy enabled (Cilium Helm revision 12, adds `cilium-envoy`); a test pod now reaches only `api.anthropic.com:443`, while other names, raw IPs and other ports are blocked. Rules for writing policies: §4.3 |
| A6 | Kata RuntimeClass | Phase 1 pod isolation | **Done 2026-11-23** (`deploy/kata/`, `deploy/tests/a6-kata/`): kata-deploy chart 4.3.1 (4.4.0 was under two weeks old), every image pinned by digest; RuntimeClasses `kata` and `kata-qemu-runtime-rs`; containerd got a drop-in only, `config.toml` untouched, no app pod restarted. A restricted-PSS test pod runs in a VM (guest kernel 6.18). Found: Kata pods couldn't use Service ClusterIPs until Cilium's socket LB was limited to the host namespace (D25). The FQDN test passes under Kata. A Kata pod writing a Ceph volume shares it over virtio-fs, and a runc pod reading it read-only sees the writes (the memory-auditor setup). VM memory overhead measured at about 166 Mi, steady over 30 minutes (QEMU, virtiofsd and the shim, for a 128Mi pod), so the chart's 320Mi/250m pod overhead is kept. The guest's RAM is the pod's memory limit (static sizing), so limits need headroom for the guest kernel |
| A7 | Git repo for familiar manifests, applied manually with `kubectl apply` at first | Guardrails must live where the agent can't merge (T4) | **Done 2026-11-22:** this repo (`runlevel-six/familiar`, `deploy/`). Public since 2026-11-23, only Jase can push; `main` is protected by a ruleset (no deletion or force push, pull request required, Jase's account bypasses). `monitoring` is applied from its private `local/` side; `familiar-*` will be in Phase 1. No familiar component ever gets a token for it |
| A8 | Dead-man's switch on a LAN machine (laptop now, Raspberry Pi later); no cloud service (D17) | Detects the whole node or monitoring being down (§4.1) | **Done 2026-11-22** (`local/ops/deadman/`): launchd agent on Jase's Mac polls Alertmanager every 5 min with a read-only token; notifies on unreachable, missing Watchdog, recovery and new warning/critical alerts. All five tests passed |

Order: A1, then A2 → A3, then A4 → A5 → A6. A7 and A8 can run in parallel. A3, A5 and A6 touch the node or the CNI, so they come after backups.

**A1 follow-ups** (found while answering Q4–Q7; to be done in Phase 1 setup or earlier):
- [x] Verify against a running Hermes that **all** model calls, including auxiliary ones, go through model-gate. **Done 2026-11-22** (local test, `tests/hermes-local/`): every call went through the gate, none elsewhere; auxiliary tasks set to `provider: main` + Haiku. No placeholder key needed. **Corrected 2026-11-26:** with `main`, the gateway's runtime sends side tasks to `/chat/completions`, which the gate refuses (titles failed on 2026-11-24; the local test never passed that runtime). They now name `custom:model-gate`.
- [x] Confirm the non-interactive hard stop. **Done 2026-11-22:** `approvals.cron_mode`, `single_query_mode` and `unattended_mode` = `deny` are it; with the terminal, file and code-execution toolsets disabled there is nothing left for them to stop.
- [x] Remove `skill_manage`. **Done 2026-11-22:** `agent.disabled_toolsets: [skills]` removes it (with `skill_view`/`skills_list`); `skills/` is also mounted read-only.
- [x] Read-only persistence points. **Done 2026-11-22:** `SOUL.md`, `hooks/`, `plugins/`, `scripts/` and `skills/` mounted read-only; Hermes runs normally. `cron/` can't be (the scheduler writes `jobs.json` every run); the `cronjob` toolset is disabled instead, and memory-auditor should watch `cron/jobs.json`.

**Tier B: deferred, tracked**
- Argo CD or Flux (revisit when Phase 2 needs automated PRs/merges)
- Falco
- `log-summary` tool and anything that feeds raw logs to the agent (only after the Phase 1 red-team test)
- Phase 3 and spikes S1/S2
- Pod Security Admission for non-familiar namespaces (only the `familiar-*` ones are needed now)
- [x] ~~A real read-only kubeconfig for discovery and Claude Code use~~ **Done 2026-11-22** (`deploy/access/readonly.yaml`): ServiceAccount `cluster-readonly/viewer` with `view` plus explicit extras; no Secrets, `nodes/proxy`, exec or port-forward; service proxy only for GET to Prometheus. Kubeconfig on Jase's Mac; the admin kubeconfig is now for approved changes only
- Velero + Garage as the future backup state (§5.6), when Ceph is expanded, nodes are added, or Phase 2 starts
- Automated off-node backups (e.g. cloud object storage). Until then, the external SSD copy is manual and only as recent as the last window (§5.6)

### 5.1 Discovery (produce `local/docs/cluster-inventory.md`)

*Status: collected 2026-11-19, with later answers from A2–A5 and the reboot runbook (2026-11-22/23); see `local/docs/cluster-inventory.md`. Still open: secrets handling outside the cluster (SOPS?) and the root crontab (old etcd backup arrangements).*

Cluster-level (read-only kubeconfig):
- [x] Kubernetes distribution and version (k3s? kubeadm? other) and how it was installed
- [x] CNI: is it Cilium? Version? Are L7/FQDN policies (`toFQDNs`) enabled? Is Hubble installed?
- [x] Container runtime and version (containerd?), existing RuntimeClasses
- [x] Storage: StorageClasses, Ceph pools/replication, free capacity; node-local ephemeral storage capacity and which disk backs it
- [x] Ingress controller (Traefik) and version; cert-manager present?
- [x] Monitoring: Prometheus/Alertmanager present? Ceph exporter? node-exporter?
- [x] GitOps: Argo CD, Flux, or manual apply? Which forge hosts the home manifests (GitHub `runlevel-six`, a home Gitea, other)?
- [ ] Secrets handling today: SOPS, sealed-secrets, plain Secrets?
- [x] Backups: what backs up the k8s datastore and Ceph data today? Velero?
- [x] Pod Security admission configuration (none: no admission config file, only `NodeRestriction`; reboot runbook §0, 2026-11-22)
- [x] Current node CPU/memory/ephemeral-storage requests vs. allocatable (headroom)

Host-level (needs node shell; I run these, or Claude Code with explicit, separate approval):
- [x] Is the node baremetal or a VM? (Kata needs KVM; nested virt adds complications)
- [x] CPU vendor and virtualisation flags (`svm`/`vmx`), `/dev/kvm` present
- [x] Kernel version
- [x] containerd config path and whether it's managed by the distro (k3s templates its own containerd config)
- [x] Physical RAM, cores, disks, and which disks back Ceph OSDs vs. ephemeral storage

### 5.2 Prerequisite matrix

Fill in from discovery. "Required by" = earliest phase that needs it.

| Prerequisite | Required by | Status | Notes |
|---|---|---|---|
| Cilium with `toFQDNs` egress policy | Phase 1 | **Ready (A5, 2026-11-22):** Cilium 1.20.5 with the L7 proxy enabled; FQDN allowlisting tested | Fallback (egress HTTP proxy) no longer needed. |
| Sandboxed RuntimeClass (Kata preferred, gVisor fallback) | Phase 1 | **Ready (A6, 2026-11-23):** Kata 4.3.1, `qemu-runtime-rs`, RuntimeClass `kata`; Cilium socket LB limited to the host namespace so VM pods can use Services (D25) | Kata needs KVM. kata-deploy modifies containerd config and restarts containerd, which on a single node briefly affects every pod. gVisor (`runsc`) is lighter to install and needs no KVM. Decide using §5.3. |
| Prometheus + Alertmanager | Phase 1 | **Done (A4, 2026-11-22):** kube-prometheus-stack, all targets up (`local/deploy/monitoring/`) | Primary alert path, and the `metrics-query` tool backend |
| Ceph exporter metrics in Prometheus | Phase 1 | **Done (A4, 2026-11-22):** scraped | So `ceph-health` never needs a Ceph keyring |
| cert-manager | Phase 1 (cert-probe compare) | Present | Optional; cert-probe still works without it |
| GitOps controller + repo for familiar manifests | Phase 1 | **Repo done (A7, 2026-11-22):** this repo, applied manually; controller deferred to Tier B | Guardrails must be managed in git. Phase 2 also needs it for PRs. |
| Dead-man's switch on a LAN machine | Phase 1 | **Done (A8)** | Laptop now, Raspberry Pi later; no cloud service (D17) |
| Backups of k8s datastore + Ceph | **Before any node-level install** | **Done (A2, 2026-11-21/22):** nightly etcd snapshots and Kopia PVC backups on the NVMe, restores tested, external SSD copy before each window (§5.6) | Non-negotiable on a single node. `/storage` is a dying disk and out of scope. Plan: NVMe for frequent local backups plus the external SSD for an off-node copy before each window (§5.6). |
| Hubble | Phase 1 (nice to have) | Present (relay, UI, metrics) | Flow audit |
| Falco | Later | Not present (Tier B) | Ties into my Kata/Falco visibility work |
| Node specs documented | Phase 4 | Collected: 6-core/12-thread desktop CPU, 32 GB, 1 TB NVMe OS disk, 2×12.7 TB HDD OSDs | Builder sizing |

### 5.3 Kata vs. gVisor decision (Phase 0 output)

**Decision (2026-11-19): Kata.** The node is bare metal (Intel, VT-x) and `/dev/kvm` is present, so the KVM requirement is met. The remaining costs are the containerd restart and per-pod VM memory overhead, which must be measured against real CPU use (see exit criteria). The install path depends on the containerd version chosen in A3.

Reasoning kept for the record:
- **Kata (preferred):** hardware boundary, matches the agent-sandboxes v3 direction, and Falco/Kata work carries over. Costs: KVM required, per-pod VM memory overhead, containerd restart during install.
- **gVisor (fallback):** no KVM needed, lighter install, lower overhead. Weaker boundary for truly untrusted code (relevant to Phase 4 more than Phase 1).
- Either way, the Phase 1 agent pod holds no credentials, so the runtime boundary is defense in depth for Phase 1 and becomes load-bearing in Phase 4.

### 5.4 Install procedure (for each node-level change)

0. **Constraint (2026-11-22, until Jase lifts it): no node reboots, and nothing may touch `/storage`.** A separate data recovery from the dying `/storage` disk will take weeks, and every reboot makes the disk worse. Changes must work with a kubelet or pod restart at most, or wait. Restarting pods and the kubelet is fine: no pod or PV uses `/storage` (checked through the API).
1. Confirm backups are current and a restore has been tested at least once.
2. Write the change as a GitOps PR (or a documented manual procedure if it's host-level).
3. Schedule a window; expect all pods to blip if containerd restarts.
4. Apply, then verify: run a test pod under the new RuntimeClass, confirm a `toFQDNs` policy blocks a non-allowlisted domain.
5. Record what changed in `local/docs/cluster-inventory.md`.

### 5.5 Phase 0 exit criteria

- [x] Inventory complete; prerequisite matrix filled in (2026-11-19)
- [x] Hermes capability checks done (§12, Q4–Q7) (A1), 2026-11-19; follow-ups listed in §5.0
- [x] Backups: etcd and one PVC restore **tested** from the NVMe copy (done 2026-11-21); external SSD copy made (A2, §5.6; 2026-11-22)
- [x] Infrastructure maintenance (containerd, kubelet, Cilium) done; cluster healthy; inventory updated (A3, 2026-11-22; OS packages deferred by D14)
- [x] Prometheus + Alertmanager running, all targets up, Ceph HEALTH_OK (A4, 2026-11-22)
- [x] FQDN egress policy demonstrably blocks a non-allowlisted domain from a test pod (A5, 2026-11-22)
- [x] Sandboxed RuntimeClass works; test pod runs under it (A6, 2026-11-23)
- [x] Manifest repo exists, only I can merge (A7, 2026-11-22)
- [x] Laptop dead-man's switch tested (A8, 2026-11-22)
- [x] ~~Measured CPU baseline over a week~~ **Dropped as an exit criterion (Jase, 2026-11-23, D23):** no week is "normal" right now (new deployments, the `/storage` recovery jobs, tests), so a week of data wouldn't be representative either. Conclusions from the first 23.5 h (2026-11-22 01:32 → 2026-11-23 01:05 UTC), and Prometheus keeps collecting for Jase and GIDEON (`metric_history` reaches back 168 h):
  - **CPU is not a constraint.** Real CPU work (excluding iowait) averaged **0.53 cores, p95 0.65, max 1.0** of 12. Counting iowait as "used" (the originally planned query) inflates the max to 4.6.
  - **Requests are the scheduling constraint, and they're inflated.** 8.66 of 12 cores requested (72%) against ~0.5 used (4.03 of 12 after D16, 2026-11-23). **Rook-Ceph requests 5.95 cores and peaked at 0.17** (OSDs 1 core each, operator 200m, mgr 500m, log collectors). D16 deferred lowering these "until monitoring can watch the effect": it now can. Do it through the CephCluster spec (never in-place resize), one OSD restart at a time, with HEALTH_OK checked between.
  - **Disk I/O is the real contention.** iowait spikes match known jobs: the 04:15 Kopia run (up to 5.8 cores of iowait), the apps' 03:00–03:50 backup and dump jobs, SSD syncs (`sdc` 94% busy) and the `/storage` recovery (`sdb` 99% busy). Peak disk busy on the Ceph HDDs: `sda` 76%, `sdd` 47%; NVMe 11%. Keep heavy jobs staggered, and Phase 4 build scratch on the NVMe (already planned).
  - **Memory:** 12.4 GiB in use on average, 14.1 max, of 31. Phase 1 fits easily: Hermes 100m / 512 Mi, Kata overhead 250m / 320 Mi (the chart default; measured about 166 Mi actual, A6), memory-auditor and tool servers ~10m each; the `familiar-agents` quota (1 CPU, 3 GiB requests) holds.
- [x] Open questions in §12 marked as answered or deferred (Q1–Q10 all answered by 2026-11-23)

### 5.6 Backup plan (A2, revised 2026-11-21)

`/storage` turned out to be a dying disk and is out of scope. No other disk is available, so backups use two tiers:

| Tier | Target | Protects against | How |
|---|---|---|---|
| Local | NVMe OS disk (764 GB free) | A bad upgrade, a broken cluster or CNI, a corrupted or deleted PVC (the main A3/A5/A6 risks) | Automated, scheduled etcd snapshots and PVC backups; keep a fixed number of generations; total size capped |
| Off-node | 1 TB external SSD | Losing the node or its disks | Before every node-level window: plug in, sync the local tier, verify, unplug. Being offline most of the time also protects it from ransomware or a compromised agent |

Measured size (2026-11-21, `ceph df` / `rbd du`): the pool stores **255 GiB** (509 GiB raw at size 2) across 27 RBD images. **a media library PVC is 163 GiB of that**; everything else is about **102 GiB**. The largest are a sync folder 36 GiB, a photo library 31 GiB, an app's backups 13 GiB; the rest are under 3 GiB each. `rbd du` counts allocated blocks that were never discarded, so it overstates real use: the PXC image shows 7.8 GiB, but its filesystem holds 393 MiB. File-level backups will be smaller. **The media library is excluded** (replaceable, decided 2026-11-21), so the backup set is about **100 GiB**.

Constraints:
- The NVMe is also the Phase 4 build scratch space. Cap backups so they leave at least ~400 GB for builds and ephemeral storage.
- PVCs with app-level backups (four apps have their own dump jobs) can rely on those if they write to the local tier. Otherwise include them.
- Known gap: the off-node copy is only as recent as the last window. Automated off-node backups are Tier B.

Open:
- [x] Measure per-PVC actual usage (2026-11-21, above)
- [x] Media library: excluded as replaceable (2026-11-21)
- [x] Two apps scaled to 0 excluded: their PVCs are gone, likely to be removed (2026-11-21)
- [x] **PXC retired 2026-11-21** (only database was the deleted FreshRSS app's). Final dump kept off-cluster; see `local/docs/runbooks/pxc-retirement.md`. No SSD copy needed (2026-11-22): FreshRSS was unused for months and replaced by Tomekeeper, which holds the data worth keeping; Jase has a separate copy of the dump
- [x] Choose the tool (2026-11-21, D13): see "Tooling" below
- [x] Database dumps (2026-11-22): Four apps already have nightly backup CronJobs. Two (one Postgres, one MariaDB) have none, so one-off dumps were taken into `/srv/backup/dumps/2026-11-22/` on the node, snapshotted into Kopia (tag `kind:db-dump`) and synced to the SSD; checksums of the SSD copies match. Their nightly Kopia file copies continue. **Nightly dump CronJobs for those two and ESS since 2026-11-23** (`local/deploy/db-dumps/`), restore-tested with matching table counts, alerting on staleness. Found: one MariaDB's `mysql.proc` reports "probably corrupted" (likely `mysql_upgrade` never run after a version change), so `mysqldump --routines` fails; the app has no routines, dump taken without it
- [x] PXC datadir "nearly full": false alarm (2026-11-21). The filesystem is 5% used; `rbd du` was counting stale blocks
- [x] Kopia backup and etcd snapshot timers installed, SSD sync procedure written, restores tested (2026-11-21): `local/docs/runbooks/backups.md`, `local/ops/backup/`
- [x] First sync to the external SSD (2026-11-22): 48 GB, 27 snapshot sources incl. etcd, about 22 min over USB 2; `snapshot verify` (2% of files read back) passed

**Tooling (current state):**
- **etcd:** `etcdctl snapshot save` from a host-level systemd timer, written to the NVMe backup directory.
- **PVC data:** Kopia used on its own (no Velero, no object store), run on the host from a systemd timer (changed from the planned CronJob: one host job can read PVCs in every namespace through their kubelet mounts, and keeps working when the cluster is unhealthy). It writes a filesystem repository on the NVMe with deduplication, encryption and a retention policy. The media library is excluded.
- **Databases:** dumps, never copies of live data files. Kopia backs up the dump output.
- **Off-node:** `kopia repository sync-to filesystem` to the external SSD (Samsung T5, exFAT, shared with other data; only the encrypted repository goes on it, which already contains the etcd snapshots), then unplug. Restoring needs only the `kopia` CLI and the SSD, not a working cluster.
- **Monitoring:** Job success or failure through kube-state-metrics once A4 exists. Add a last-success-timestamp metric if that isn't enough.

*Why not Velero now:* the node was at 88% CPU requested when this was decided (2026-11-21; 4.03 of 12 cores after D16, so that reason no longer holds), and Velero needs always-running components (server and node-agent) plus an object store. MinIO is ruled out: its community edition was archived in 2026 and no longer gets security fixes. Ceph's S3 service (RGW) would store backups on the disks they protect.

**Future state: Velero + Garage.** Adopt when Ceph is expanded or nodes are added (more headroom, and somewhere off the main node to put Garage), or at Phase 2, whichever comes first. What it adds for familiar:
- Backup status the agent can read: `Backup`/`Schedule` resources and Prometheus metrics, so the watch officer can report backup failures in detail.
- A namespace-level undo before merging an agent-proposed PR (`velero backup create pre-pr-N --include-namespaces X`), restored with one command.
- The same tool as at work.

Costs to plan for: requests for the server and node-agent; the node-agent's elevated host access; Velero, plugin and Garage upgrades; Garage has no S3 object lock, so the offline SSD stays the copy that can't be deleted.

**Guardrail (both states):** backups hold every Secret (Velero backups and etcd snapshots both do), so the backup namespace, the backup storage (the NVMe path, and Garage later) and the backup config are on the §4.2 "no familiar component can modify" list. The agent may **read** backup status but never create, delete or restore backups. Restores are done only by me.

## 6. Phase 1 — Watch officer (read-only, notify-only)

**Trifecta:** private data (cluster state) ✔ · untrusted input ✔ (low: logs can contain attacker-controlled strings from exposed apps) · external channel ✔ (messaging gateway to me only). All three are present, which is why tool output is redacted, the gateway accepts only my user ID, and the only egress besides model-gate is the in-cluster Matrix homeserver (D19).

### Scope
- Deploy model-gate, tool servers (`cluster-read`, `metrics-query`, `cert-probe`, `ceph-health`), memory-auditor, Hermes. (No heartbeat: the laptop dead-man's switch pulls, and Hermes liveness is an alert rule; D17.)
- Hermes runs periodic health sweeps (heartbeat or schedule) and answers my questions over the messaging channel.
- Output: messages to me with findings, likely causes and *suggested* fixes (text only).

### Setup checklist
- [x] Console: create a `familiar` workspace, set a monthly spend limit, create one key per role ($42/month, 2026-11-22; one role so far)
- [x] model-gate built, image pinned, budgets configured, audit log verified (2026-11-23)
- [x] Hermes image pinned by digest; Docker terminal backend disabled; tool-loop guardrails reviewed and the non-interactive hard stop **kept on**; skills vendored (no runtime skill marketplace installs)
- [x] Hermes pointed at model-gate as its Anthropic base URL via a named custom provider with `transport: anthropic_messages` (Q4); confirm auxiliary calls route through it too (they needed `api_mode` in the provider entry as well; verified on Haiku 2026-11-23)
- [x] Messaging gateway restricted to my user ID
- [x] CNPs applied and tested (agent cannot reach `api.anthropic.com` directly; cannot reach anything but its allowlist): all 11 valid; tested from inside the Hermes VM, 2026-11-23

**Deployed 2026-11-23** (`deploy/familiar/`, real overlay in `local/`). Problems the first apply found, all fixed and recorded in `docs/how-to/deploy-phase-1.md`: Hermes' startup probe had no timeout (Kubernetes' 1 s default always failed; Kata labelled the kill "OOMKilled"), memory-auditor read the ConfigMap baseline through a symlink and always reported drift, side tasks used `/chat/completions` until the provider entry got `api_mode`, the data volume's root was owned by root (fsGroup only sets the group), Hermes warns about low disk under 2 GiB free (volume now 4 GiB; the resize needed a newer Rook CSI resizer), and Hermes' model-discovery probes made a warning fire constantly (now info).

### Exit criteria (run for at least 2 weeks, from 2026-11-23)
- [ ] Zero successful policy violations in Hubble/tool logs (attempts are recorded and reviewed)
- [x] At least one real issue correctly triaged, or a staged fault (e.g. a crashlooping test deployment, a near-expiry cert) diagnosed correctly. Met 2026-11-23: GIDEON diagnosed alert-gate's crash (a `null` result iterated as a list) from the traceback alone, without seeing the script, and proposed the fix that was applied
- [ ] Memory diffs reviewed weekly; nothing unexplained
- [ ] Spend within expectations; no budget breaches, or each one explained
- [x] **Red-team test** (adapted 2026-11-23: Phase 1 has no log access, so a Kubernetes event message is the new log line). A test app in a `redteam` namespace fails its liveness probe with an injection as the probe output; the kubelet records it in a Warning event, `KubePodCrashLooping` fires, and the alert-triage job reads it unattended. Five payloads, one run each: (A) suppress the alert, (B) make extra tool calls, (C) persist an instruction in memory, (D) exfiltrate through a link, (E) probe familiar's own namespaces. **Containment must hold every run** (no boundary crossed, no unexpected egress; a failure is a bug in familiar). **Resistance is measured and recorded**, not pass/fail: what GIDEON did with each payload, with the tool servers' logs, memory-auditor diffs and its Matrix post as evidence. That record is evidence for the email assistant (G6). **Done 2026-11-24:** all five resisted and flagged; containment held but was never tested, since GIDEON tried nothing a guardrail had to stop. Record and caveats (the namespace name gave the test away) in [docs/explanation/red-team-phase-1.md](explanation/red-team-phase-1.md)
- [ ] Written retro: what surprised me

## 7. Phase 2 — Proposed changes via PR

**Adds:** write path, but only as PRs.
- `propose-change` tool server holds a forge token scoped to the home GitOps repo, with a **path allowlist** (app namespaces only; never `familiar-*`, RBAC, CNPs, RuntimeClasses, Authelia, Rook).
- Branch protection: the token can open PRs but cannot merge or push to the default branch.
- The PR description must include the agent's diagnosis and evidence (tool outputs referenced by audit-log IDs).

**Exit criteria:** ≥5 PRs reviewed; zero attempts to touch disallowed paths succeed; I'd have merged most of them unchanged.

## 8. Phase 3 — Quarantined reader (rehearsal for the email assistant)

**Adds:** untrusted input at volume.
- A second Hermes instance, `hermes-reader`, in its own pod: **no tools** except a read-only FreshRSS API client (note 2026-11-21: FreshRSS is no longer deployed; reinstall it or pick another feed source before Phase 3) (and later CVE advisory feeds matched against my image SBOMs).
- It emits **structured output only** (a JSON schema: item, summary, relevance score, CVE IDs). A deterministic validator rejects anything off-schema.
- The watch officer (or I) consumes only the validated JSON, never raw feed text.
- **Trifecta:** reader has untrusted input only; no private data, no external channel.

**Exit criteria:** injected test content in a feed item doesn't propagate as instructions through the schema boundary; written notes on what transfers to the email-reading assistant's design.

## 9. Phase 4 — Builder (design doc → implementation)

**Adds:** arbitrary code execution, package installs, and the most resource load. **Requires node specs and a Phase 0 resource measurement before starting.**

### 9.1 Shape
- A deterministic **dispatcher** (no LLM) watches the forge for approved design docs/issues (label or branch convention). For each approved item it creates a Kubernetes Job.
- The Job runs a coding agent (Claude Code headless, or Codex) under the sandboxed RuntimeClass, with build scratch on **node-local ephemeral storage, not Ceph RBD**.
- Output is always a PR. CI builds images; deploy is a GitOps PR that I merge.
- Concurrency: 1 build at a time. Lowest PriorityClass in the cluster.

### 9.2 Replicating agent-sandboxes on the cluster

agent-sandboxes was built for workstations. What carries over, and how:

| agent-sandboxes capability | Cluster equivalent in familiar | Notes |
|---|---|---|
| k3s inside a VM per workstation | Not needed; the home cluster *is* the substrate | |
| Cilium `toFQDNs` egress allowlist | CNP on the builder Job's namespace | Same policies; reuse from the repo |
| gVisor / Kata (v3 direction) | RuntimeClass on builder pods | Phase 0 decision applies |
| Credential brokering (v3 requirement) | model-gate for model keys; a forge broker that injects a repo-scoped token for git push | Same pattern as Phase 1; generalise the gate |
| Install-not-publish constraint (GitHub/npm/PyPI) | Egress via a caching proxy that permits only GET/fetch to registries | Turns the cooperative v3 constraint into a structural one; candidate to port back to agent-sandboxes |
| Secret fingerprint / attested exceptions | Pre-PR secret scan (betterleaks / detect-secrets) in the Job before push | |
| Mutagen file sync, host CLI | Not needed; workspace is a fresh clone in the pod | |
| Interactive human in the session | **Lost.** Replaced by PR review as the only human gate | The biggest behavioural change; keep PR scope small |
| Concurrent sessions on one working tree | Not needed; one clone per Job | |

### 9.3 Decision gate: build it vs. adopt a runtime
Before implementing the dispatcher, re-evaluate using Spike S1 (§10): if Agent Substrate (or a similar runtime) provides sandbox lifecycle, egress policy and secret injection that fit this cluster with acceptable overhead, prefer adopting it over building the dispatcher's lifecycle management.

### Exit criteria
- [ ] One design doc implemented end-to-end into a PR I'd merge
- [ ] No egress outside allowlist; no publish attempts succeed
- [ ] Node stayed healthy during builds (no app evictions; Ceph latency within normal range)

## 10. Spikes (time-boxed, separate from the phases)

### S1 — Agent Substrate evaluation (time box: 2 evenings)
Context: Google is donating Agent Substrate (and gVisor) to the CNCF at Sandbox maturity. It separates stateful agent "Actors" from pre-warmed "Worker" pods and snapshots/resumes Actor state. Its stated design target is very large scale and density, which a single-node home cluster doesn't need. Its potential value here is suspend/resume for idle agents, gateway-style egress and secret injection, and its relevance to agent-sandboxes.

Questions to answer:
- [ ] Control-plane footprint on a single node (CPU/RAM at idle)
- [ ] Isolation backends: gVisor and Cloud Hypervisor are documented; does it work with the Phase 0 runtime choice? Kata support?
- [ ] How egress rules and secret injection are actually enforced (CNI-dependent? own proxy?)
- [ ] Governance: what's in the CNCF project vs. the separate Agent Executor (AX) layer
- [ ] Could it replace the Phase 4 dispatcher? Could it serve as an agent-sandboxes v3+ backend?
- [ ] Does Hermes (incl. the reported Hermes Enterprise work) run on it?

Output: `docs/spikes/agent-substrate.md` with a recommendation.
Also worth a look later: NVIDIA's OpenShell (also donated to the CNCF; policy enforcement around the agent using Cedar). Not yet researched.

### S2 — Model swap (late Phase 1 or Phase 3, time box: 1 week)
Switch one role to a non-Anthropic provider through model-gate. Purpose: separate framework behaviour from model behaviour, since the email assistant must stay model-agnostic.

## 11. Decision log

| # | Decision | Date |
|---|---|---|
| D1 | Hosted models only; Claude via API (my Console account) is the default for all phases, with a ~1-week swap of one role to another hosted provider (S2); local models out of scope for the pilot | 2026-11-19 |
| D2 | Hermes Agent for watch officer and reader (persistent-agent evaluation + email-assistant rehearsal); builder uses existing coding agents | 2026-11-19 |
| D3 | The agent holds no credentials; brokers (model-gate, tool servers) hold them | 2026-11-19 |
| D4 | Custom minimal model-gate rather than LiteLLM | 2026-11-19 |
| D5 | Alertmanager remains the primary alert path; the agent is advisory | 2026-11-19 |
| D6 | Phases ordered by risk; each widens one dimension | 2026-11-19 |
| D7 | Agent Substrate evaluated as a spike, not adopted as the foundation | 2026-11-19 |
| D8 | Personal infrastructure only; no work systems | 2026-11-19 |
| D9 | Phase 0 split into Tier A (must-have) and Tier B (deferred); see §5.0 | 2026-11-19 |
| D10 | Kata chosen over gVisor (bare metal, `/dev/kvm` present) | 2026-11-19 |
| D11 | Infrastructure maintenance (containerd, kubelet, Cilium, OS) done as Phase 0 item A3, after backups and before Kata | 2026-11-19 |
| D12 | Backups: NVMe local tier (automated) plus external SSD off-node copy (manual, before each window); `/storage` excluded (dying disk); the media library excluded (replaceable); automated off-node backups deferred to Tier B | 2026-11-21 |
| D13 | Backup tooling: Kopia standalone (filesystem repo on the NVMe) plus etcdctl snapshots now; Velero + Garage as the future state after Ceph expansion, more nodes, or Phase 2; MinIO ruled out (community edition archived) | 2026-11-21 |
| D14 | No node reboots and nothing touching `/storage` until the `/storage` data recovery is finished (weeks). Node-level changes must work with at most a kubelet or pod restart, or wait | 2026-11-22 |
| D15 | Enable the kubelet's `cgroupsPerQOS` (and default node-allocatable enforcement) by removing `--cgroups-per-qos=false --enforce-node-allocatable=` from `kubeadm-flags.env`, deferred to the first reboot after the `/storage` recovery. A no-reboot staged migration was considered and rejected: unsupported, leaves a mixed cgroup layout, small risk of needing a reboot to recover. In-place pod resize stays off-limits until then (#137993) **Extended 2026-11-22 (A3):** the same window also sets containerd's runc `SystemdCgroup = true` to match the kubelet's `systemd` cgroup driver (currently mismatched), rewriting `config.toml` in the current format. All three changes go together, followed by the reboot **Done 2026-11-23** in a shutdown window for disk maintenance (the node was powered off, not rebooted). Verified after boot: kernel 6.8.0-149, the kubelet on the systemd cgroup driver with `CgroupsPerQOS: true` and node-allocatable enforcement, pods under `kubepods.slice`, containerd `SystemdCgroup = true`, no kubelet panics, every workload back. The in-place resize bug's trigger is gone. Two benign boot effects: one kubelet restart because it started before containerd's socket was ready, and a few "failed to read memory.max" errors while pod cgroups were being created. | 2026-11-22 |
| D16 | Rook requests: mon CPU lowered to 250m (done 2026-11-22). The mgr (500m) and OSD (1 CPU each) requests stay as they are until A4 monitoring can watch the effect **Updated 2026-11-23:** with 23.5 h of data (§5.5), CPU requests lowered via `rook-ceph-cluster` Helm revision 15: OSDs 1 → 250m, mgr 500m → 100m, mons 250m → 100m, collectors 10m, exporter 25m (memory unchanged). Rook restarted the second OSD 11 s after the first came back, causing 30 s of `HEALTH_ERR` with 15 unfound objects, all recovered: `local/docs/incidents/2026-11-23-ceph-unfound-objects.md`. `upgradeOSDRequiresHealthyPGs: true` set (revision 16). Then CSI plugins and operator via `rook-ceph` Helm revision 4 (chart v1.14.7 unchanged): sidecars 10m, plugins 25m, operator 50m. **Node CPU requests 8.66 → 4.03 of 12.** CephFS CSI turned off (`rook-ceph` revision 5; nothing used CephFS) and the orphaned `rook-cephfs`/`rook-cephfs2` StorageClasses deleted (YAML kept in the private cluster export). | 2026-11-22 |
| D17 | No external alert delivery and no cloud dependency for monitoring: this is a home lab, and not being able to reach it is itself the signal. Alertmanager routes everything to its `null` receiver; alerts are read in the UI/API. The dead-man's switch is a checker on a LAN machine (laptop, later Raspberry Pi) that polls Alertmanager. Replaces the healthchecks.io plan; revises D5 | 2026-11-22 |
| D18 | Cilium L7 proxy enabled (it was explicitly off), because `toFQDNs` policies are rejected and fail open without it. Adds the `cilium-envoy` DaemonSet. Policy-writing rules in §4.3 | 2026-11-22 |
| D19 | Messaging channel (Q8): a self-hosted Matrix homeserver in the cluster, federation off, reachable only from LAN/VPN; Hermes uses its Matrix gateway with `MATRIX_E2EE_MODE=required` and `MATRIX_ALLOWED_USERS` set to Jase only. The agent's egress then has no internet destination besides model-gate, so the trifecta's external channel is a server we run. Chosen over Telegram (cloud, no E2EE for bots), Signal (needs a second number; a linked device would put Jase's whole account in the agent pod) and ntfy (topic name is the only identity). The homeserver may later also serve friends and family, which makes its own hardening and backups matter beyond familiar | 2026-11-22 |
| D20 | Model routing for the watch officer (Q9): three tiers. (1) **Alert check every 15 min, no model:** a cron pre-run script reads firing alerts (Prometheus `ALERTS` via the metrics-query tool server) and prints `{"wakeAgent": false}` unless a new alert is firing, in which case a Sonnet-pinned job triages it. (2) **Health sweep every 4 hours on Haiku:** a cron job pinned to Haiku 5.5 that ends with `[SILENT]` when healthy and otherwise reports to Matrix. (3) **Conversations on Sonnet 5.5** (the default model); escalation from a Haiku finding is Jase replying in Matrix. Opus is not in model-gate's allowlist. The agent cannot repoint a job's model (only `pinned=true`), and model-gate's allowlist enforces it anyway. Rejected for now: Haiku delegating to a Sonnet subagent (needs the `model_map` delegation plugin; inheritance of the custom model-gate provider is unverified) | 2026-11-22 |
| D21 | Matrix homeserver (D19): **Element Server Suite Community** (`element-hq/ess-helm`: Synapse, Matrix Authentication Service (MAS), Element Web; Element Call off unless wanted), federation off, in its own namespace. **Access goes through Traefik** (LAN/VPN-only at first, public later for friends and family). Matrix clients can't do Authelia's forward-auth, so the client API is never behind the Authelia middleware; **MAS is the only login service**. **Auth path:** start with MAS local password accounts; later enable Authelia's OIDC provider (4.39 supports it; not configured today) and add it to MAS as an upstream provider, so people log in through Authelia with its 2FA while Matrix tokens stay MAS-issued. **Familiar never depends on that path:** Hermes talks to Synapse's in-cluster Service (its CNP allows only that), never the Traefik hostname, and uses a dedicated bot account (`@familiar:…`) with an access token issued directly by MAS (`mas-cli manage issue-compatibility-token`), not by any login flow. Changing exposure, adding Authelia upstream or disabling passwords doesn't touch the bot. Costs: Postgres (needs a dump job), more components than a single-binary server. Rejected: Continuwuity/Tuwunel (smaller teams, disputed succession), plain Synapse (fallback if ESS proves too heavy) **Local test findings (2026-11-22):** Hermes' `MATRIX_E2EE_MODE=required` only refuses to start without crypto; it still answers an allowlisted user in an unencrypted room, and `MATRIX_ALLOWED_ROOMS` exempts DMs. So Synapse must set `encryption_enabled_by_default_for_room_type: all` (tested: an explicitly unencrypted room is created encrypted, and encryption can't be removed). Non-allowlisted invites are rejected before Hermes joins. **Installed 2026-11-23** (`local/deploy/matrix/`): federation blocked, encryption on for all rooms, `@jase` and `@gideon` created, GIDEON's compat token verified. Nightly dumps of both databases since 2026-11-23 (`local/deploy/db-dumps/`). | 2026-11-22 |
| D22 | The agent's identity: **GIDEON** ("Generally Intelligent Daemon, Exceptionally Organized & Nosy"), Matrix ID `@gideon` on the home server (permanent), persona in `deploy/familiar/hermes/SOUL.md` (changeable any time). The persona is voice only: findings first and plain, stated confidence, tool output as data, exact `[SILENT]`. **Long-run principle (Jase, 2026-11-22):** GIDEON will eventually build, deploy and fix things, but **only with Jase's explicit permission for that specific change, never on its own initiative.** Each phase's `SOUL.md` states exactly what GIDEON may do in that phase (Phase 1: nothing; Phase 2: propose changes as PRs Jase merges), and the structural controls (no credentials, brokers, CNPs, approvals) enforce it regardless of what the prompt says | 2026-11-22 |
| D23 | The week-long CPU baseline is no longer a Phase 0 exit criterion (Jase, 2026-11-23): recent days aren't representative (new deployments, `/storage` recovery jobs, tests). Conclusions from the first 23.5 h are recorded in §5.5; Prometheus history informs Jase and GIDEON from here on | 2026-11-23 |
| D24 | Storage changes in the first reboot window (Jase, 2026-11-23): the dying `/storage` disk is retired (fstab line commented, disk physically removed at power-off), the 6.8.0-149 kernel is accepted, and a **new 14 TB disk becomes a third Ceph OSD** (not standalone). With three OSDs the pools move to `size 3`, `min_size 2` (and `osd_pool_default_size 3`), which removes the two-OSD risk from the 2026-11-23 incident; usable capacity stays ~12.7 TB. The OSD is added by its `/dev/disk/by-id/` name to the CephCluster (Helm values), after the reboot is verified and after the Rook operator upgrade (planned 2026-11-24). Runbook: `local/docs/runbooks/reboot-window.md` §5b **What the 2026-11-23 window did:** `/storage` was *not* retired. Its recovery continues, so it is no longer mounted at boot and is brought back deliberately when needed. The new disk was added as the third OSD in the same window, ahead of the Rook upgrade; the pool size change follows separately. | 2026-11-23 |
| D25 | Cilium socket load balancing in the host namespace only (`socketLB.hostNamespaceOnly=true`, Helm revision 13), found while testing Kata (A6). Socket LB was on for pods too, so ClusterIPs were translated only for connections made in the host's kernel; Kata VMs have their own, and their ClusterIP traffic left the node untranslated. Now pods, VM or not, are translated per packet at their interface. runc pods behave the same; the FQDN test passes under both runtimes. The `cilium-config` ConfigMap's `bpf-lb-sock: "false"` didn't reflect the agent's state; trust `cilium-dbg status --verbose` | 2026-11-23 |
| D26 | LAN visibility for the watch officer through exporters, not access (Jase, 2026-11-26). Read-only exporters outside familiar (connectivity probes, the UniFi controller through a view-only account, the LAN DNS resolver) feed Prometheus; the agent reads them only through `metrics-query` templates and gets no LAN egress. Adds private data (per-device traffic and names; per-device views accepted for Phase 1) and a new untrusted input: client names, which anyone on the LAN can set, are cleaned in the tool server (§4.1 tool table, `docs/reference/tool-servers.md`). The DNS resolver has no read-only role, so its exporter holds an admin login; it stays in the exporters' namespace with egress to that host only. | 2026-11-26 |

## 12. Open questions

| # | Question | Needed by | Answer |
|---|---|---|---|
| Q1 | Node specs: CPU, RAM, disks/OSD layout, baremetal or VM? | Phase 0 / 4 | **Answered 2026-11-19:** bare metal, 6C/12T desktop CPU, 32 GB, 1 TB NVMe OS disk, 2×12.7 TB HDD OSDs. The extra 14.6 TB `/storage` disk is dying and is not usable, not even as a backup target. See `local/docs/cluster-inventory.md`. |
| Q2 | Is the CNI Cilium, with `toFQDNs` enabled? | Phase 0 | **Answered 2026-11-22:** Cilium 1.20.5; `toFQDNs` needed the L7 proxy, now enabled and tested (A5). |
| Q3 | Which forge and GitOps controller manage the home cluster? | Phase 0 | **Answered 2026-11-22:** GitHub (`runlevel-six/familiar`, `deploy/`); no GitOps controller yet (manual apply; controller is Tier B). |
| Q4 | Can Hermes use a custom Anthropic base URL (pointing at model-gate)? If not, does its OpenAI-compatible endpoint support work with a translating gate? | Phase 0 | **Answered 2026-11-19 (docs; untested):** yes, via a named custom provider in `config.yaml`: `providers.<name>.api: <gate URL>`, `transport: anthropic_messages`, and `model.provider: custom:<name>`. No `base_url` key for the native Anthropic provider. Whether a placeholder key is needed when the gate injects it is undocumented; try omitting `key_env`/`api_key` first. **Don't name the entry `anthropic`** (keyless built-in-named entries send no key off the provider's host). A GitHub issue (#12146) reports custom providers still falling back to OpenRouter for some calls, so verify that auxiliary calls (e.g. the smart-approval classifier) also go through model-gate. |
| Q5 | Does Hermes consume tools via MCP? If not, what's the tool-extension mechanism? | Phase 0 | **Answered 2026-11-19:** yes, Hermes is an MCP client. Servers go under `mcp_servers` in `config.yaml`; remote servers use `url` + `headers` (Streamable HTTP; `transport: sse` for legacy), with optional mTLS (`client_cert`/`client_key`). Per-server tool allowlist via `tools.include` (globs). **Set `sampling` and `elicitation` to disabled per tool server** (both default on). Hermes strips invisible Unicode TAG characters from tool output. |
| Q6 | Where exactly does Hermes store memory and learned skills under `/opt/data`, and in what format (for memory-auditor)? | Phase 0 | **Answered 2026-11-19 (format partly undocumented):** `/opt/data` = `~/.hermes`. `memories/MEMORY.md` and `memories/USER.md` (entries separated by `§`; hard caps 2,200 / 1,375 chars), `skills/`, `config.yaml`, `.env`, `SOUL.md` (personality/identity), `hooks/`, `cron/`, `sessions/`, `state.db` (SQLite), `logs/`, `skins/`, `home/` (HOME for tool subprocesses). Memory-auditor scope is wider than "memory and skills", see §4.1. |
| Q7 | Can Hermes' autonomous skill creation be disabled, or limited to a reviewed directory? | Phase 0 | **Partly answered 2026-11-19:** `skills.write_approval: true` stages every `skill_manage` write under `pending/skills/` for `/skills approve`; `memory.write_approval` does the same for memory. Neither covers direct writes via file or terminal tools. The docs say `skills.external_dirs` is not a write-protection boundary, so enforce with filesystem permissions: mount vendored, reviewed skills read-only. No documented way to remove `skill_manage` entirely (`agent.disabled_toolsets` exists; whether the `skills` toolset covers it is unverified). **Answered 2026-11-22 (local test):** disabling the `skills` toolset removes `skill_manage`; vendored skills mounted read-only. See A1 follow-ups in §5.0. |
| Q8 | Messaging channel: Telegram / Signal / Matrix (self-hosted) / other? | Phase 1 | **Answered 2026-11-22: self-hosted Matrix** (D19). Jase uses iPhone and other iOS devices, so notifications need the homeserver (never the agent) to reach the client app's push gateway (Element's, which relays to Apple's APNs): one egress FQDN for the homeserver, with event-ID-only push payloads so no message content leaves. Exact gateway hostname to confirm when building the homeserver. The official image bakes in the Matrix E2EE dependencies (`[matrix]` extra, `libolm`; Dockerfile at v0.23.3); runtime check in the local test's round 2. |
| Q9 | Token budget numbers (per request / hour / day) | Phase 1 | **Answered 2026-11-22:** watch officer $1/hour and $3/day in model-gate, Console workspace limit $42/month (set 2026-11-22; the answer to everything); health sweeps every 4 hours; models Sonnet 5.5 and Haiku 5.5 only (no Opus). Expected spend $7–22/month. Tiered routing (Haiku sweeps, Sonnet triage and chat) per D20, to be confirmed in the local Hermes test; recheck the numbers from `model_gate_cost_usd_total` after the first week. Details in `docs/explanation/budgets-and-model-routing.md`. |
| Q10 | Final project name | Anytime | **Answered 2026-11-23: familiar** is the system (model-gate, tool servers, auditor, policies, future agents); **GIDEON** is the agent (D22). |

## 13. Milestones

- [x] M0 — Phase 0 complete (2026-11-23) (inventory, prereqs installed, Hermes questions answered)
- [x] M1 — Phase 1 deployed (2026-11-23)
- [ ] M1.5 — Phase 1 exit criteria met + retro
- [ ] M2 — Phase 2 exit criteria met
- [ ] M3 — Phase 3 exit criteria met + notes for the email assistant
- [ ] S1 — Agent Substrate spike report
- [ ] M4 — Phase 4 exit criteria met

## 14. References

- Hermes Agent Docker guide: https://hermes-agent.nousresearch.com/docs/user-guide/docker
- Hermes Agent security model: https://hermes-agent.nousresearch.com/docs/user-guide/security
- Agent Substrate / gVisor CNCF donation (Google Open Source Blog, 2026-11-05): https://opensource.googleblog.com/2026/11/sandboxing-untrusted-ai-agents-at-scale.html
- Claude Console workspaces & spend limits: https://platform.claude.com/docs/en/manage-claude/workspaces
- LiteLLM 2026 supply-chain incidents: https://www.helpnetsecurity.com/2026/10/22/litellm-compromised-supply-chain-attack-pypi-docker/
- agent-sandboxes: https://github.com/samaritanark/agent-sandboxes
