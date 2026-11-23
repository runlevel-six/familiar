// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
)

// Usage mirrors the Messages API usage object. input_tokens is the
// uncached remainder only; total prompt = input + cache_creation + cache_read.
type Usage struct {
	InputTokens              int64          `json:"input_tokens"`
	OutputTokens             int64          `json:"output_tokens"`
	CacheCreationInputTokens int64          `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64          `json:"cache_read_input_tokens"`
	CacheCreation            *CacheCreation `json:"cache_creation,omitempty"`
	// Iterations is present with server-side fallbacks: one entry per
	// attempt, and the per-attempt billing source of truth (top-level usage
	// covers only the attempt that produced the returned message).
	Iterations []Iteration `json:"iterations,omitempty"`
}

type CacheCreation struct {
	Ephemeral5m int64 `json:"ephemeral_5m_input_tokens"`
	Ephemeral1h int64 `json:"ephemeral_1h_input_tokens"`
}

// Iteration is one attempt. Its exact shape isn't pinned down in the docs we
// have, so accept usage fields either inline or nested under "usage".
type Iteration struct {
	Type  string `json:"type"`
	Model string `json:"model"`
	Usage
	Nested *Usage `json:"usage,omitempty"`
}

func (it Iteration) usage() Usage {
	if it.Nested != nil {
		return *it.Nested
	}
	return it.Usage
}

// merge keeps the larger value of each counter. message_delta usage is
// cumulative, so max() over events gives the final numbers.
func (u *Usage) merge(o Usage) {
	u.InputTokens = max(u.InputTokens, o.InputTokens)
	u.OutputTokens = max(u.OutputTokens, o.OutputTokens)
	u.CacheCreationInputTokens = max(u.CacheCreationInputTokens, o.CacheCreationInputTokens)
	u.CacheReadInputTokens = max(u.CacheReadInputTokens, o.CacheReadInputTokens)
	if o.CacheCreation != nil {
		if u.CacheCreation == nil {
			u.CacheCreation = &CacheCreation{}
		}
		u.CacheCreation.Ephemeral5m = max(u.CacheCreation.Ephemeral5m, o.CacheCreation.Ephemeral5m)
		u.CacheCreation.Ephemeral1h = max(u.CacheCreation.Ephemeral1h, o.CacheCreation.Ephemeral1h)
	}
	if len(o.Iterations) > 0 {
		u.Iterations = o.Iterations
	}
}

// costUSD prices one usage object at one model's rates, choosing the
// long-context tier by this request's own prompt length.
func costUSD(p Price, u Usage) float64 {
	write5m, write1h := u.CacheCreationInputTokens, int64(0)
	if u.CacheCreation != nil && u.CacheCreation.Ephemeral5m+u.CacheCreation.Ephemeral1h > 0 {
		write5m, write1h = u.CacheCreation.Ephemeral5m, u.CacheCreation.Ephemeral1h
	}
	p = p.forPrompt(u.InputTokens + u.CacheReadInputTokens + write5m + write1h)
	m := 1e6
	return float64(u.InputTokens)*p.Input/m +
		float64(u.OutputTokens)*p.Output/m +
		float64(u.CacheReadInputTokens)*p.CacheRead/m +
		float64(write5m)*p.CacheWrite5m/m +
		float64(write1h)*p.CacheWrite1h/m
}

// totalCost prices a response, summing iterations when present.
// unknownModel reports whether a model without a configured price was seen.
func (c *Config) totalCost(model string, u Usage) (usd float64, unknownModel bool) {
	if len(u.Iterations) == 0 {
		p, ok := c.priceFor(model)
		return costUSD(p, u), !ok
	}
	for _, it := range u.Iterations {
		m := it.Model
		if m == "" {
			m = model
		}
		p, ok := c.priceFor(m)
		usd += costUSD(p, it.usage())
		unknownModel = unknownModel || !ok
	}
	return usd, unknownModel
}

// parseJSONUsage extracts model and usage from a non-streaming response.
func parseJSONUsage(body []byte) (model string, u Usage, ok bool) {
	var r struct {
		Model string `json:"model"`
		Usage *Usage `json:"usage"`
	}
	if json.Unmarshal(body, &r) != nil || r.Usage == nil {
		return "", Usage{}, false
	}
	return r.Model, *r.Usage, true
}

// sseUsage incrementally parses an SSE stream as bytes pass through,
// collecting usage from message_start and message_delta events.
type sseUsage struct {
	buf      bytes.Buffer
	Usage    Usage
	Model    string
	Seen     bool // at least one usage-bearing event parsed
	Complete bool // message_stop seen
	Error    string
}

func (s *sseUsage) Write(p []byte) (int, error) {
	s.buf.Write(p)
	for {
		line, err := s.buf.ReadBytes('\n')
		if err != nil { // incomplete line: keep it for the next write
			rest := append([]byte(nil), line...)
			s.buf.Reset()
			s.buf.Write(rest)
			return len(p), nil
		}
		s.line(bytes.TrimRight(line, "\r\n"))
	}
}

func (s *sseUsage) line(l []byte) {
	data, ok := bytes.CutPrefix(l, []byte("data:"))
	if !ok {
		return
	}
	data = bytes.TrimSpace(data)
	var ev struct {
		Type    string `json:"type"`
		Message *struct {
			Model string `json:"model"`
			Usage *Usage `json:"usage"`
		} `json:"message"`
		Usage *Usage `json:"usage"`
		Error *struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &ev) != nil {
		return
	}
	switch ev.Type {
	case "message_start":
		if ev.Message != nil {
			s.Model = ev.Message.Model
			if ev.Message.Usage != nil {
				s.Usage.merge(*ev.Message.Usage)
				s.Seen = true
			}
		}
	case "message_delta":
		if ev.Usage != nil {
			s.Usage.merge(*ev.Usage)
			s.Seen = true
		}
	case "message_stop":
		s.Complete = true
	case "error":
		if ev.Error != nil {
			s.Error = ev.Error.Type
		}
	}
}
