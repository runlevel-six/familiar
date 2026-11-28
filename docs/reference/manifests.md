# Manifests and policies

Phase 1 is a kustomize base in [`deploy/familiar`](../../deploy/familiar), applied
through a site overlay ([how-to](../how-to/deploy-phase-1.md)). Everything runs
under the restricted Pod Security Standard.

## Files

| File | Contents |
|---|---|
| `kustomization.yaml` | The base: every file below plus `hermes/` |
| `00-namespaces.yaml` | `familiar-system`, `familiar-agents`, `familiar-tools`, each enforcing the restricted Pod Security Standard |
| `01-priority-and-quotas.yaml` | The `familiar-low` PriorityClass (-100, never preempts); a ResourceQuota and LimitRange per namespace |
| `02-default-deny.yaml` | Default-deny ingress and egress in each namespace |
| `10-model-gate.yaml` | model-gate: Deployment, config (ConfigMap), state PVC, Service, network policies, ServiceMonitor and alert rules |
| `20-tool-servers.yaml` | The five tool servers in `familiar-tools`, the ClusterRoles of cluster-read and vuln-read, their network policies, and the watch officer's egress policy |
| `hermes/kustomization.yaml` | Hermes and memory-auditor; ConfigMaps generated from `config.yaml`, `SOUL.md`, `scripts/` and `memory-auditor.py`, so a change rolls the pods |
| `hermes/deployment.yaml` | Hermes: ServiceAccount (no token mounted), data PVC, Deployment |
| `hermes/memory-auditor.yaml` | memory-auditor: its PVC, Deployment, Service, network policy, ServiceMonitor and alert rules |
| `hermes/alerts.yaml` | Hermes liveness alerts |
| `hermes/config.yaml` | Hermes' configuration, copied onto its volume at every start |
| `hermes/SOUL.md` | The agent's persona and rules |
| `hermes/scripts/alert-gate.py` | Decides whether a 15-minute alert check should wake the agent |

## Site values

The base holds placeholders for these; set them in your overlay
([example](../../deploy/overlays/example/kustomization.yaml)):

| Value | Where | Placeholder |
|---|---|---|
| Hosts cert-probe checks | `cert-probe` Deployment, `HOSTS` | `app.example.org,auth.example.org` |
| The agent's Matrix ID | `hermes` Deployment, `MATRIX_USER_ID` | `@gideon:example.org` |
| The one user it answers | `hermes` Deployment, `MATRIX_ALLOWED_USERS` | `@owner:example.org` |
| Its home room, for scheduled output | `hermes` Deployment, `MATRIX_HOME_ROOM` | commented out |
| The backup gauge | `metrics-query` Deployment, `BACKUP_METRIC` | the code's default |
| UniFi firewall zone names | `metrics-query` Deployment, `UNIFI_FIREWALL_ZONES` | made-up IDs for External, VPN and Internal |

Secrets are created by hand, never kept in the repository: `model-gate-keys` in
`familiar-system` (one file per role) and `hermes-matrix` in `familiar-agents`
(key `access-token`).

## Labels

| Label | Meaning |
|---|---|
| `familiar/managed: "true"` | On the three namespaces |
| `familiar/role: watch-officer` | The Hermes pod; selects the watch officer's egress policy and its access to model-gate and the tool servers |
| `familiar/kind: tool-server` | Tool server pods |
| `familiar/needs: prometheus` | Pods allowed to reach Prometheus (metrics-query, ceph-health) |

## What each policy allows

Everything not listed is denied.

| From | To | Port |
|---|---|---|
| watch officer | model-gate | 8081 |
| watch officer | the tool servers (`familiar-tools`, and host-read in `familiar-host`) | 8080 |
| watch officer | kube-dns, for eight names only: model-gate, the six tool servers and `ess-synapse.matrix` | 53 |
| watch officer | the Matrix homeserver's HAProxy pods (`matrix` namespace) | 8008 |
| model-gate | `api.anthropic.com`, by DNS name | 443 |
| cluster-read | the Kubernetes API | 6443 |
| metrics-query, ceph-health | Prometheus (never Alertmanager) | 9090 |
| cert-probe | the ingress controller | 8443 (Service 443) |
| Prometheus | model-gate metrics | 9090 |
| Prometheus | memory-auditor metrics | 9100 |
| memory-auditor | nothing | |

Names are fully qualified (`*.svc.cluster.local`), and every pod under a strict-DNS
policy sets `ndots: 1`; see [Network isolation](../explanation/network-isolation.md).

## Resources

| Namespace | Quota |
|---|---|
| `familiar-agents` | 1 CPU and 3 GiB requested, 4 GiB memory limit, 3 pods, 2 PVCs, 10 GiB storage |

The other namespaces' quotas and the per-container defaults are in
`01-priority-and-quotas.yaml`.
