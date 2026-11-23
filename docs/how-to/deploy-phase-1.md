# Deploy the watch officer (Phase 1)

> **Not yet done on the author's cluster.** The manifests pass a strict
> server-side dry run and are restricted-PSS compliant, and the whole stack has
> run locally ([tutorial](../tutorials/run-the-watch-officer-locally.md)), but
> Phase 1 starts only after the Phase 0 prerequisites are met. Expect this page
> to change after the first real deployment.

## Before you start

The cluster needs:

- **Cilium with the L7 proxy on**, so FQDN egress policies are enforced. Check it
  with [Verify FQDN egress](verify-fqdn-egress.md) first: with the proxy off,
  the policies fail open.
- **kube-prometheus-stack** (Prometheus Operator CRDs, Prometheus in a
  `monitoring` namespace as `kps-prometheus`, kube-state-metrics). The tool
  servers query it and the manifests add ServiceMonitors and PrometheusRules.
- **cert-manager** and an ingress controller, for cert-probe.
- **A Matrix homeserver** reachable in the cluster (the manifests expect the
  Element Server Suite's `ess-synapse` Service in a `matrix` namespace), with a
  bot account and its access token. Enforce encryption on the server:
  `encryption_enabled_by_default_for_room_type: all`.
- **Kata Containers** with a `kata` RuntimeClass: Hermes runs in a VM. If your
  Cilium uses kube-proxy replacement, set `socketLB.hostNamespaceOnly=true`
  first: otherwise Cilium translates Service addresses only for connections made
  in the host's kernel, and a Kata VM, with its own kernel, can't reach any
  Service. Check with `cilium-dbg status --verbose` ("Socket LB Coverage:
  Hostns-only"); the `cilium-config` ConfigMap isn't reliable for this. Without
  Kata, remove `runtimeClassName` from `hermes/deployment.yaml`. Under Kata, a
  pod's memory limit is also its VM's RAM, so leave headroom above what the
  process uses.
- An Anthropic API workspace with a monthly spend limit and a key for the
  `watch-officer` role.

## Deploy

1. **Make an overlay.** The base in [`deploy/familiar`](../../deploy/familiar)
   holds placeholders for the hosts cert-probe checks, the Matrix IDs and the
   backup metric. Copy [`deploy/overlays/example`](../../deploy/overlays/example)
   somewhere outside this repository (or into `local/`), point `resources` at the
   base, and set your values. Check the result with `kubectl kustomize <overlay>`.

2. **Create the namespaces and Secrets.** Secrets never go in the repository.

   ```sh
   kubectl apply -f deploy/familiar/00-namespaces.yaml
   kubectl -n familiar-system create secret generic model-gate-keys \
     --from-file=watch-officer=<key file>
   kubectl -n familiar-agents create secret generic hermes-matrix \
     --from-literal=access-token="$(cat <token file>)"
   ```

   Use `--from-literal` for the Matrix token: a trailing newline breaks it.

3. **Apply the overlay**, never the base on its own:

   ```sh
   kubectl apply -k <overlay>
   ```

4. **Give the data volume to Hermes' user.** Kubernetes' `fsGroup` sets the
   volume's group, but a new RBD volume's root directory stays owned by root, and
   Hermes' CLI (`hermes cron`, `hermes -z`) then refuses to talk to its gateway
   ("unsafe_control_path … owned by uid 0"). Fix it once, as root, outside the
   restricted namespace: on the node,
   `sudo chown 10000 /var/lib/kubelet/pods/<hermes pod uid>/volumes/kubernetes.io~csi/<pv>/mount`.
   Only the top directory needs it; the volume keeps it until it's re-created.

5. **Create the scheduled jobs.** The agent can't create jobs itself (its
   `cronjob` toolset is off), so you do it:

   ```sh
   kubectl -n familiar-agents exec deploy/hermes -- hermes cron create … --no-wrap-response
   ```

   Pin health sweeps to Haiku (`--provider custom:model-gate --model
   claude-haiku-5-5`); see [Budgets and model routing](../explanation/budgets-and-model-routing.md).
   The 15-minute alert check runs `scripts/alert-gate.py` as its pre-run script.

## Verify

Don't trust an apply; check each boundary.

- **Policies are valid.** `kubectl get cnp -A` shows every familiar policy
  `Valid=True`. An invalid policy enforces nothing.
- **cluster-read's role is narrow.**
  `kubectl auth can-i --as=system:serviceaccount:familiar-tools:cluster-read -A …`
  says **no** to `get secrets`, `get configmaps`, `create pods`,
  `get pods --subresource=log` and `create pods --subresource=exec`, and **yes**
  to `list pods`. Write subresources with `--subresource`: `get pods/log` asks
  about a pod *named* "log", which `get pods` allows.
- **The agent's network is closed.** From a throwaway pod with the label
  `familiar/role: watch-officer` in `familiar-agents`: model-gate:8081 and the
  tool servers answer; `api.anthropic.com`, Alertmanager, Prometheus and any
  other name don't. Use the method in [Verify FQDN egress](verify-fqdn-egress.md).
- **model-gate reaches `api.anthropic.com` and nothing else**, and its metrics
  target is up in Prometheus.
- **memory-auditor sees what Hermes writes.** It reads the same volume from a
  second pod, read-only. Its log shows `baseline recorded`. Then plant a canary
  through Hermes' mount:

  ```sh
  kubectl -n familiar-agents exec deploy/hermes -- sh -c 'echo canary > /opt/data/home/auditor-canary'
  ```

  Within five minutes, `kubectl -n familiar-agents exec deploy/memory-auditor --
  git -C /audit/repo log -1 --stat` lists `files/inventory.tsv` and
  `GideonControlStateChanged` fires. Remove the file and check the next commit
  drops it. End to end: ask GIDEON in Matrix to remember something, approve it,
  and check a `memories` commit appears.
- **After moving Hermes to Kata, repeat the memory-auditor check.** Under Kata,
  Hermes sees the volume through the VM while the auditor reads the host's
  mount. First confirm Kata shares the volume over virtio-fs rather than passing
  the block device into the VM: two kernels mounting one filesystem would
  corrupt it.

## Notes from the first deployment (2026-11-23)

- Hermes' startup probe runs `hermes gateway status`, which takes about 2 s,
  longer in a VM: it needs a `timeoutSeconds` well above Kubernetes' 1 s default.
- Kata may report a probe kill as "OOMKilled" with exit code 0. Check the pod's
  `memory.events` before believing it.
- Hermes' side tasks (titles, compression) need `api_mode: anthropic_messages`
  in the provider entry, next to `transport`: their client reads only
  `api_mode`, and otherwise calls `/chat/completions`, which model-gate refuses.
  (`model.api_mode` doesn't help.)
- Hermes warns about low disk whenever under 2 GiB is free, so its volume is
  4 GiB.
- Growing `hermes-data` while memory-auditor mounts it read-only fails at the
  node ("requested read-only file system"). Scale memory-auditor to 0 until the
  PVC shows the new capacity, then back to 1.

## Afterwards

Phase 1 runs for at least two weeks before anything widens: review memory
changes weekly ([how-to](review-and-restore-memory.md)), check spend against the
estimate, and run the red-team test from the [design](../DESIGN.md) (a prompt
injection planted in a log line).
