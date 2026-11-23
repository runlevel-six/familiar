# model-gate reference

model-gate is the only familiar component that holds Anthropic API keys. Agents
send unauthenticated Messages API requests to their role's port; the gate checks
each request, swaps in the role's key, forwards it to the API, meters the cost and
writes one audit line. Source: [`model-gate/`](../../model-gate). Go standard
library only.

## Command line

| Flag | Default | |
|---|---|---|
| `-config` | `/etc/model-gate/config.json` | The JSON configuration file |
| `-check` | off | Validate the configuration and read the key files, then exit |

## Configuration

A JSON file; unknown fields are errors. Example:
[`model-gate/config.example.json`](../../model-gate/config.example.json).

### Top level

| Field | Default | |
|---|---|---|
| `upstream` | `https://api.anthropic.com` | The API base URL. Must be `https` |
| `metrics_listen` | | Address for `/metrics` and `/healthz`. Keep agents away from it with network policy |
| `state_file` | | Where spend is persisted, so a restart doesn't reset the budget windows. Optional, recommended |
| `max_request_bytes` | 32 MiB | Largest accepted request body |
| `max_response_bytes` | 64 MiB | Largest buffered non-streaming response |
| `prices` | | Rates per model ID; see [Prices](#prices). Every model a role allows needs one |
| `roles` | | One or more [roles](#roles) |

### Roles

Each role is one caller identity with its own listening port. The network policy
decides which pods can reach which port, so a caller can't claim another role.

| Field | Required | |
|---|---|---|
| `name` | yes | Unique role name, used in logs and metrics |
| `listen` | yes | Address the role listens on, e.g. `:8081`. Unique, and not `metrics_listen` |
| `key_file` | yes | File holding the role's API key (a mounted Secret). Keys never come from environment variables |
| `models` | yes | Exact model IDs the role may request; also applies to fallback models |
| `max_tokens_per_request` | yes | Highest `max_tokens` allowed |
| `clamp_max_tokens` | | Lower an over-cap `max_tokens` to the cap instead of rejecting the request. The original value is logged as `max_tokens_requested` |
| `hourly_usd`, `daily_usd` | | Budgets per fixed UTC hour and day. `0` disables a window |
| `allowed_betas` | yes | Exact `anthropic-beta` values passed through; anything else is rejected. `[]` allows none |
| `allowed_tool_types` | | Tool `type` prefixes allowed besides custom tools, e.g. `"bash_"`. Empty means custom tools only |
| `allow_default_fallbacks` | | Allow `fallbacks: "default"`, where the API picks the fallback model, possibly outside `models` |
| `debug_capture_dir` | | Write request and response bodies here. Off by default: bodies are prompts and may contain cluster data |

### Prices

USD per million tokens. Check them against
[Anthropic's pricing page](https://platform.claude.com/docs/en/about-claude/pricing)
before deploying; they were last checked on 2026-11-23.

| Field | |
|---|---|
| `input`, `output` | Uncached input and output |
| `cache_read` | Cache hits |
| `cache_write_5m`, `cache_write_1h` | Cache writes with a 5-minute or 1-hour lifetime |
| `long_context` | Optional second rate card: `over_prompt_tokens` plus the same five rates. A request whose prompt is over the threshold is charged entirely at these rates |

## What it checks

In order. Rejections use the API's own error format (`{"type":"error","error":{…}}`)
and carry an `X-Model-Gate: rejected` header.

| Check | On failure |
|---|---|
| Endpoint is `POST /v1/messages`, `POST /v1/messages/count_tokens` or `GET /v1/models[/{id}]` | 403 `path_not_allowed`, or 405 for the wrong method |
| Hourly and daily budget not yet spent (`count_tokens` and `models` are free) | 429 `hourly_budget_exhausted` / `daily_budget_exhausted` |
| Body within `max_request_bytes` | 413 `request_too_large` |
| Body is JSON | 400 `invalid_json` |
| Body is unambiguous: valid UTF-8, no duplicate keys at any depth, no key that differs only in case from a field the gate checks | 400 `ambiguous_json` |
| Every image and document block in `system` and `messages` (including inside `tool_result` content and content documents) has an inline source: `base64`, `text` or `content`. A `url` source makes the API fetch the URL from Anthropic's network; `file` reads the Files API. Other types are refused until allowed. `tool_use` input isn't inspected | 403 `source_not_allowed` |
| `model` is in `models` (also for `count_tokens`) | 403 `model_not_allowed` |
| `max_tokens` between 1 and the cap (after clamping, if on) | 400 `max_tokens_over_cap` |
| Tools are custom, or their `type` matches `allowed_tool_types` | 403 `server_tool_not_allowed` for tools that reach the network from Anthropic's side (`web_search*`, `web_fetch*`, `code_execution*`, `mcp_toolset`), otherwise `tool_type_not_allowed` |
| No `mcp_servers` | 403 `mcp_servers_not_allowed` |
| No `container` | 403 `container_not_allowed` |
| `fallbacks` name only allowed models; `"default"` only with `allow_default_fallbacks` | 403 `fallback_model_not_allowed` / `fallbacks_not_allowed`, 400 `fallbacks_invalid` |
| Every `anthropic-beta` value is in `allowed_betas` | 403 `beta_not_allowed` |

## Headers

- **Removed before forwarding:** `x-api-key`, `authorization`, cookies, proxy and
  `x-forwarded-*` headers, hop-by-hop headers, `accept-encoding`.
- **Passed through:** `anthropic-version`, allowed `anthropic-beta` values,
  `content-type`, other `anthropic-*` headers.
- **Set:** `x-api-key`, from the role's key file.
- **Removed from responses:** `set-cookie`.

The gate ignores `HTTP(S)_PROXY` and doesn't follow redirects.

## Metering

- **Non-streaming responses:** the `usage` object in the body.
- **Streaming responses:** parsed as the bytes pass through, unbuffered. Usage
  comes from `message_start` and `message_delta`, taking the largest value of each
  counter. A stream that ends without `message_stop` is logged as
  `stream_incomplete` and charged for what it reported.
- **Cost:** uncached input, output, cache reads and cache writes (split by
  lifetime when the response says), each at its rate.
- **Long-context tier:** the prompt length is input plus cache reads plus cache
  writes. Each request picks its tier on its own.
- **Server-side fallbacks:** when `usage.iterations` is present, each attempt is
  charged at its own model's rates and tier.
- **Unknown models** are charged at the highest configured rate in each category
  and counted in `model_gate_unpriced_total`.
- **Error responses** aren't charged.

Budgets are checked before forwarding, so a window can overshoot by at most one
request, which `max_tokens_per_request` bounds.

## Audit log

One JSON line per request on stdout: request ID, role, method, path, model,
response model, stream, `max_tokens` (and `max_tokens_requested` if clamped),
status, rejection reason or error, the upstream request ID, token counts, cost,
the hour and day totals, latency. Never prompt or response bodies.

## Metrics

On `metrics_listen`:

| Metric | Labels |
|---|---|
| `model_gate_requests_total` | `role`, `outcome`, `status` |
| `model_gate_rejections_total` | `role`, `reason` |
| `model_gate_tokens_total` | `role`, `model`, `kind` |
| `model_gate_cost_usd_total` | `role` |
| `model_gate_unpriced_total` | `role`, `model` |
| `model_gate_budget_spent_usd` | `role`, `window` |
| `model_gate_budget_limit_usd` | `role`, `window` |

The alert rules are listed in [Alerts](alerts.md).

## The watch-officer role

As configured in [`deploy/familiar/10-model-gate.yaml`](../../deploy/familiar/10-model-gate.yaml):
Sonnet 5.5 and Haiku 5.5 (no Opus), `max_tokens` 32,000 with clamping, $1 an hour
and $3 a day, and the two betas Hermes v0.23.3 sends
(`interleaved-thinking-2025-05-14`, `fine-grained-tool-streaming-2025-05-14`). A
Hermes upgrade that sends a new beta shows up as `beta_not_allowed` rejections and
the `ModelGatePolicyRejections` alert.
