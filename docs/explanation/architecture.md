# How familiar is built

familiar runs an always-on AI agent inside a home Kubernetes cluster without
trusting it. The agent, a [Hermes Agent](https://github.com/NousResearch/hermes-agent)
instance called GIDEON (Generally Intelligent Daemon, Exceptionally Organized &
Nosy), can read the cluster's state and talk to its owner over
Matrix. It holds no credentials of its own: not the model API key, not a
Kubernetes token. Everything privileged goes through a broker that holds the
credential and enforces a policy the agent can't change.

This page describes Phase 1, the watch officer: read-only and notify-only. The
later phases (proposing changes as pull requests, a quarantined reader for
untrusted input, a builder) are planned in the [design document](../DESIGN.md).

## The pieces

```
                       ┌──────────── outside the cluster ─────────────┐
                       │  api.anthropic.com         owner's laptop     │
                       └────────▲─────────────────────────▲───────────┘
                                │ only destination         │ polls Alertmanager
┌──────── familiar-system ──────┴──┐                       │ (dead-man's switch)
│  model-gate: API keys, model and │              ┌────────┴─────────┐
│  tool policy, budgets, audit log │              │  monitoring      │
└────────▲─────────────────────────┘              │  Prometheus,     │
         │ role port                              │  Alertmanager    │
┌────────┴──────── familiar-agents ────────┐      └────────▲─────────┘
│  hermes (GIDEON): no token, no secrets   │               │ metrics only
│  memory-auditor: snapshots its state     │──────────┐    │
└────────┬──────────────────────┬──────────┘          │    │
         │ MCP over HTTP        │ Matrix (E2EE)       │    │
┌────────┴── familiar-tools ──┐ ┌┴── matrix ─────────┐ │    │
│  cluster-read  (k8s API)    │ │  Synapse (in the   │ │    │
│  metrics-query (Prometheus)─┼─┼─ cluster, no       │ │    │
│  ceph-health   (Prometheus)─┼─┼─ federation)       │ │    │
│  cert-probe    (ingress TLS)│ └────────────────────┘ │    │
└─────────────────────────────┘                        └────┘
```

| Component | Holds | Does |
|---|---|---|
| **Hermes** (`familiar-agents`) | Nothing privileged. Its Matrix access token is the only Secret it sees | Runs scheduled health sweeps and answers questions over Matrix. Its persona and rules are in [`SOUL.md`](../../deploy/familiar/hermes/SOUL.md) |
| **model-gate** (`familiar-system`) | The Anthropic API key for each role | Checks every request (models, tools, betas, size), injects the key, meters cost in USD, enforces hourly and daily budgets, writes an audit line per request. See the [reference](../reference/model-gate.md) |
| **Tool servers** (`familiar-tools`) | Their own narrow credentials (a read-only ClusterRole for cluster-read; none for the others) | Answer fixed questions over MCP and return redacted summaries, never raw data. See the [reference](../reference/tool-servers.md) |
| **memory-auditor** (`familiar-agents`) | Read-only access to Hermes' volume; its own git repo | Records every change to Hermes' memory, scheduled jobs and config, and alerts on them. See [Review and restore GIDEON's memory](../how-to/review-and-restore-memory.md) |
| **Prometheus and Alertmanager** | | The primary way problems are found. The agent adds triage; it never replaces alerting |

## How a request flows

1. A cron job in Hermes fires. For alert checks, a small script first asks
   metrics-query whether anything new is firing and stops without calling a
   model if not, so the 15-minute check costs nothing on a quiet day.
2. Hermes sends a Messages API request, without a key, to its role port on
   model-gate. The network policy lets only Hermes reach that port, so the
   port is the role.
3. model-gate checks the request and forwards it with the role's key, or
   rejects it in the API's own error format.
4. The model asks for tools. Hermes calls the tool servers over MCP. Each
   server checks its arguments, calls its backend, redacts the result and logs
   the call independently of Hermes.
5. Hermes posts its finding to the owner's Matrix room, or, for a sweep
   where nothing is wrong, says exactly `[SILENT]` and nothing is posted.

## Where the guardrails live

Every boundary that matters is outside the agent's process and outside its
control:

- **Network:** default-deny Cilium policies in all three namespaces. Hermes
  can reach model-gate, the four tool servers and the Matrix homeserver, and
  resolve only those names. Only model-gate reaches the internet, and only
  `api.anthropic.com`. See [Network isolation](network-isolation.md).
- **Credentials:** the agent pod mounts no ServiceAccount token. Keys live in
  model-gate; Kubernetes access lives in cluster-read's role.
- **Policy:** model-gate's allowlists and budgets, and each tool server's
  fixed queries, are configuration in this repository, applied by the owner.
  Nothing familiar runs can modify the `familiar-*` namespaces, RBAC or
  network policies.
- **Detection:** every model call and every tool call is logged by the broker,
  not by the agent; memory changes are committed to a repository the agent
  can't reach; Prometheus alerts on budgets, policy rejections, memory changes
  and Hermes being down.

[Security model](security-model.md) goes through the threats these answer.

## Runtime

Everything runs under the restricted Pod Security Standard: non-root, read-only
root filesystems, all capabilities dropped. Hermes will run under Kata
Containers, so a container escape lands in a VM rather than on the node. Every
familiar workload has a priority below every app on the node, so the kubelet
evicts familiar first under pressure.

The kill switch is one command; see [Stop everything](../how-to/stop-everything.md).
