# Stop everything

The kill switch (goal G4 in the [design](../DESIGN.md)). Use it when the agent
does something unexpected, spend climbs, or you just want it quiet.

## Stop all model traffic

```sh
kubectl -n familiar-system scale deploy/model-gate --replicas=0
```

model-gate holds the only API key, so with it gone no familiar component can
reach a model. Hermes gets connection errors and stops doing anything useful;
nothing else depends on model-gate. `ModelGateDown` fires after ten minutes, as a
reminder that it's off.

## Stop the agent too

```sh
kubectl -n familiar-agents scale deploy/hermes --replicas=0
```

This also stops its scheduled jobs and its Matrix connection. Scaling to zero
fires none of the Hermes liveness alerts, so this is quiet by design.

Leave memory-auditor running: it keeps the record of what the agent last
changed. If you think the agent's memory or jobs were tampered with, continue
with [Review and restore GIDEON's memory](review-and-restore-memory.md) before
starting it again.

## Start again

```sh
kubectl -n familiar-system scale deploy/model-gate --replicas=1
kubectl -n familiar-agents scale deploy/hermes --replicas=1
```

## Revoke the key

If the API key itself may be exposed, scaling down isn't enough: revoke the key in
the Anthropic Console, create a new one, and replace the Secret:

```sh
kubectl -n familiar-system create secret generic model-gate-keys \
  --from-file=watch-officer=<new key file> --dry-run=client -o yaml | kubectl apply -f -
kubectl -n familiar-system rollout restart deploy/model-gate
```
