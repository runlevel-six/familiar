# Local watch-officer test (Hermes + model-gate + tool servers)

Runs the Phase 1 watch officer on a Mac with Podman, isolated like the cluster
will isolate it, against the live cluster through the read-only kubeconfig.

```sh
./run.sh up      # build model-gate and tools images, start everything
./run.sh down    # remove containers (keeps volumes, images, secrets)
./run.sh purge   # also remove volumes, networks and the Podman secrets
```

Needs `~/.config/familiar/watch-officer.key` (the `familiar` workspace key,
mode 600) and a read-only kubeconfig (`deploy/access/readonly.yaml`). Site values
(`READONLY_KUBECONFIG`, `TRAEFIK`, `CERT_HOSTS`) come from the environment or
`local/tests/hermes-local.env`.
Both go into Podman secrets, never into an image or this repo.

## Layout

| Container | Networks | Notes |
|---|---|---|
| `hermes` | `fam-int` only (`--internal`: no internet, no host, no LAN) | Hermes v0.23.3 by digest, run like `deploy/familiar/hermes/deployment.yaml`: uid 10000, read-only root filesystem, all capabilities dropped, the same `config.yaml`, `SOUL.md` and `scripts/` from `deploy/familiar/hermes/`; `hooks/`, `plugins/`, `skills/` read-only (empty) |
| `model-gate` | `fam-int`, `fam-ext` | the only route to `api.anthropic.com`; key from a Podman secret |
| tool servers | `fam-int`, `fam-ext` | local builds; on `fam-int` they answer to their cluster DNS names (`*.familiar-tools.svc.cluster.local`, network aliases), as does model-gate; `cluster-read` uses `KUBE_DEV_PROXY_URL` |
| `kubeproxy` | `fam-ext` only | `kubectl proxy` with the read-only kubeconfig; Hermes can't reach it |

Known gap versus the cluster: Hermes can reach model-gate's metrics port
(read-only); the CNP allows only Prometheus there.

## Results, round 1 (2026-11-22)

Total spend: **$0.15** (25 model calls).

**Isolation.** From the Hermes container: model-gate and the four tool servers
answer; `api.anthropic.com`, OpenRouter, raw IPs, the Kubernetes API (proxy and
real), Traefik on the LAN and the Mac all fail. Hermes' own outbound attempts
were only its model-catalog fetch (`hermes-agent.nousresearch.com` and
`raw.githubusercontent.com/.../main/...model-catalog.json`), at every start;
it fails harmlessly and must stay blocked in the cluster (unpinned content).

**Routing (Q4, A1).** Every model call went through model-gate; no call to any
other provider, no keyless fallback. Hermes needs no placeholder key: a
custom provider with no key sends none, and the gate injects it. Hermes also
probes `/api/tags`, `/props`, `/version`, `/models` and similar on the gate
(model discovery for other server types); the gate rejects them (403), even
with `discover_models: false`. Harmless noise.

**max_tokens.** Hermes v0.23.3 always asks for the model maximum (128,000)
and no longer has a setting to lower it, so the gate rejected every call.
Fixed with the new `clamp_max_tokens` role option: the gate lowers it to the
cap (32,000) and logs `max_tokens_requested`.

**Per-job models (D20).** A cron job created with `--provider custom:model-gate
--model claude-haiku-5-5` runs on Haiku through the gate; conversations and
`-z` runs use the default (Sonnet). `[SILENT]` works: the healthy sweep
returned exactly `[SILENT]`. Hermes also prepends its own cron instructions
(no creating jobs from a run, `[SILENT]` rules).

**Alert gate (D20 tier 1).** `hermes/scripts/alert-gate.py` asks metrics-query
for firing alerts and prints `{"wakeAgent": false}` unless a non-info alert is
newly firing. Real gate with only an info alert firing: **0 model calls**.
Test copy counting info alerts: first tick woke Sonnet (4 calls, $0.031, good
triage), second tick **0 calls** (not new). Works with `scripts/` read-only;
state is kept in `cron/alert-gate-state.json`.

**Sweep cost (Q9).** Same sweep prompt, same cluster state:

