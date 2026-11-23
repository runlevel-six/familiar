# familiar tool servers

Four read-only MCP servers for the watch officer: `cluster-read`,
`metrics-query`, `ceph-health` and `cert-probe`. They hold the credentials,
enforce the policy, log every call and return redacted summaries; the agent holds
nothing. Go standard library only; one image with four binaries.

- [Reference](../docs/reference/tool-servers.md): every tool and parameter, the
  query templates, environment variables, guardrails and protocol.
- [`hermes-mcp-servers.example.yaml`](hermes-mcp-servers.example.yaml): the
  `mcp_servers` block for Hermes.

## Develop

```sh
go vet ./... && go test -race ./...     # unit tests against fake APIs
```

Live, read-only tests against a cluster, through `kubectl proxy` with a
read-only kubeconfig:

```sh
kubectl proxy --port=18001 &
LIVE_KUBE_PROXY=http://127.0.0.1:18001 go test -tags live -run Live -v ./cmd/cluster-read/
LIVE_PROM_URL=http://127.0.0.1:18001/api/v1/namespaces/monitoring/services/kps-prometheus:9090/proxy \
  go test -tags live -run Live -v ./cmd/metrics-query/ ./cmd/ceph-health/
```

Images: [Build and publish the images](../docs/how-to/build-and-publish-images.md).
