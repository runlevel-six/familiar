# familiar tool servers

Four **read-only** MCP tool servers for the Phase 1 watch officer (design §4.1). Go standard library only. They **hold the credentials, enforce the policy, log every call, and return redacted summaries**; the agent holds nothing.

**Status (2026-11-23):** written and tested (unit tests; live read-only runs against the home cluster; an end-to-end MCP run over HTTP). Image `ghcr.io/runlevel-six/familiar-tools:0.2.1-f0562ac` (2026-11-23: `describe_workload` resources, cluster-read hides `familiar-*`, no site names; metrics-query's backup metric set by `BACKUP_METRIC`), pinned by digest. **Not deployed**: Phase 1. Manifests: `deploy/familiar/20-tool-servers.yaml`.

| Server | Credential | Tools |
|---|---|---|
| `cluster-read` | Narrow ClusterRole: get/list on pods, events, nodes, workloads, storage, ingresses, cert-manager Certificates. **Never** Secrets, ConfigMaps, `pods/log`, exec, RBAC or policies. familiar's own namespaces (`familiar-*`) are hidden: asking for one is an error, and their objects are filtered out of every list, below the handlers (`scope.go`; T3) | `cluster_overview`, `list_pods`, `describe_workload`, `recent_events`, `storage_status`, `certificates`, `ingresses` |
| `metrics-query` | None (Prometheus over the network) | `query_metric` (14 allowlisted templates with validated parameters; **no arbitrary PromQL**), `metric_history` |
| `ceph-health` | None (Ceph metrics already in Prometheus; **no keyring**) | `ceph_status`, `ceph_osds`, `ceph_pools` |
| `cert-probe` | None (TLS to Traefik) | `probe_all_certificates`, `probe_certificate`, over a **fixed host list** (no caller-supplied hosts, so no SSRF) |

## Protocol

`internal/mcp` implements MCP over Streamable HTTP at `/mcp`, **dual-era**:

- **Modern, 2026-07-28** (current spec): stateless. Each request carries `_meta["io.modelcontextprotocol/protocolVersion"]`. The `MCP-Protocol-Version`, `Mcp-Method` and `Mcp-Name` headers must match the body (otherwise 400 with `-32020` HeaderMismatch; base64-sentinel names decoded). An unsupported version gets 400 with `-32022` and the list of supported versions; an unknown method gets 404 with `-32601`. Implements `server/discover`; results carry `resultType: "complete"`.
- **Legacy, 2025-03-26 to 2025-11-25**: the `initialize` handshake, answered with the requested (or newest legacy) version. Served statelessly: no `Mcp-Session-Id` is minted.
- **Both:** POST only (GET/DELETE get 405), no batches, notifications get 202, and a request with a foreign `Origin` header gets 403 (DNS rebinding). Only the `tools` capability is offered. Every tool is annotated `readOnlyHint: true`, tools are listed in a fixed order (helps prompt caching), and each result is returned as `structuredContent` plus a JSON text block.

## Guardrails in every server

- **Redaction** (`internal/redact`) of every string in every result, keys included: Anthropic, AWS and GitHub keys, JWTs, bearer tokens, private keys, `password=`-style pairs, credentials in URLs, email addresses, and IPv4/IPv6 addresses. It also strips invisible characters that could hide instructions (Unicode tags, bidi controls, zero-width characters) and truncates long strings. Results over 256 KiB are refused, with a hint to narrow the query.
- **Audit:** one JSON log line per call, including rejected and unknown-tool calls: tool, arguments (redacted), client, remote address, protocol version, outcome, result size, duration (T10).
- **Rate limits:** 30 calls/min for metrics-query and ceph-health, 20 for cert-probe, 60 for cluster-read.
- **Strict arguments:** unknown fields are errors, and namespaces and names are validated against Kubernetes naming rules before any API call. Tool problems come back as `isError` results the model can correct; internal errors don't leak details.
- **Untrusted content** (event messages, names) is labelled as data, not instructions, in the tool descriptions (T9).

## Tests

```sh
go vet ./... && go test -race ./...                    # unit tests, fake APIs
kubectl proxy --port=18001 &                            # live, read-only:
LIVE_KUBE_PROXY=http://127.0.0.1:18001 go test -tags live -run Live -v ./cmd/cluster-read/
LIVE_PROM_URL=http://127.0.0.1:18001/api/v1/namespaces/monitoring/services/kps-prometheus:9090/proxy \
  go test -tags live -run Live -v ./cmd/metrics-query/ ./cmd/ceph-health/
```

**Live results (2026-11-22):**
- **cluster-read:** 71 pods, 0 unhealthy; 7 warning events in 24 h, with IPs redacted.
- **metrics-query:** 0.61 cores used, 8.38 of 12 requested; it found an app database's CPU throttling.
- **ceph-health:** HEALTH_OK, 3/3 monitors, 2/2 OSDs, 33/33 PGs clean.
- **cert-probe:** run as a binary over MCP HTTP against Traefik: all 23 ingress hosts valid, 33 to 76 days left. A probe of `169.254.169.254` was refused.

Bugs the live runs caught, now fixed:
- A NaN from a 0-size filesystem broke JSON encoding.
- Unknown-tool calls weren't audited.

## Hermes

`hermes-mcp-servers.example.yaml` holds the `mcp_servers` block: URLs, a `tools.include` allowlist per server, and sampling and elicitation off (Q5). Check the key names against the Hermes version you deploy.

## Build

`docker build --build-arg VERSION=… -t ghcr.io/runlevel-six/familiar-tools:… .` produces one image with `/cluster-read`, `/metrics-query`, `/ceph-health` and `/cert-probe`. Pin the digest in the manifests.
