# Network isolation

The network is familiar's main containment. If the agent can't reach a
destination, no amount of prompt injection gets data there. Every familiar
namespace starts from default-deny for both ingress and egress, and each
component then gets exactly the connections it needs. The table of what's
allowed is in [Manifests and policies](../reference/manifests.md#what-each-policy-allows).

In short:

- **Hermes** reaches model-gate's role port, the four tool servers and the
  Matrix homeserver, and can resolve only those six names.
- **model-gate** is the only thing that reaches the internet, and only
  `api.anthropic.com` on port 443.
- **cluster-read** reaches the Kubernetes API; **metrics-query** and
  **ceph-health** reach Prometheus; **cert-probe** reaches the ingress
  controller.
- **memory-auditor** has no egress at all.
- **Nothing** in familiar reaches Alertmanager, whose unauthenticated API can
  create silences.

## Allowing one hostname

Limiting model-gate to `api.anthropic.com` uses Cilium's `toFQDNs` rules: Cilium
watches the pod's DNS answers and allows connections only to the addresses it
learned for permitted names. Testing that before relying on it turned up several
ways it fails quietly. Re-run the test after every Cilium upgrade or config change
([how-to](../how-to/verify-fqdn-egress.md)).

Results on Cilium 1.20.5 (2026-11-22):

| Setup | `api.anthropic.com:443` | other hosts | raw IP | `api.anthropic.com:80` |
|---|---|---|---|---|
| No policy | reached | reached | reached | reached |
| Policy, L7 proxy off | reached | **reached** | **reached** | **reached** |
| Policy, L7 proxy on, DNS allowed for any name | reached | blocked | blocked | blocked |
| DNS limited to exact names, default `ndots:5` | **can't resolve** | blocked | blocked | blocked |
| **DNS limited to exact names, pod `ndots:1`** | **reached** | **blocked** | **blocked** | **blocked** |

What that taught:

1. **With Cilium's L7 proxy off, an FQDN policy fails open.** Cilium marks the
   policy `Valid=False` and enforces *nothing*. The only sign is the status
   condition. The proxy has to be on (it adds a `cilium-envoy` DaemonSet).
2. **The Cilium operator caches the old setting.** After changing Cilium's
   configuration, restart `cilium-operator` too, or policy status stays stale.
3. **Always check `Valid=True` after applying a policy**, and alert on invalid
   ones.
4. **Allow DNS for exact names only** (`matchName`). Allowing any name leaves
   DNS open as a way to send data out.
5. **Pods under strict DNS need `ndots: 1`.** With the default `ndots:5`, the
   resolver first tries search-domain variants of every name; the DNS proxy
   refuses them, and musl-based resolvers give up before trying the real name.
   So in-cluster names are written fully qualified
   (`model-gate.familiar-system.svc.cluster.local`) and listed one by one.
6. **Raw-IP connections are blocked.** Only addresses learned from allowed DNS
   answers work, and only on the allowed ports.

## Beyond the cluster network

Two paths leave the cluster without crossing a network policy, and both are
closed elsewhere:

- **Anthropic's server-side tools** (web fetch, web search, code execution, the
  MCP connector) would make the API itself fetch URLs for the agent.
  model-gate refuses them, along with any tool type it doesn't know.
- **The messaging channel.** Matrix runs inside the cluster with federation
  off, so the agent's only external audience is the owner's own devices.
  Server-side settings make every room encrypted; Hermes' own "E2EE required"
  setting only means it refuses to start without crypto.

Hubble keeps flow logs for the familiar namespaces, so attempted connections are
visible after the fact.
