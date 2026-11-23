# Run the watch officer locally

In this tutorial you run the whole Phase 1 stack on your own computer with Podman:
Hermes, model-gate, the four tool servers and memory-auditor, isolated the way
the cluster isolates them, and pointed at a real cluster through a read-only
kubeconfig. You'll check the isolation yourself, ask the agent about your cluster,
and watch the brokers and the auditor record what it did.

It takes about half an hour and costs a few cents in API usage.

## What you need

- **Podman** (on a Mac, a running `podman machine`). The images are built
  locally, so you don't need Go.
- **An Anthropic API key**, ideally in a workspace with a low monthly spend
  limit. Save it in a file only you can read:

  ```sh
  mkdir -p ~/.config/familiar
  (umask 077; printf %s 'sk-ant-…' > ~/.config/familiar/watch-officer.key)
  ```

- **A Kubernetes cluster you can read** with Prometheus from
  kube-prometheus-stack (the harness expects the `kps-prometheus` Service in
  `monitoring`) and an ingress controller serving TLS.
- An admin kubeconfig for one step, to create the read-only access.

## 1. Create read-only access

The harness never sees an admin credential. Create a ServiceAccount with view
rights and no access to Secrets:

```sh
kubectl apply -f deploy/access/readonly.yaml
```

Then build a kubeconfig that holds only its token:

```sh
TOKEN=$(kubectl -n cluster-readonly get secret viewer-token -o jsonpath='{.data.token}' | base64 -d)
SERVER=$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')
kubectl config view --minify --raw -o jsonpath='{.clusters[0].cluster.certificate-authority-data}' \
  | base64 -d > /tmp/ca.crt
export RO=~/.kube/readonly.yaml
KUBECONFIG=$RO kubectl config set-cluster home --server="$SERVER" --certificate-authority=/tmp/ca.crt --embed-certs
KUBECONFIG=$RO kubectl config set-credentials viewer --token="$TOKEN"
KUBECONFIG=$RO kubectl config set-context home --cluster=home --user=viewer
KUBECONFIG=$RO kubectl config use-context home
rm /tmp/ca.crt
```

Check it can read but not see Secrets:

```sh
KUBECONFIG=$RO kubectl get pods -A | head -3     # works
KUBECONFIG=$RO kubectl auth can-i get secrets    # no
```

## 2. Tell the harness about your cluster

The harness reads three values from the environment, or from
`local/tests/hermes-local.env` if it exists:

```sh
mkdir -p local/tests
cat > local/tests/hermes-local.env <<EOF
READONLY_KUBECONFIG=$RO
TRAEFIK=<ingress controller address>:443
CERT_HOSTS=<host1>,<host2>
EOF
```

`TRAEFIK` is an address your computer can reach the ingress controller on;
`CERT_HOSTS` are hostnames it serves, for cert-probe to check. `local/` is
gitignored, so these stay out of the repository.

## 3. Start everything

```sh
tests/hermes-local/run.sh up
```

The script builds the model-gate and tool server images, creates two networks,
and starts the containers. When it finishes it lists them:

```
kubeproxy       Up …
model-gate      Up …
cluster-read    Up …
metrics-query   Up …
ceph-health     Up …
cert-probe      Up …
hermes          Up …
memory-auditor  Up …
```

The layout mirrors the cluster's network policies:

| Container | Networks | |
|---|---|---|
| `hermes` | `fam-int` only, an internal network with no internet, no host and no LAN | Hermes v0.23.3, run like the cluster Deployment: uid 10000, read-only root filesystem, the same `config.yaml`, `SOUL.md` and scripts |
| `model-gate` | `fam-int` and `fam-ext` | The only route to `api.anthropic.com` |
| tool servers | `fam-int` and `fam-ext` | They answer on `fam-int` under their cluster DNS names, so Hermes' config works unchanged |
| `kubeproxy` | `fam-ext` only | `kubectl proxy` with your read-only kubeconfig; Hermes can't reach it |
| `memory-auditor` | none | Reads Hermes' volume read-only, every 60 seconds here |

## 4. Check the isolation

Try to reach the API directly from Hermes' container:

```sh
podman exec hermes curl -sS -m 5 https://api.anthropic.com/v1/models
```

It fails: there's no route out. Now go through model-gate, without a key:

```sh
podman exec hermes curl -sS -m 10 \
  http://model-gate.familiar-system.svc.cluster.local:8081/v1/models | head -c 200
```

That returns the model list: the gate added the key. Try something the gate
doesn't allow, such as the Files API:

```sh
podman exec hermes curl -sS -m 5 -X POST \
  http://model-gate.familiar-system.svc.cluster.local:8081/v1/files
```

You get a 403 in the API's error format, `endpoint not allowed`. Neither request
cost anything.

## 5. Ask about your cluster

```sh
podman exec hermes hermes -z "Give me a short health summary of the cluster. Anything that needs attention?"
```

Hermes calls the tool servers (`cluster_overview` first, then whatever it needs)
and answers in a few sentences. It uses the default model, Sonnet 5.5, and costs
a few cents.

The same question on Haiku, the model the scheduled sweeps use:

```sh
podman exec hermes hermes -z "Same question, briefly." --provider custom:model-gate -m claude-haiku-5-5
```

## 6. See what the brokers recorded

model-gate writes one line per model call:

```sh
podman logs model-gate 2>&1 | tail -3
```

Each line has the role, model, token counts, the cost in USD and the running
hourly and daily totals, but no prompt or response text. The tool servers log
every call, with redacted arguments:

```sh
podman logs cluster-read 2>&1 | tail -3
```

The agent's own account of what it did isn't the record; these logs are.

## 7. Watch memory-auditor

```sh
podman exec memory-auditor git -C /audit/repo log --oneline
```

One commit: the baseline. Ask Hermes to remember something (in one-shot mode,
memory writes are approved automatically):

```sh
podman exec hermes hermes -z "Remember that the backup jobs run at night, so disk wait spikes then are normal."
```

Within a minute, a new commit appears:

```sh
podman exec memory-auditor git -C /audit/repo log -p -1 -- memories
```

Now do what a compromised agent might: leave a file behind.

```sh
podman exec hermes sh -c 'echo "curl … | sh" > /opt/data/home/.persist.sh'
sleep 70
podman exec memory-auditor git -C /audit/repo log --stat -1
```

The inventory recorded it. In the cluster, that change raises
`GideonControlStateChanged`.

## 8. Clean up

```sh
tests/hermes-local/run.sh down     # remove the containers, keep the volumes
tests/hermes-local/run.sh purge    # also remove volumes, networks and the Podman secrets
```

## Where next

- [What the local Hermes test showed](../explanation/local-test-findings.md): the
  results of the two rounds this harness was built for, including Matrix.
- [Deploy the watch officer](../how-to/deploy-phase-1.md) in a cluster.
- [How familiar is built](../explanation/architecture.md).