| | Haiku 5.5 | Sonnet 5.5 |
|---|---|---|
| Cost per sweep | $0.0038–0.0043 | $0.091 |
| Model calls / tool calls | 4 / 13 | 6 / 19 |
| Found the real issue (an app database's CPU throttling) | yes | yes, more context |

Hermes' base context with this toolset is about 9–10k tokens. It defers MCP
tool schemas behind its `tool_search` (1 visible, 15 deferred, ~3k tokens).

**Lockdown (A1, Q7).** Enabled: `memory`, `session_search`, `clarify` and the
14 MCP tools. Disabling the `skills` toolset removes `skill_manage`;
disabling `cronjob` removes agent-created jobs. With `SOUL.md`, `scripts/`,
`hooks/`, `plugins/` and `skills/` read-only, Hermes runs normally; it logs a
"Failed to copy" warning per bundled skill at each start (it tries to seed
them into `skills/`). The MCP keys in `tools/hermes-mcp-servers.example.yaml`
(`tools.include`, `sampling`, `elicitation`) are accepted.

**Container user.** The image starts as root and drops to uid 10000 with
s6, which needs SETUID/SETGID/CHOWN. Started directly as uid 10000 with all
capabilities dropped it works (the entrypoint accepts the image's own
`hermes` user; any other UID is refused). For the cluster: `runAsUser: 10000`,
`runAsGroup: 10000`, `fsGroup: 10000`, a writable `/run` (emptyDir). Startup
warnings about `/run` ownership and `chgrp` are cosmetic.

**Tool gap found.** `describe_workload` doesn't return container resource
requests/limits, so triage of CPU throttling can't see the limit.

## Results, round 2: Matrix (2026-11-22)

Set up by hand (not in `run.sh` yet): Synapse v1.165.0 (`ghcr.io/element-hq/synapse`
by digest) on `fam-int` only, SQLite, server name `familiar.test`, client
listener only (no federation resource: the federation API returns 404),
`federation_domain_whitelist: []`, registration off, and
`encryption_enabled_by_default_for_room_type: all`. Users `jase` (stands in
for Jase's phone), `familiar` (the bot) and `mallory` (not allowlisted),
created with the registration shared secret. `matrix/testclient.py` is the
stand-in phone: an E2EE client on the Hermes image's mautrix + libolm.
Hermes gets `MATRIX_ACCESS_TOKEN` from a Podman secret, plus
`MATRIX_ALLOWED_USERS=@jase:familiar.test`, `MATRIX_E2EE_MODE=required`,
`MATRIX_DEVICE_ID=HERMES_BOT` and `MATRIX_HOME_ROOM`.

- **E2EE works both ways.** Hermes auto-joined Jase's encrypted DM,
  bootstrapped cross-signing (recovery key written to its volume, mode
  0600, never logged), decrypted the question and answered encrypted. A
  Sonnet question with two tool calls: about $0.037.
- **Allowlist.** Mallory's invite was rejected outright (logged); Hermes
  never joined, never saw the message; 0 model calls.
- **`MATRIX_E2EE_MODE=required` is not per-room enforcement.** It only
  means "refuse to start if crypto can't initialise". An allowlisted user's
  *unencrypted* room was joined and answered in plaintext (Hermes code:
  no per-room check; `MATRIX_ALLOWED_ROOMS` exempts DMs). **Fix on the
  server:** `encryption_enabled_by_default_for_room_type: all`. A client
  that explicitly asked for an unencrypted room got an encrypted one, and
  encryption can't be removed from a room; with federation off, every room
  is created on our server.
- **Cron delivery.** With `MATRIX_HOME_ROOM` set, the Haiku sweep returned
  `[SILENT]` and nothing was posted; a non-silent job posted, encrypted. Use
  `--no-wrap-response`: the default footer tells the user to message the
  bot to manage the job, which it can't (no `cronjob` toolset).
- **Turn cap.** Hermes defaults to unlimited iterations per turn;
  `agent.max_turns: 30` (+ `budget_warning_ratio: 0.8`) now applies.
- **API server.** Hermes starts its OpenAI-compatible API server on
  `127.0.0.1:8642` even with `API_SERVER_ENABLED=false`; in the image the
  local `hermes` CLI talks to the gateway through it, behind a generated
  key. Loopback in a pod is reachable only from the same pod: accepted.
- **Secrets with a trailing newline break the token** ("Forbidden control
  character detected in headers"). Create the Kubernetes Secret with
  `printf %s`/`--from-literal`, not from a file ending in a newline.
- Hermes posts tool-progress lines ("⚙️ ceph_status…") into the chat before
  answers. Fine for now; it's a display setting if it gets noisy.

**Read-only root filesystem** (2026-11-22): works with writable `/run` and `/tmp`. `hermes gateway status` (the pod's probe) exits 0 in about 2 s; it also prints "Hermes storage is unavailable … Read-only file system: SOUL.md" because Hermes tries to write a default `SOUL.md`. Cosmetic: everything works.

Not tested locally: MAS. The bot-token path in design D21 (a MAS
compatibility token for `@familiar`) is checked when ESS is deployed, since
the chart configures MAS itself.

Round 2 spend: about $0.05. Total for both rounds: about $0.21.
