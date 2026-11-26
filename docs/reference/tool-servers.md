# Tool servers reference

Five read-only MCP servers, one image (`ghcr.io/runlevel-six/familiar-tools`), one
binary each. They hold the credentials, enforce the policy, log every call and
return redacted summaries; the agent holds nothing. Source: [`tools/`](../../tools).
Go standard library only.

| Server | Credential | Backend |
|---|---|---|
| `cluster-read` | A ClusterRole with get/list on pods, events, nodes, workloads, storage, ingresses and cert-manager Certificates. Never Secrets, ConfigMaps, `pods/log`, exec, RBAC or network policies | The Kubernetes API |
| `metrics-query` | None | Prometheus, through fixed query templates |
| `ceph-health` | None (no Ceph keyring) | Ceph metrics already in Prometheus |
| `cert-probe` | None | TLS connections to the ingress controller, for a fixed list of hosts |
| `host-read` | A ClusterRole with list on pods only; the host's PID namespace, as a non-root user with no capabilities | `/proc` on the node |

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

familiar's own namespaces (`familiar-*`) are hidden here too. Asking for one is
an error, every template excludes them in the query itself, and any result
series that still carries a `familiar-*` namespace (in `namespace`,
`source_namespace` or `destination_namespace`) or one of familiar's own alerts
(`Gideon*`, `ModelGate*`, `MemoryAuditor*`, `Hermes*`) is dropped before it is
returned. The agent can't watch its own guardrails, or learn which of its own
connections the network policies refuse.

One exception: `firing_alerts` includes **operational** alerts about familiar,
the ones that describe its environment's health rather than how it is watched:
`HermesDown`, `HermesRestarting`, `HermesIdle`, `ModelGateDown`,
`ModelGateDailyBudgetHigh`, `ModelGateBudgetExhausted`, `ModelGateUnpricedModel`,
and the generic quota, pod, deployment, volume and throttling alerts
(`KubeQuota*`, `KubePodCrashLooping`, `KubePodNotReady`, `KubeContainerWaiting`,
`KubeDeployment*`, `KubePersistentVolume*`, `CPUThrottlingHigh`). The agent can
report these but not investigate them. Detection alerts (`Gideon*`,
`MemoryAuditor*`, `ModelGatePolicyRejections`, `ModelGateEndpointRejections`,
policy alerts) stay hidden, and so does any operational alert whose labels
mention the auditor: memory-auditor being down is a gap in detection.

A second exception: the `self_usage` template measures the agent's own pod
(`hermes-*` in `familiar-agents`) and nothing else there. It sums over the pod,
so no `familiar-*` label is returned. Hermes runs in a Kata VM, so CPU and
memory are the pod cgroup's, the whole VM as the host pays for it, which is
more than `kubectl top` reports from inside the VM.

| Tool | Parameters |
|---|---|
| `query_metric` | `template` (required); `namespace`, `window` (`5m`, `15m`, `1h`, `6h`, `24h`; default `1h`), `limit` (1–20, default 10) for templates that take them |
| `metric_history` | `template` (`node_cpu_cores_used`, `node_memory_used_fraction`, `self_usage`, `node_cpu_by_mode`, `node_pressure`, `disk_busy_fraction`, `lan_devices_down` or `wan_latency_ms`); `hours` (1–168, default 6). At most 120 points |

