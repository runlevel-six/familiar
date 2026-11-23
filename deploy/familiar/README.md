# familiar Phase 1 manifests

A generic kustomize base for the watch officer: namespaces, quotas, default-deny
policies, model-gate, the tool servers, Hermes and memory-auditor, with their
network policies, ServiceMonitors and alert rules. Site values (cert-probe's
hosts, Matrix IDs, the backup metric) are placeholders here and set by an overlay;
see [`../overlays/example`](../overlays/example).

Applied on the author's cluster on 2026-11-23, with Hermes under Kata.

- [Deploy the watch officer](../../docs/how-to/deploy-phase-1.md)
- [Manifests and policies](../../docs/reference/manifests.md): what each file and
  policy does
- [Alerts](../../docs/reference/alerts.md)
- [Review and restore GIDEON's memory](../../docs/how-to/review-and-restore-memory.md)
