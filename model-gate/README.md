# model-gate

The only familiar component that holds Anthropic API keys. Agents send
unauthenticated Messages API requests to their role's port; model-gate checks
each one (endpoint, models, tools, betas, size, an unambiguous body), swaps in the
role's key, forwards it to `api.anthropic.com`, meters the cost in USD against
hourly and daily budgets, and writes one audit line. Go standard library only.

- [Reference](../docs/reference/model-gate.md): flags, configuration, every check,
  metering, the audit log and metrics.
- [Budgets and model routing](../docs/explanation/budgets-and-model-routing.md):
  why the limits are what they are.
- [`config.example.json`](config.example.json): a working configuration.

## Develop

```sh
go vet ./... && go test -race ./...
go build -o model-gate . && ./model-gate -check -config config.example.json   # needs the key files
```

The tests run against a fake upstream. They cover key injection and header
stripping, every rejection (and that rejected requests never reach the
upstream), ambiguous JSON, streaming pass-through with chunks split mid-line,
budgets across restarts, fallback and long-context pricing, and the metrics.