| Template | Parameters | Returns |
|---|---|---|
| `node_cpu_cores_used` | | CPU cores in use (all non-idle modes) |
| `node_memory` | | Total, available and used fraction |
| `self_usage` | | The agent's own pod: CPU cores, memory working set (the whole Kata VM) and container restarts |
| `node_memory_used_fraction` | | Fraction of memory in use |
| `node_filesystems` | | Free fraction of each node filesystem (tmpfs and overlay excluded) |
| `node_cpu_by_mode` | | CPU cores in use by mode (user, system, iowait, ...) |
| `node_pressure` | | Pressure stall information: fraction of time tasks waited on CPU, I/O or memory, or were all stalled |
| `disk_io` | | Per physical disk: busy fraction, reads and writes per second, MB/s each way, read and write wait, flush time |
| `disk_busy_fraction` | | Busy fraction of each physical disk |
| `node_network_interfaces` | `window` | Physical NICs: up, link speed, MB/s each way; errors, drops and carrier changes within the window |
| `node_tcp_health` | `window` | TCP retransmit share, timeouts, listen overflows and drops, receive errors, softirq drops, conntrack fill |
| `node_temperatures` | | Hardware temperatures by chip and sensor, with critical thresholds |
| `cpu_requests_vs_allocatable` | | Requested CPU of running pods, and allocatable CPU |
| `pod_restarts` | `namespace`, `window` | Containers that restarted within the window |
| `top_container_cpu` | `namespace`, `limit` | Pods using the most CPU |
| `top_container_memory` | `namespace`, `limit` | Pods using the most memory (working set) |
| `cpu_throttling` | `namespace` | Containers throttled in more than 25% of CFS periods over 5 minutes |
| `pvc_usage` | `namespace` | Used fraction of each mounted PVC |
| `firing_alerts` | | Firing alerts, except `Watchdog`, `InfoInhibitor` and familiar's detection alerts (operational ones about familiar are included; see above) |
| `targets_down` | | Scrape targets that are down, outside `familiar-*` |
| `backup_age_hours` | | Hours since the last successful backup of each kind (see `BACKUP_METRIC`) |
| `kubelet_restarts` | `window` | kubelet restarts within the window |
| `network_drops` | `namespace`, `window`, `limit` | Packets Cilium dropped, by source and destination namespace and reason |
| `network_policy_denials` | `namespace`, `window`, `limit` | Flows a network policy denied, by source and destination namespace |
| `network_flow_verdicts` | `namespace`, `window` | Flows by verdict (forwarded, dropped and so on); with `namespace`, also by source and destination namespace |
| `dns_errors` | `namespace`, `window`, `limit` | DNS responses other than `No Error`, by source namespace and response code |
| `cilium_health` | | Cilium agent checks: endpoints not ready, failing controllers, unreachable nodes and health endpoints, fullest BPF map, Hubble events lost and agent errors in the last hour |

### LAN templates

These read exporters that run outside familiar (a site's own deployment; see
the overlay): blackbox-exporter probes with `device` and `group` labels on
jobs `lan-ping`, `lan-tcp` and `lan-dns`, unpoller (UniFi) and
adguard-exporter (AdGuard Home). The agent gets no LAN access; the exporters
hold the credentials.

