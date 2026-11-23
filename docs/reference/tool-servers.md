# Tool servers reference

Four read-only MCP servers, one image (`ghcr.io/runlevel-six/familiar-tools`), one
binary each. They hold the credentials, enforce the policy, log every call and
return redacted summaries; the agent holds nothing. Source: [`tools/`](../../tools).
Go standard library only.

| Server | Credential | Backend |
|---|---|---|
| `cluster-read` | A ClusterRole with get/list on pods, events, nodes, workloads, storage, ingresses and cert-manager Certificates. Never Secrets, ConfigMaps, `pods/log`, exec, RBAC or network policies | The Kubernetes API |
| `metrics-query` | None | Prometheus, through fixed query templates |
| `ceph-health` | None (no Ceph keyring) | Ceph metrics already in Prometheus |
| `cert-probe` | None | TLS connections to the ingress controller, for a fixed list of hosts |

## Tools

### cluster-read

familiar's own namespaces (`familiar-*`) are invisible: asking for one is an
error, and their objects are filtered out of every list before any tool sees them.

| Tool | Parameters | Returns |
|---|---|---|
| `cluster_overview` | | Node health, pod counts by phase, unhealthy pods, workloads not fully available, warning events in the last hour |
| `list_pods` | `namespace`, `only_unhealthy` (default false), `limit` (1–300, default 100) | Phase, readiness, restarts, last restart reason, current problem, node, owner, images. No environment, arguments or logs |
| `describe_workload` | `namespace`, `kind` (`deployment`, `statefulset`, `daemonset`, `job`, `cronjob`), `name`; all required | Status, conditions, containers (image, CPU and memory requests and limits), pods, recent warning events |
| `recent_events` | `namespace`, `warnings_only` (default true), `since_minutes` (1–1440, default 60), `limit` (1–200, default 50) | Events, newest first, messages redacted |
| `storage_status` | `namespace` | PVCs (phase, capacity, class), PersistentVolumes that aren't Bound, StorageClasses |
| `certificates` | `namespace` | cert-manager Certificates: DNS names, Ready, expiry, renewal time |
| `ingresses` | `namespace` | Hosts, class, TLS hosts, backend services |

`namespace` is optional everywhere except `describe_workload`; leaving it out means
all (visible) namespaces.

### metrics-query

No arbitrary PromQL: the agent picks a template, and parameters are validated.

| Tool | Parameters |
|---|---|
| `query_metric` | `template` (required); `namespace`, `window` (`5m`, `15m`, `1h`, `6h`, `24h`; default `1h`), `limit` (1–20, default 10) for templates that take them |
| `metric_history` | `template` (`node_cpu_cores_used` or `node_memory_used_fraction`); `hours` (1–168, default 6). At most 120 points |

| Template | Parameters | Returns |
|---|---|---|
| `node_cpu_cores_used` | | CPU cores in use (all non-idle modes) |
| `node_memory` | | Total, available and used fraction |
| `node_memory_used_fraction` | | Fraction of memory in use |
| `node_filesystems` | | Free fraction of each node filesystem |
| `cpu_requests_vs_allocatable` | | Requested CPU of running pods, and allocatable CPU |
| `pod_restarts` | `namespace`, `window` | Containers that restarted within the window |
| `top_container_cpu` | `namespace`, `limit` | Pods using the most CPU |
| `top_container_memory` | `namespace`, `limit` | Pods using the most memory (working set) |
| `cpu_throttling` | `namespace` | Containers throttled in more than 25% of CFS periods over 5 minutes |
| `pvc_usage` | `namespace` | Used fraction of each mounted PVC |
| `firing_alerts` | | Firing alerts, except `Watchdog` and `InfoInhibitor` |
| `targets_down` | | Scrape targets that are down |
| `backup_age_hours` | | Hours since the last successful backup of each kind (see `BACKUP_METRIC`) |
| `kubelet_restarts` | `window` | kubelet restarts within the window |

### ceph-health

| Tool | Returns |
|---|---|
| `ceph_status` | Overall health, active health checks, monitor quorum, OSD up/in counts, placement group states, capacity |
| `ceph_osds` | Per OSD: up/in, fullness, size, apply and commit latency |
| `ceph_pools` | Per pool: used fraction, stored bytes, space available |

### cert-probe

| Tool | Parameters | Returns |
|---|---|---|
| `probe_all_certificates` | | For every configured host: whether the served certificate is valid for it, issuer, expiry, days left |
| `probe_certificate` | `host`, one of the configured hosts | The same, for one host |

Hosts come only from the `HOSTS` setting, never from the caller, so the probe
can't be pointed anywhere else.

## Environment variables

| Variable | Server | Default | |
|---|---|---|---|
| `LISTEN` | all | `:8080` | MCP listen address |
| `PROMETHEUS_URL` | metrics-query, ceph-health | `http://kps-prometheus.monitoring.svc:9090` | Prometheus base URL |
| `BACKUP_METRIC` | metrics-query | `backup_last_success_timestamp_seconds` | The gauge your backup jobs write (a Unix timestamp per backup kind). Must be a bare metric name |
| `TARGET` | cert-probe | `traefik.traefik.svc:443` | Where to connect; each host is sent as the TLS server name |
| `HOSTS` | cert-probe | | Comma-separated hosts to probe |
| `KUBE_DEV_PROXY_URL` | cluster-read | | Local testing only: use a `kubectl proxy` instead of the pod's ServiceAccount |

## Guardrails in every server

- **Redaction** of every string in every result, keys included: Anthropic, AWS
  and GitHub keys, JWTs, bearer tokens, private keys, `password=`-style pairs,
  credentials in URLs, email addresses, IPv4 and IPv6 addresses. Invisible
  characters that could hide instructions (Unicode tags, bidi controls,
  zero-width characters) are stripped, and long strings are truncated. Results
  over 256 KiB are refused with a hint to narrow the query.
- **Audit:** one JSON log line per call, rejected and unknown ones included:
  tool, redacted arguments, client, remote address, protocol version, outcome,
  result size, duration.
- **Rate limits** per minute: cluster-read 60, metrics-query and ceph-health 30,
  cert-probe 20.
- **Strict arguments:** unknown fields are errors; namespaces and names are
  checked against Kubernetes naming rules before any API call. Problems come back
  as `isError` results the model can correct; internal errors don't leak detail.
- **Untrusted text** (event messages, names) is described as data, not
  instructions, in the tool descriptions.

## Protocol

MCP over Streamable HTTP at `/mcp`, POST only.

- **Current spec (2026-07-28):** stateless; each request carries its protocol
  version in `_meta`. The `MCP-Protocol-Version`, `Mcp-Method` and `Mcp-Name`
  headers must match the body (400, `-32020`). An unsupported version gets 400,
  `-32022`, with the supported list; an unknown method gets 404, `-32601`.
  `server/discover` is implemented.
- **Older clients (2025-03-26 to 2025-11-25):** the `initialize` handshake,
  served statelessly (no session ID).
- **Both:** no batches; notifications get 202; a foreign `Origin` header gets 403
  (DNS rebinding). Only the `tools` capability. Every tool is marked
  `readOnlyHint: true` and listed in a fixed order (it helps prompt caching).
  Results are `structuredContent` plus a JSON text block.

## Connecting Hermes

[`tools/hermes-mcp-servers.example.yaml`](../../tools/hermes-mcp-servers.example.yaml)
is the `mcp_servers` block for Hermes' `config.yaml`: one URL per server, a
`tools.include` allowlist each, and MCP sampling and elicitation off, so a server
can't ask the agent's model for completions.
