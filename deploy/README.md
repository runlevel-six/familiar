# deploy

Manifests for everything familiar depends on, applied by hand with `kubectl` and `helm` (no GitOps controller yet; Tier B). This repo is the source of truth for them (A7): change the files here, then apply.

| Directory | What |
|---|---|
| [familiar](familiar/) | Phase 1, as a generic kustomize base: model-gate, tool servers, Hermes, memory-auditor, policies (draft, not applied) |
| [overlays/example](overlays/example/) | How a site sets its own values (cert-probe hosts, Matrix IDs) on top of the base |
| [tests](tests/) | Throwaway tests for Phase 0 prerequisites (A5 FQDN egress; A6 Kata to follow) |
| [access](access/) | The read-only kubeconfig ServiceAccount |

Site-specific material (the real overlay, the monitoring stack's values and ingress, the Matrix homeserver, app database dumps, host backup scripts, runbooks, the cluster inventory and incident notes) lives in the gitignored `local/` directory: see `docs/public-repo.md`.

## Rules (design §4.2, T4)

- **Only Jase can push or merge.** As of 2026-11-22 the repo is private with no collaborators, deploy keys, webhooks or app installations. GitHub doesn't offer branch protection on free private repos; once the repo is public, protect `main` (require PRs, no force push).
- **No familiar component gets a token for this repo.** The Phase 2 `propose-change` token must be scoped to a different repo (the app manifests), never this one, because this repo holds the guardrails.
- **No secrets and no site details in this repo.** It will be public. Secrets are created on the cluster and listed by name only; hostnames, addresses and the like go in `local/` (`scripts/check-private.sh` enforces it).
- Claude Code (run by Jase on his laptop) may push here under Jase's account; that's Jase acting, not the agent.