| Template | Parameters | Returns |
|---|---|---|
| `lan_reachability` | `window` | Per probed device and group: up now, ping round trip, TCP or DNS probe time, and the fraction of probes failed in the window (when above 0) |
| `lan_devices_down` | | Probed devices failing, by group (history) |
| `wan_health` | `window` | WAN up, latency and availability from the gateway; ping to the internet; internet drops in the window; last speed test and its age |
| `wan_latency_ms` | | WAN latency from the gateway (history) |
| `dns_health` | `window` | AdGuard Home: running and version, protection, average processing time, queries and blocked fraction, each upstream's average response time, exporter errors, and the DNS probe |
| `unifi_devices` | | Gateway, switches and access points: uptime, CPU, memory, temperatures, firmware and Network application updates |
| `switch_port_errors` | `window`, `limit` | Ports with errors or drops in the window, by device and port |
| `wifi_health` | `limit` | Per radio: channel utilization, stations, satisfaction; the Wi-Fi clients with the lowest satisfaction, with signal |
| `top_clients_traffic` | `limit` | Clients moving the most data over 5 minutes, to and from each client (from the byte counters; unpoller's rate gauges read near zero) |

Every LAN template aggregates to named labels, so MAC addresses, BSSIDs and
serial numbers never come back. **Client names are set by the devices
themselves**, so anyone on the LAN controls them: in `wifi_health` and
`top_clients_traffic` the `name` label keeps only letters, digits, spaces and
`.-_'’()#&+`, at most 40 characters (anything else becomes `_`). No link,
markup, control or invisible character survives; plain words still can,
which is why the agent treats tool output as data. Templates with such labels
have no history, because `metric_history` doesn't clean labels.

The network templates read Hubble's flow metrics, which need Hubble metrics
enabled with namespace context (`labelsContext=source_namespace,destination_namespace`
on `drop` and `flow`, `source_namespace` on `dns`); `cilium_health` needs the
Cilium agent's own metrics scraped. Flows are counted, never listed: no pod
names, IPs or ports.

### ceph-health

| Tool | Returns |
|---|---|
| `ceph_status` | Overall health, active health checks, monitor quorum, OSD up/in counts, placement group states, capacity |
| `ceph_osds` | Per OSD: up/in, fullness, size, apply and commit latency, and the host disk it sits on (`device`, as `disk_io` names it; from Ceph's `ceph_disk_occupation_human`) |
| `ceph_pools` | Per pool: used fraction, stored bytes, space available |

### cert-probe

| Tool | Parameters | Returns |
|---|---|---|
| `probe_all_certificates` | | For every configured host: whether the served certificate is valid for it, issuer, expiry, days left |
| `probe_certificate` | `host`, one of the configured hosts | The same, for one host |

Hosts come only from the `HOSTS` setting, never from the caller, so the probe
can't be pointed anywhere else.

### host-read

| Tool | Parameters | Returns |
|---|---|---|
| `top_processes` | `sort` (`cpu`, `memory`, `blocked`; default `cpu`), `by` (`process` or `group`; default `process`), `limit` (1–40, default 15) | The node's processes over five samples about two seconds apart: CPU cores, resident memory, state, how many samples found it blocked in uninterruptible I/O wait (state D), threads, and its group (`pod <namespace>/<name>`, `systemd <unit>`, `user session`, `kernel`, `other`). `by: group` sums per group. `blocked` lists only processes blocked at least once |

It runs in the host's PID namespace, in its own `familiar-host` namespace
(the only familiar namespace whose Pod Security level allows that), as a
non-root user with no capabilities, a read-only root filesystem, no hostPath
and no host network. Without ptrace rights the kernel lets it read only
`/proc/<pid>/stat` and `/cgroup`, which any user may: never command lines,
environments, open files, per-process I/O or another process's filesystem.

Processes are matched to pods by the pod UID in their cgroup path (static
pods by their config hash). Processes in `familiar-*` pods are dropped,
including the agent's own Kata VM. A process in a pod the index doesn't know
(created in the last minute) is dropped too, and if listing pods fails,
every pod process is dropped and the result says so: a process that might be
familiar's is never shown. Process names are chosen by the processes, so
they're cleaned like LAN client names (15 characters at most).

## Environment variables

| Variable | Server | Default | |
|---|---|---|---|
| `LISTEN` | all | `:8080` | MCP listen address |
| `PROMETHEUS_URL` | metrics-query, ceph-health | `http://kps-prometheus.monitoring.svc:9090` | Prometheus base URL |
| `BACKUP_METRIC` | metrics-query | `backup_last_success_timestamp_seconds` | The gauge your backup jobs write (a Unix timestamp per backup kind). Must be a bare metric name |
| `TARGET` | cert-probe | `traefik.traefik.svc:443` | Where to connect; each host is sent as the TLS server name |
| `HOSTS` | cert-probe | | Comma-separated hosts to probe |
| `PROC_ROOT` | host-read | `/proc` | Where procfs is mounted (tests) |
| `KUBE_DEV_PROXY_URL` | cluster-read, host-read | | Local testing only: use a `kubectl proxy` instead of the pod's ServiceAccount |

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
