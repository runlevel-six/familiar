# Security model

familiar assumes the agent can turn adversarial at any moment: through prompt
injection in something it reads, a poisoned memory, a malicious skill or plugin,
or the model simply misbehaving. The design has to hold even if the agent
actively tries to get around it. So the controls are structural (network
policy, credentials it doesn't have, brokers it can't reconfigure) rather than
instructions it's asked to follow. The prompt in `SOUL.md` describes the agent's
role; nothing depends on the agent obeying it.

## The lethal trifecta

An agent becomes dangerous when one instance has all three of: **private data**,
**untrusted input**, and **a way to send data out**. Each phase records which of
the three it grants. Phase 1 has all three, in limited form:

- private data: cluster state, but only as redacted summaries from fixed queries;
- untrusted input: low, since logs and event messages can carry text from
  applications exposed to the internet;
- an external channel: Matrix, to one allowlisted user, on a homeserver inside
  the cluster with federation off.

That is why tool output is redacted, the Matrix gateway accepts only the owner,
and the agent's network reaches nothing on the internet. Phase 3's reader, which
handles genuinely untrusted input, gets no tools and only structured output.

## Threats and controls

| # | Threat | Primary control | Secondary control |
|---|---|---|---|
| T1 | Exfiltrate the model API key | The key exists only in model-gate, never in the agent's environment | Egress deny; a monthly spend limit on the API workspace |
| T2 | Exfiltrate cluster data | Tools return redacted summaries; egress allowlist | Hubble flow audit; model-gate rejects server-side tools that would fetch from Anthropic's side |
| T3 | Escalate in the cluster | No ServiceAccount token in the agent pod; tool servers hold narrow roles | RBAC excludes policy objects; cluster-read hides familiar's own namespaces |
| T4 | Disable or change its own guardrails | No write path to `familiar-*` namespaces, RBAC or network policies | Guardrails live in this repository, which only the owner can merge to |
| T5 | Persist an injection through memory | memory-auditor commits every memory change to a repo the agent can't reach; changes are reviewed | A restore procedure |
| T6 | Runaway spend or loops | Per-request, hourly and daily budgets in model-gate; a turn cap in Hermes | The workspace's monthly spend limit |
| T7 | Starve the node | ResourceQuota, LimitRange, lowest PriorityClass | The off-cluster dead-man's switch |
| T8 | Supply-chain compromise | Images pinned by digest; brokers use only the Go standard library; skills vendored | Egress deny limits any callback |
| T9 | Untrusted input steers actions | Tool descriptions mark returned text as data; Phase 3's reader has no tools | Matrix accepts only the owner |
| T10 | The agent hides what it did | Brokers log every call independently of the agent | Falco, later |

The full table, with the reasoning behind each choice, is §3 of the
[design document](../DESIGN.md).

## Choices worth explaining

**A purpose-built gateway instead of an off-the-shelf one.** model-gate holds
the most valuable credential in the system. It's a few hundred lines of Go with
no third-party dependencies, which is easy to audit; a popular LLM gateway's
release pipeline was compromised twice in 2026.

**What the gateway refuses.** Anthropic's server-side tools (web search, web
fetch, code execution, the MCP connector) make *Anthropic* reach the network on
the caller's behalf, which would walk straight past the cluster's egress
policy. So model-gate allows only custom tools unless a role opts in, and the
list is an allowlist: a server-side tool Anthropic adds later is refused until
someone decides it's safe. `anthropic-beta` headers are allowlisted per role for
the same reason. Request bodies with duplicate or case-variant keys are
rejected, because the gate's JSON parser and the API's could read them
differently, and the gate would then check one request and forward another.

**Broker-side logging.** The agent's own transcript is not evidence. Every model
call is logged by model-gate and every tool call by the tool server, with
arguments, outcome and size, whatever the agent reports.

**Detection where prevention isn't possible.** Hermes has to write its memory,
so memory poisoning can't be prevented outright. memory-auditor makes it
visible instead: a change to scheduled jobs, config or unexpected files is a
warning that reaches the owner; a memory change is reviewed weekly.

**Redaction at the source.** Tool servers strip API keys, tokens, private keys,
credentials in URLs, email addresses and IP addresses from every string they
return, and remove invisible Unicode that could hide instructions.

**The agent can't see its own guardrails.** cluster-read hides the `familiar-*`
namespaces, so the agent can't inspect model-gate, the tool servers or
memory-auditor. Access would be read-only anyway; this removes the
reconnaissance.

**Alertmanager stays out of reach.** Its API is unauthenticated and can create
silences, which would let an agent hide alerts. No familiar component may
connect to it, and Prometheus runs with its admin API off.

## What this doesn't cover

- A compromised node or cluster admin: the owner's credentials are out of scope.
- The model provider itself.
- Everything the agent says. In Phase 1 it can only produce text for the owner,
  who decides what to act on.
