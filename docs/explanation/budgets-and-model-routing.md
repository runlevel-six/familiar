# Budgets and model routing

An agent that runs on a schedule, unattended, can spend money in a loop. familiar
limits that in three layers and keeps the normal cost low by sending each kind of
work to the cheapest model that does it well.

## Three layers of spend limits

1. **Per request:** model-gate caps `max_tokens` (32,000 for the watch officer),
   so one request can't overshoot a budget by more than that. Hermes v0.23
   always asks for the model's maximum and can't be configured otherwise, so the
   role uses `clamp_max_tokens`: the gate lowers the value instead of rejecting
   the request. Billing is per token actually produced, so this changes nothing
   for normal replies.
2. **Per hour and per day:** model-gate meters every response in USD and refuses
   further requests (with a 429, like a rate limit) once a window is spent. The
   watch officer gets **$1 an hour and $3 a day**: a normal day is well under $1,
   and a runaway loop is stopped within the hour. An alert fires at 80% of the
   daily budget.
3. **Per month:** the API workspace has its own spend limit in the Anthropic
   Console, $42, as the outer cap that holds even if model-gate is wrong.

Budgets are in dollars, not tokens, because tokens aren't equal: a cache read
costs a twentieth of fresh input on Sonnet 5.5, and Haiku 5.5 charges five times
more for a prompt over 100,000 tokens. model-gate prices each response from a
table of per-model rates, including both of those details; see
[model-gate reference](../reference/model-gate.md#metering).

## Routing work to models

| Work | How often | Model | Why |
|---|---|---|---|
| Alert check | every 15 min | none, unless an alert is newly firing | A script asks metrics-query for firing alerts and wakes the agent only for a new non-info alert |
| Health sweep | every 4 hours | Haiku 5.5 | Routine checks; Haiku found the same real issue Sonnet did in testing, at about a twentieth of the cost |
| Alert triage | when woken | Sonnet 5.5 | Diagnosis benefits from the stronger model |
| Conversation | when asked | Sonnet 5.5 | The default model |

Opus isn't in the watch officer's model allowlist at all. Each cron job is pinned
to its model, the agent can't change a job's model, and model-gate's allowlist
enforces it regardless.

## What it costs

Measured in the local test (2026-11-22), same prompt and cluster state:

| | Haiku 5.5 | Sonnet 5.5 |
|---|---|---|
| Cost per sweep | $0.004 | $0.09 |
| Model calls / tool calls | 4 / 13 | 6 / 19 |

Expected per month:

| Item | Expected |
|---|---|
| Haiku sweeps (about 180) | $1–1.50 |
| Sonnet alert triage (5–15 at $0.15–0.40) | $1–6 |
| Conversations (a few a week at $0.30–1.00) | $5–15 |
| **Total** | **$7–22** |

Context compression also runs on Haiku and fires when a session nears Hermes'
200,000-token context limit, so those calls land in Haiku's higher price tier at
roughly $0.05–0.10 each. They're rare enough not to move the estimate. The
numbers get rechecked against `model_gate_cost_usd_total` after the first week
of Phase 1.
