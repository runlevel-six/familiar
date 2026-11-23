# A5: FQDN egress test

Proves that a CiliumNetworkPolicy can limit a pod to one external hostname, which is the core containment for model-gate (T1, T2; design §4.3). Re-run after every Cilium upgrade or config change.

```sh
export KUBECONFIG=~/.kube/admin.yaml   # an admin kubeconfig: the test creates a namespace and policies
kubectl apply -f test-pod.yaml -f policy-strict-dns.yaml
kubectl -n a5-egress-test wait --for=condition=Ready pod/probe
kubectl -n a5-egress-test get cnp fqdn-allowlist -o jsonpath='{.status.conditions}'   # must say Valid=True
./probe.sh
kubectl delete ns a5-egress-test
```

`policy.yaml` is the first, looser version (DNS for any name allowed); `policy-strict-dns.yaml` is the one to copy.

## Results (2026-11-22, Cilium 1.20.5)

| Step | `api.anthropic.com:443` | `example.com`, `github.com` | `1.1.1.1:443` (raw IP) | `api.anthropic.com:80` |
|---|---|---|---|---|
| No policy | reached | reached | reached | reached |
| Policy, `enable-l7-proxy=false` | reached | **reached** | **reached** | **reached** |
| Policy, L7 proxy on, DNS `matchPattern: "*"` | reached | blocked (timeout) | blocked | blocked |
| Strict DNS (`matchName` only), default `ndots:5` | **can't resolve** | blocked (DNS refused) | blocked | blocked |
| **Strict DNS + pod `ndots:1`** (final) | **reached** | **blocked (DNS refused)** | **blocked** | **blocked** |

## What we learned

1. **With the L7 proxy off, an FQDN policy fails open.** Cilium marked the policy `Valid=False` ("L7 policy is not supported since L7 proxy is not enabled") and enforced **nothing**; every destination stayed reachable. The only sign was the status condition. Fixed by enabling the L7 proxy (`helm upgrade cilium ... --reuse-values --set l7Proxy=true`, revision 12). It had been explicitly set to `false` in the release values; the chart default is `true`. This adds a `cilium-envoy` DaemonSet. The agent rollout took ~14 s; ingress stayed up and no pods were disrupted.
2. **The Cilium operator caches the old setting.** After enabling the proxy, enforcement was correct, but the policy kept the stale `Valid=False` status until `kubectl -n kube-system rollout restart deploy/cilium-operator`. After any Cilium config change, restart the operator too.
3. **Always check `Valid=True` after applying a policy**, and alert on invalid policies (follow-up: export CNP status through kube-state-metrics before Phase 1).
4. **Restrict DNS to exact names.** With `matchPattern: "*"`, the pod could resolve any name, which leaves DNS open as an exfiltration channel. Use `matchName` for each allowed name.
5. **Pods under strict DNS need `ndots:1`.** With the default `ndots:5`, the resolver tries search-domain variants first (`api.anthropic.com.<ns>.svc.cluster.local`). The DNS proxy refuses them, and musl-based resolvers give up before trying the real name. Also, `*` in `matchPattern` matches **one** DNS label, so `*.cluster.local` doesn't cover those variants. In-cluster names must then be written fully qualified (`model-gate.familiar-system.svc.cluster.local`) and listed with `matchName`.
6. **Raw-IP egress is blocked.** Only IPs learned through allowed DNS answers are reachable, and only on the allowed port.
