# Alerts

Prometheus rules shipped with the Phase 1 manifests, as `PrometheusRule` objects
in the `monitoring` namespace. Alerts are read in Alertmanager and by an
off-cluster dead-man's switch that reports new **warning** and critical alerts;
**info** alerts are for review.

## model-gate

In [`deploy/familiar/10-model-gate.yaml`](../../deploy/familiar/10-model-gate.yaml).

| Alert | Severity | When |
|---|---|---|
| `ModelGateDown` | warning | model-gate hasn't been scraped for 10 minutes: down, or stopped with the kill switch |
| `ModelGateBudgetExhausted` | warning | A role's hourly or daily budget refused requests in the last 15 minutes. A runaway loop? |
| `ModelGateDailyBudgetHigh` | info | A role has spent over 80% of its daily budget |
| `ModelGatePolicyRejections` | warning | A role sent requests the gate refused for a policy reason in the last hour; check the audit log |
| `ModelGateUnpricedModel` | info | A response named a model with no configured price; add it |

## memory-auditor

In [`deploy/familiar/hermes/memory-auditor.yaml`](../../deploy/familiar/hermes/memory-auditor.yaml).

| Alert | Severity | When |
|---|---|---|
| `GideonControlStateChanged` | warning | The agent's scheduled jobs, config, or the inventory of other files changed |
| `GideonMemoryChanged` | info | The agent's memory changed. Expected; review weekly |
| `MemoryAuditorStale` | warning | No completed run for 15 minutes, or not scraped, for 10 minutes |
| `MemoryAuditorErrors` | warning | A run failed in the last 30 minutes |

## Hermes

In [`deploy/familiar/hermes/alerts.yaml`](../../deploy/familiar/hermes/alerts.yaml).
kube-prometheus-stack's own `KubePodCrashLooping` and
`KubeDeploymentReplicasMismatch` also cover Hermes; these add what they miss.
Scaling Hermes to zero fires none of them.

| Alert | Severity | When |
|---|---|---|
| `HermesDown` | warning | No available replica for 10 minutes, or the Deployment is gone |
| `HermesRestarting` | warning | More than 2 container restarts in an hour |
| `HermesIdle` | warning | No model call from the watch officer in 5 hours, once the pod has been up 5 hours. The 4-hourly sweep always calls a model, so the scheduler has stopped, or the jobs were never created |
