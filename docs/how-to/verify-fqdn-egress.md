# Verify FQDN egress

Proves that a CiliumNetworkPolicy can limit a pod to one external hostname, the
core containment for model-gate. Run it before Phase 1 and again after every
Cilium upgrade or configuration change. Why each step matters is in
[Network isolation](../explanation/network-isolation.md).

You need an admin kubeconfig: the test creates a namespace and policies. The
files are in [`deploy/tests/a5-fqdn`](../../deploy/tests/a5-fqdn).

```sh
cd deploy/tests/a5-fqdn
export KUBECONFIG=<admin kubeconfig>
kubectl apply -f test-pod.yaml -f policy-strict-dns.yaml
kubectl -n a5-egress-test wait --for=condition=Ready pod/probe
kubectl -n a5-egress-test get cnp fqdn-allowlist -o jsonpath='{.status.conditions}'
./probe.sh
kubectl delete ns a5-egress-test
```

## What to look for

- The policy's condition says **`Valid=True`**. If it says `Valid=False` with "L7
  policy is not supported since L7 proxy is not enabled", the policy is enforcing
  nothing: enable Cilium's L7 proxy, then restart `cilium-operator` and check
  again.
- `probe.sh` reaches **`api.anthropic.com:443`** and nothing else: other hosts fail
  at DNS, a raw IP and `api.anthropic.com:80` time out.
- If even `api.anthropic.com` can't be resolved, the pod is missing
  `dnsConfig.options ndots: "1"`.

`policy.yaml` is an earlier, looser version that allows DNS for any name; keep it
for comparison, copy `policy-strict-dns.yaml`.
