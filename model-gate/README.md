# model-gate

The only familiar component that holds Anthropic API keys (design §4.1; D3, D4; threats T1, T6). Agents send **unauthenticated** Messages API requests to their role's port. model-gate checks each request, swaps in the role's key, forwards it to `api.anthropic.com`, meters the usage in USD, and writes one audit line per request.

Go standard library only, no third-party dependencies (D4: LiteLLM's supply chain was compromised twice in 2026). About 1,900 lines including tests.

**Status (2026-11-23):** written and tested locally (unit tests, an end-to-end run against a fake API, and the local Hermes test in `tests/hermes-local`). **Not deployed**: that's Phase 1, after the Phase 0 exit criteria. Image `ghcr.io/runlevel-six/model-gate:0.2.0-86b0d42` is built and pinned by digest in `deploy/familiar/10-model-gate.yaml`. 0.2.0 because the config changed incompatibly: `allowed_betas` is required and `allowed_server_tools` became `allowed_tool_types`.

## What it enforces

| Check | Result when it fails |
|---|---|
| Endpoint: only `POST /v1/messages`, `POST /v1/messages/count_tokens`, `GET /v1/models[/{id}]` | 403 `path_not_allowed` (Batches, Files, Skills, Managed Agents, Admin API are unreachable) |
| Body is unambiguous: valid UTF-8, no duplicate keys at any depth, and no key that differs only in case from a field the gate checks (`model`, `max_tokens`, `tools`, a tool's `type`, …). Go's JSON decoder matches keys case-insensitively and keeps the last duplicate, while the API may read the same bytes differently, so the gate would check one request and forward another | 400 `ambiguous_json` |
| `model` in the role's allowlist (also for `count_tokens`) | 403 `model_not_allowed` |
| `max_tokens` between 1 and the role's cap | 400 `max_tokens_over_cap` |
| Tools: only custom tools (no `type`, or `type: "custom"`) unless the role lists a type prefix in `allowed_tool_types`. An allowlist, so a server-side tool Anthropic adds later is denied by default. Tools that reach the network from Anthropic's side (`web_search*`, `web_fetch*`, `code_execution*`, `mcp_toolset`) get their own reason | 403 `server_tool_not_allowed` / `tool_type_not_allowed` |
| No `mcp_servers` (MCP connector) and no `container` (skills/code execution) | 403 |
| `fallbacks`: list entries must be allowlisted models; `"default"` only with `allow_default_fallbacks` | 403 |
| `anthropic-beta` values in the role's `allowed_betas` (required; `[]` allows none) | 403 `beta_not_allowed` |
| Hourly and daily USD budget not yet spent (fixed UTC windows; `count_tokens` and `models` are free) | **429** `rate_limit_error` (`hourly_budget_exhausted` / `daily_budget_exhausted`) |

**Why server-side tools are blocked:** web search, web fetch, code execution and the MCP connector make *Anthropic* fetch URLs or run code on the caller's behalf. That bypasses the cluster's egress policy entirely: a prompt-injected agent could read arbitrary URLs, or send data out, through the model API (T2, T9). The default is deny.

Rejections use the API's own error shape (`{"type":"error","error":{...}}`), so clients handle them like API errors, plus an `X-Model-Gate: rejected` header.

## Headers

- **Removed before forwarding:** `x-api-key`, `authorization`, cookies, proxy and `x-forwarded-*` headers, hop-by-hop headers, `accept-encoding` (so the gate can read responses).
- **Passed through:** `anthropic-version`, `anthropic-beta` (subject to the allowlist), `content-type`, other `anthropic-*` headers.
- **Set:** `x-api-key` with the role's key, read from `key_file` (a mounted Secret; never an env var).
- **Stripped from responses:** `set-cookie`.

The HTTP client ignores `HTTP(S)_PROXY` and doesn't follow redirects.

## Metering

- **Non-streaming:** `usage` from the response body.
- **Streaming (SSE):** parsed as the bytes pass through, unbuffered and flushed per chunk. Usage comes from `message_start` (`message.usage`) and `message_delta` (`usage`, cumulative), taking the maximum of each counter. A stream without `message_stop` is logged as `stream_incomplete` and still charged for what was reported.
- **Cost** = `input_tokens`×input + `output_tokens`×output + `cache_read_input_tokens`×cache read + cache writes (split into 5-minute and 1-hour TTLs via `usage.cache_creation` when present), per the price table (USD per million tokens). `input_tokens` is only the uncached remainder.
- **Long-context tier:** a price may carry `long_context` with `over_prompt_tokens` and a second set of rates. A request whose prompt (input + cache reads + cache writes) is over that threshold is charged entirely at those rates. Claude Haiku 5.5 is the one current model with a tier: over 100,000 prompt tokens it costs 5× as much.
- **Server-side fallbacks:** when `usage.iterations` is present, cost is summed per attempt at each attempt's model rate (top-level usage covers only the final attempt). Each attempt picks its own long-context tier. A model with no configured price is charged at the highest configured rate in each category, long-context tiers included, and counted in `model_gate_unpriced_total`.
- **Error responses** (4xx/5xx) aren't charged.
- **Enforcement** happens before forwarding, so a window can overshoot by one request; `max_tokens_per_request` bounds that. Spend persists in `state_file`, so restarting the gate doesn't reset budgets.

## Audit log and metrics

One JSON line per request on stdout: role, method, path, model, response model, stream, max_tokens, status, rejection reason or error, upstream `request-id`, input/output/cache tokens, iterations, cost, hour/day totals, latency. **No prompt or response bodies.** `debug_capture_dir` (per role, off by default) writes bodies to files for debugging only.

`metrics_listen` serves `/metrics` and `/healthz`:

- `model_gate_requests_total{role,outcome,status}`
- `model_gate_rejections_total{role,reason}`
- `model_gate_tokens_total{role,model,kind}`
- `model_gate_cost_usd_total{role}`
- `model_gate_unpriced_total{role,model}`
- `model_gate_budget_spent_usd{role,window}` and `model_gate_budget_limit_usd{role,window}`

Budget alerts are Prometheus rules (D17: seen in Alertmanager and by the laptop dead-man's switch). They're added with the Phase 1 deployment.

## Configuration

See `config.example.json`. Each role needs its own `listen` port. The CiliumNetworkPolicy decides which pods reach which port, so a caller can't claim another role by setting a header. Every allowlisted model must have a price. Run `model-gate -check -config …` to validate a config and its key files. `clamp_max_tokens: true` lowers an over-cap `max_tokens` to the cap instead of rejecting the request; Hermes v0.23 always asks for the model maximum (128k) and has no setting to lower it. Billing is per token produced, so clamping changes nothing for normal replies; it keeps the one-request budget overshoot bounded by the cap. The audit line records the original as `max_tokens_requested`.

### Budgets (Q9, decided 2026-11-22)

Pricing per million tokens (checked against platform.claude.com/docs/en/about-claude/pricing on 2026-11-23): Sonnet 5.5 $2 in / $10 out (cache read $0.10, 5-minute cache write $2.50); Haiku 5.5 $0.10 / $0.50 (cache read $0.01) for prompts up to 100,000 tokens, and $0.50 / $2.50 (cache read $0.05) above that.

Sweeps (15–30k tokens) stay in Haiku's base tier. Context compression also runs on Haiku and fires when a session nears Hermes' `context_length` (200,000), so those calls are billed at the higher tier, roughly $0.05–0.10 each. They're rare enough not to change the monthly estimate below.

A health sweep is roughly 5 to 7 model calls, each resending Hermes' context (system prompt, tool schemas, memory; assumed 15–30k tokens, **to be measured in the local Hermes test**). Calls inside a sweep hit the prompt cache, but the first call of each sweep rewrites it because the cache has expired since the previous sweep. Estimate: **$0.08–0.15 per sweep on Sonnet, $0.004–0.008 on Haiku**.

Routing (design D20): alert checks every 15 minutes cost nothing unless an alert is firing; health sweeps every 4 hours on Haiku; alert triage and conversations on Sonnet.

| Item | Expected per month |
|---|---|
| Haiku sweeps (180) | ~$1–1.50 |
| Sonnet alert triage (5–15 at $0.15–0.40) | ~$1–6 |
| Conversations (a few a week at $0.30–1.00) | ~$5–15 |
| **Total** | **~$7–22** |

| Role | Models | max_tokens | Hourly | Daily | Why |
|---|---|---|---|---|---|
| `watch-officer` (Phase 1) | Sonnet 5.5, Haiku 5.5 (no Opus) | 32,000 | **$1** | **$3** | A normal day is well under $1; $3 leaves room for a busy day. A runaway loop is stopped within the hour (T6) |
| `reader` (Phase 3) | Haiku 5.5, Sonnet 5.5 | 8,000 | $0.25 | $1 | Summarises feed items into a JSON schema; no reason for Opus or long outputs |

Plus the **Console workspace monthly spend limit, $42**, as the outer cap. Budget alerts fire at 80% of the daily cap. Recheck after the first week of Phase 1 with the `model_gate_cost_usd_total` data.

## Kill switch

`kubectl -n familiar-system scale deploy/model-gate --replicas=0` stops all model traffic (G4). Agents get connection errors; nothing else depends on it.

## Develop

```sh
go vet ./... && go test -race ./...
go build -o model-gate . && ./model-gate -check -config config.example.json   # needs key files
```

The tests run against a fake upstream (`httptest`). They cover key injection and header stripping, every rejection (and that rejected requests never reach the upstream), streaming pass-through with chunks split mid-line, budget exhaustion and window roll-over across restarts, fallback iteration costing, cache-write TTL pricing, and the metrics output.

## Open items before deployment

- ~~Which `anthropic-beta` values Hermes sends~~: done 2026-11-23 from Hermes v0.23.3's source (`agent/anthropic_adapter.py`). With an API key, a non-Azure base URL and fast mode off, it sends `interleaved-thinking-2025-05-14` and `fine-grained-tool-streaming-2025-05-14`, which `watch-officer` now allows. All its tools are custom (its `tool_search` is a client-side function, not Anthropic's tool search). Recheck when Hermes is upgraded: a new beta shows up as a `beta_not_allowed` rejection and the `ModelGatePolicyRejections` alert.
- Prices were checked against Anthropic's pricing page on 2026-11-23 (Haiku cache read corrected from $0.005 to $0.01; Haiku long-context tier added). Recheck before deploying if much time has passed.
- ~~Deployment, Service, CNPs and budget alert rules~~: done, `deploy/familiar/10-model-gate.yaml`.
