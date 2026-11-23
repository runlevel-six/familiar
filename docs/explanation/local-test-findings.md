# What the local Hermes test showed

Before anything runs in the cluster, the whole watch officer was run on a laptop
with Podman, isolated the way the cluster will isolate it, against the live
cluster through a read-only kubeconfig. ([Tutorial](../tutorials/run-the-watch-officer-locally.md).)
Two rounds on 2026-11-22 cost about $0.21 in total. This page records what they
found, because several findings changed the design, and producing that kind of
evidence is one of familiar's goals.

## Isolation held

From the Hermes container, model-gate and the four tool servers answered;
`api.anthropic.com`, other model providers, raw IPs, the Kubernetes API, the
ingress controller on the LAN and the host itself all failed. Hermes' only own
outbound attempts were a model-catalog fetch from its vendor's site and GitHub at
every start. It fails harmlessly, and must stay blocked in the cluster, because
the content is unpinned.

## Every model call went through model-gate

Including Hermes' auxiliary calls (the approval classifier, compression, titles):
all configured to use the same custom provider, all routed through the gate. No
placeholder key was needed: a provider without a key sends none, and the gate adds
it. Hermes also probes model-discovery endpoints meant for other server types
(`/api/tags`, `/props`, `/version`); the gate rejects them. Harmless noise.

## Things that had to change

- **`max_tokens`.** Hermes v0.23.3 always requests the model maximum (128,000)
  and no longer has a setting to lower it, so the gate rejected every call. Fix:
  the gate's `clamp_max_tokens` role option.
- **Container user.** The image starts as root and drops privileges with s6,
  which needs capabilities the restricted profile forbids. Started directly as
  uid 10000 with every capability dropped, it works; the pod sets
  `runAsUser`/`runAsGroup`/`fsGroup` 10000 and gives it a writable `/run`.
- **A tool gap.** `describe_workload` didn't return containers' resource requests
  and limits, so the agent couldn't see a CPU limit while triaging throttling.
  Added.
- **Turn cap.** Hermes allows unlimited model iterations per turn by default; the
  config now caps it at 30.
- **Secrets with a trailing newline** break the Matrix token ("Forbidden control
  character detected in headers"). Create Kubernetes Secrets with
  `--from-literal` or `printf %s`, never from a file ending in a newline.

## Locking Hermes down

Only the `memory`, `session_search` and `clarify` toolsets and the 14 MCP tools
are enabled. Disabling `skills` removes the agent's ability to write skills;
disabling `cronjob` stops it creating scheduled jobs. With `SOUL.md`, `scripts/`,
`hooks/`, `plugins/` and `skills/` mounted read-only, Hermes runs normally (it logs
a harmless warning per bundled skill it can't copy in). With a read-only root
filesystem it needs only writable `/run` and `/tmp`.

Hermes defers MCP tool schemas behind its own client-side `tool_search` (one
visible, fifteen deferred), so its base context with this toolset is about
9–10k tokens.

## Model routing works

A cron job pinned to Haiku through the gate runs on Haiku; conversations use the
default, Sonnet. A healthy sweep answered exactly `[SILENT]`. The alert gate
(the script that decides whether to wake the agent) made **zero** model calls with
only an info alert firing, woke Sonnet once for a new alert (4 calls, $0.03, good
triage), and made zero calls the next tick because the alert wasn't new.

Haiku and Sonnet both found the same real problem on the cluster, an application
database being CPU-throttled; Sonnet gave more context. See
[Budgets and model routing](budgets-and-model-routing.md) for the costs.

## Matrix

A local Synapse (federation off, registration off) stood in for the homeserver,
with a test client for the owner's phone and a second, non-allowlisted user.

- **End-to-end encryption works both ways.** Hermes joined the owner's encrypted
  DM, set up cross-signing (its recovery key written to its volume, never
  logged), and answered encrypted.
- **The allowlist works.** The other user's invite was rejected; Hermes never
  saw the message and made no model calls.
- **`MATRIX_E2EE_MODE=required` is not per-room enforcement.** It only means
  "refuse to start if crypto can't initialise". An allowlisted user's
  *unencrypted* room was answered in plaintext. The fix is on the server:
  `encryption_enabled_by_default_for_room_type: all` makes every room encrypted,
  and encryption can't be removed from a room.
- **Scheduled output** reaches the home room, encrypted; a `[SILENT]` sweep posts
  nothing. Create jobs with `--no-wrap-response`: the default footer tells the
  user to message the bot to manage the job, which it can't.
- Hermes starts a local API server on `127.0.0.1:8642` even when told not to; its
  own CLI talks to the gateway through it. Loopback in a pod is reachable only
  from that pod, so this is accepted.

- Hermes posts tool-progress lines ("⚙️ ceph_status…") into the chat before its
  answer. It's a display setting if it gets noisy.
- Not tested locally: the Matrix Authentication Service. The bot's token comes
  from it in the cluster, so that path is checked when the homeserver is
  deployed.

## The tool servers against the live cluster

Before the Hermes test, each tool server ran against the live cluster through a
read-only `kubectl proxy` (2026-11-22). cluster-read listed every pod and the
day's warning events with addresses redacted; metrics-query found the same
throttled database the agent later found; ceph-health reported a healthy cluster;
cert-probe checked every ingress host over MCP and refused a probe of the cloud
metadata address `169.254.169.254`. The live runs caught two bugs the unit tests
hadn't: a NaN from a zero-size filesystem broke JSON encoding, and calls to
unknown tools weren't audited.

## Known gap in the local harness

In the harness, Hermes can reach model-gate's metrics port. In the cluster, the
network policy allows only Prometheus there.
