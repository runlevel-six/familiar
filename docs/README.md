# familiar documentation

familiar runs an always-on AI agent in a home Kubernetes cluster without trusting
it. The agent, GIDEON (Generally Intelligent Daemon, Exceptionally Organized &
Nosy), watches the cluster and talks to its owner over Matrix; it
holds no credentials, and every privileged action goes through a broker that
does. Phase 1, the read-only watch officer, has been running since 2026-11-23.

The pages follow [Diátaxis](https://diataxis.fr/), which sorts documentation by
what you need right now:

- **Tutorials** are lessons: follow them start to finish to learn by doing.
- **How-to guides** are steps for one task you already know you want to do.
- **Reference** is the exact facts: every setting, tool, check and alert, kept
  accurate to the code.
- **Explanation** is the background: why it works the way it does.

## Reading paths

### Try it

1. [Run the watch officer locally](tutorials/run-the-watch-officer-locally.md):
   the whole stack in Podman against your cluster, read-only. Check the
   isolation, ask the agent a question, and watch the brokers and the auditor
   record it.

### Understand the approach

1. [How familiar is built](explanation/architecture.md).
2. [Security model](explanation/security-model.md): the threats, and why the
   controls are structural.
3. [Network isolation](explanation/network-isolation.md), including what
   testing Cilium's FQDN policies taught.
4. [Budgets and model routing](explanation/budgets-and-model-routing.md).
5. [What the local Hermes test showed](explanation/local-test-findings.md).

### Run it on a cluster

1. [Verify FQDN egress](how-to/verify-fqdn-egress.md) first.
2. [Deploy the watch officer](how-to/deploy-phase-1.md).
3. [Manifests and policies](reference/manifests.md) and [Alerts](reference/alerts.md).
4. [Review and restore GIDEON's memory](how-to/review-and-restore-memory.md),
   weekly.
5. [Stop everything](how-to/stop-everything.md), when you need to.

### Change the code

1. [model-gate reference](reference/model-gate.md),
   [tool servers reference](reference/tool-servers.md) and
   [memory-auditor reference](reference/memory-auditor.md).
2. [Build and publish the images](how-to/build-and-publish-images.md).
3. [Keep site details out of the repository](how-to/keep-site-details-private.md).
4. [Design document](DESIGN.md): goals, threat model, the phased plan, and the
   decision log.

## All pages

| Page | Kind | What it covers |
|---|---|---|
| [Run the watch officer locally](tutorials/run-the-watch-officer-locally.md) | Tutorial | Read-only access, the Podman harness, isolation checks, a first question, the audit trail |
| [Deploy the watch officer](how-to/deploy-phase-1.md) | How-to | Prerequisites, an overlay, Secrets, applying, and verifying every boundary |
| [Verify FQDN egress](how-to/verify-fqdn-egress.md) | How-to | Prove a Cilium policy limits a pod to one hostname |
| [Review and restore GIDEON's memory](how-to/review-and-restore-memory.md) | How-to | Read memory-auditor's history; roll memory and jobs back |
| [Stop everything](how-to/stop-everything.md) | How-to | The kill switch, restarting, revoking the key |
| [Build and publish the images](how-to/build-and-publish-images.md) | How-to | Multi-arch builds with Podman, tags, pinning by digest |
| [Keep site details out of the repository](how-to/keep-site-details-private.md) | How-to | `local/`, the denylist and the pre-commit check |
| [model-gate](reference/model-gate.md) | Reference | Flags, configuration, every check, headers, metering, audit log, metrics |
| [Tool servers](reference/tool-servers.md) | Reference | Every tool and parameter, metrics-query's templates, environment, guardrails, protocol |
| [memory-auditor](reference/memory-auditor.md) | Reference | Settings, what it records and skips, metrics |
| [Manifests and policies](reference/manifests.md) | Reference | Files, site values, labels, what each network policy allows |
| [Alerts](reference/alerts.md) | Reference | Every alert rule and what it means |
| [How familiar is built](explanation/architecture.md) | Explanation | Components, the request flow, where the guardrails live |
| [Security model](explanation/security-model.md) | Explanation | The lethal trifecta, threats T1–T10, choices worth explaining |
| [Network isolation](explanation/network-isolation.md) | Explanation | Default deny, one-hostname egress, Cilium's quiet failure modes |
| [Budgets and model routing](explanation/budgets-and-model-routing.md) | Explanation | Three spend limits, which model does what, measured costs |
| [What the local Hermes test showed](explanation/local-test-findings.md) | Explanation | Findings from running the stack locally, including Matrix |
| [Public repository, private details](explanation/public-and-private.md) | Explanation | Why site details stay out, and how the history was published |
| [Design document](DESIGN.md) | Explanation | Goals, threat model, phases, decisions, open questions |
