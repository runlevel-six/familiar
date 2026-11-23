// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Config is loaded from a JSON file (stdlib only; no YAML dependency).
type Config struct {
	// Upstream is the Anthropic API base URL. Only https is accepted.
	Upstream string `json:"upstream"`
	// MetricsListen serves /metrics and /healthz (Prometheus scrape only;
	// NetworkPolicy must keep agents away from it).
	MetricsListen string `json:"metrics_listen"`
	// StateFile persists budget spend across restarts (optional but
	// recommended: without it a restart resets the hourly/daily windows).
	StateFile string `json:"state_file"`
	// MaxRequestBytes caps request bodies (default 32 MiB).
	MaxRequestBytes int64 `json:"max_request_bytes"`
	// MaxResponseBytes caps buffered non-streaming responses (default 64 MiB).
	MaxResponseBytes int64 `json:"max_response_bytes"`
	// Prices in USD per million tokens, by model ID.
	Prices map[string]Price `json:"prices"`
	Roles  []Role           `json:"roles"`
}

// Price is USD per million tokens.
type Price struct {
	Input        float64 `json:"input"`
	Output       float64 `json:"output"`
	CacheRead    float64 `json:"cache_read"`
	CacheWrite5m float64 `json:"cache_write_5m"`
	CacheWrite1h float64 `json:"cache_write_1h"`
	// LongContext, if set, replaces all five rates for a request whose
	// prompt is over OverPromptTokens (Claude Haiku 5.5: over 100,000).
	LongContext *LongContextPrice `json:"long_context,omitempty"`
}

// LongContextPrice is the higher rate card for long prompts. Prompt length
// is input + cache reads + cache writes, and each request (each fallback
// iteration) is priced on its own (platform.claude.com pricing, 2026-11-23).
type LongContextPrice struct {
	OverPromptTokens int64   `json:"over_prompt_tokens"`
	Input            float64 `json:"input"`
	Output           float64 `json:"output"`
	CacheRead        float64 `json:"cache_read"`
	CacheWrite5m     float64 `json:"cache_write_5m"`
	CacheWrite1h     float64 `json:"cache_write_1h"`
}

// forPrompt returns the rates that apply to a prompt of n tokens.
func (p Price) forPrompt(n int64) Price {
	lc := p.LongContext
	if lc == nil || n <= lc.OverPromptTokens {
		return Price{Input: p.Input, Output: p.Output, CacheRead: p.CacheRead, CacheWrite5m: p.CacheWrite5m, CacheWrite1h: p.CacheWrite1h}
	}
	return Price{Input: lc.Input, Output: lc.Output, CacheRead: lc.CacheRead, CacheWrite5m: lc.CacheWrite5m, CacheWrite1h: lc.CacheWrite1h}
}

// Role is one caller identity, bound to its own listener (the
// CiliumNetworkPolicy decides which pods can reach which port).
type Role struct {
	Name    string `json:"name"`
	Listen  string `json:"listen"`
	KeyFile string `json:"key_file"`
	// Models the role may request (exact IDs). Also applies to fallbacks.
	Models []string `json:"models"`
	// MaxTokensPerRequest caps the request's max_tokens.
	MaxTokensPerRequest int64 `json:"max_tokens_per_request"`
	// ClampMaxTokens lowers an over-cap max_tokens to the cap instead of
	// rejecting the request (for clients that always ask for the model
	// maximum and can't be configured, e.g. Hermes v0.23). The original
	// value is recorded in the audit log as max_tokens_requested.
	ClampMaxTokens bool `json:"clamp_max_tokens"`
	// Budgets in USD; 0 disables that window (not recommended).
	HourlyUSD float64 `json:"hourly_usd"`
	DailyUSD  float64 `json:"daily_usd"`
	// AllowedBetas is the exact set of anthropic-beta values passed; any
	// other value is rejected. Required: [] means none. A beta can switch on
	// server-side behaviour the body checks don't know about (T2, T9).
	AllowedBetas []string `json:"allowed_betas"`
	// AllowDefaultFallbacks permits fallbacks:"default" (server picks the
	// model, which may be outside Models). Off by default.
	AllowDefaultFallbacks bool `json:"allow_default_fallbacks"`
	// AllowedToolTypes lists tool `type` prefixes to permit besides custom
	// tools (no type, or "custom"), e.g. "bash_" or "web_search". Empty =
	// custom tools only, so a tool type Anthropic adds later is denied until
	// someone decides it is safe.
	AllowedToolTypes []string `json:"allowed_tool_types"`
	// DebugCaptureDir, if set, stores request and response bodies there.
	// Off by default: bodies are prompts and may contain cluster data.
	DebugCaptureDir string `json:"debug_capture_dir"`

	key string // loaded from KeyFile
}

func loadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) validate() error {
	if c.Upstream == "" {
		c.Upstream = "https://api.anthropic.com"
	}
	if !strings.HasPrefix(c.Upstream, "https://") && !strings.HasPrefix(c.Upstream, "http://127.0.0.1") {
		return errors.New("upstream must be https")
	}
	c.Upstream = strings.TrimRight(c.Upstream, "/")
	if c.MaxRequestBytes == 0 {
		c.MaxRequestBytes = 32 << 20
	}
	if c.MaxResponseBytes == 0 {
		c.MaxResponseBytes = 64 << 20
	}
	for m, p := range c.Prices {
		if p.LongContext != nil && p.LongContext.OverPromptTokens <= 0 {
			return fmt.Errorf("price %q: long_context.over_prompt_tokens must be positive", m)
		}
	}
	if len(c.Roles) == 0 {
		return errors.New("no roles configured")
	}
	seenName, seenListen := map[string]bool{}, map[string]bool{}
	for i := range c.Roles {
		r := &c.Roles[i]
		switch {
		case r.Name == "":
			return fmt.Errorf("role %d: name required", i)
		case seenName[r.Name]:
			return fmt.Errorf("role %q: duplicate name", r.Name)
		case r.Listen == "" || seenListen[r.Listen]:
			return fmt.Errorf("role %q: listen address missing or duplicated", r.Name)
		case r.Listen == c.MetricsListen:
			return fmt.Errorf("role %q: listen address equals metrics_listen", r.Name)
		case len(r.Models) == 0:
			return fmt.Errorf("role %q: models allowlist required", r.Name)
		case r.MaxTokensPerRequest <= 0:
			return fmt.Errorf("role %q: max_tokens_per_request required", r.Name)
		case r.AllowedBetas == nil:
			return fmt.Errorf("role %q: allowed_betas required ([] for none)", r.Name)
		}
		for _, m := range r.Models {
			if _, ok := c.Prices[m]; !ok {
				return fmt.Errorf("role %q: model %q has no price", r.Name, m)
			}
		}
		seenName[r.Name], seenListen[r.Listen] = true, true
	}
	return nil
}

// loadKeys reads each role's API key file. Keys never come from env vars.
func (c *Config) loadKeys() error {
	for i := range c.Roles {
		r := &c.Roles[i]
		if r.KeyFile == "" {
			return fmt.Errorf("role %q: key_file required", r.Name)
		}
		b, err := os.ReadFile(r.KeyFile)
		if err != nil {
			return fmt.Errorf("role %q: %w", r.Name, err)
		}
		r.key = strings.TrimSpace(string(b))
		if r.key == "" {
			return fmt.Errorf("role %q: key file is empty", r.Name)
		}
	}
	return nil
}

// priceFor returns the model's price, or for unknown models (e.g. a
// server-side fallback model) the highest configured rate in each category,
// long-context tiers included, so cost is overestimated rather than missed.
func (c *Config) priceFor(model string) (Price, bool) {
	if p, ok := c.Prices[model]; ok {
		return p, true
	}
	var hi Price
	raise := func(in, out, read, w5, w1 float64) {
		hi.Input, hi.Output, hi.CacheRead = max(hi.Input, in), max(hi.Output, out), max(hi.CacheRead, read)
		hi.CacheWrite5m, hi.CacheWrite1h = max(hi.CacheWrite5m, w5), max(hi.CacheWrite1h, w1)
	}
	for _, p := range c.Prices {
		raise(p.Input, p.Output, p.CacheRead, p.CacheWrite5m, p.CacheWrite1h)
		if lc := p.LongContext; lc != nil {
			raise(lc.Input, lc.Output, lc.CacheRead, lc.CacheWrite5m, lc.CacheWrite1h)
		}
	}
	return hi, false
}
