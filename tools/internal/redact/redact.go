// SPDX-License-Identifier: Apache-2.0

// Package redact scrubs tool output before it reaches the agent (design
// §4.1: tool servers return redacted summaries; T2, T9). Everything a tool
// returns passes through Value, so a new field can't skip redaction.
package redact

import (
	"encoding/json"
	"regexp"
	"strings"
	"unicode"
)

type rule struct {
	re   *regexp.Regexp
	repl string
}

var rules = []rule{
	// Credentials first, so later rules don't split them.
	{regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{8,}`), "[REDACTED:anthropic-key]"},
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), "[REDACTED:jwt]"},
	{regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{12,}`), "Bearer [REDACTED]"},
	{regexp.MustCompile(`\b(AKIA|ASIA)[0-9A-Z]{16}\b`), "[REDACTED:aws-key]"},
	{regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}\b`), "[REDACTED:github-token]"},
	{regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?(-----END [A-Z ]*PRIVATE KEY-----|$)`), "[REDACTED:private-key]"},
	{regexp.MustCompile(`(?i)\b(password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|client[_-]?secret)(\s*[:=]\s*)("[^"]*"|'[^']*'|[^\s,;&]+)`), "$1$2[REDACTED]"},
	{regexp.MustCompile(`(?i)(://[^/\s:@]+:)[^/\s@]+@`), "${1}[REDACTED]@"}, // user:pass@ in URLs
	{regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`), "[REDACTED:email]"},
}

var (
	ipv4 = regexp.MustCompile(`\b(25[0-5]|2[0-4]\d|1?\d?\d)(\.(25[0-5]|2[0-4]\d|1?\d?\d)){3}\b`)
	ipv6 = regexp.MustCompile(`\b([0-9a-fA-F]{1,4}:){2,7}[0-9a-fA-F]{1,4}\b`)
)

// Options controls the optional rules.
type Options struct {
	// IPs replaces IPv4/IPv6 addresses (design: logs are redacted of IPs).
	IPs bool
	// MaxString truncates long strings (0 = 2000).
	MaxString int
}

// String redacts one string.
func String(s string, o Options) string {
	s = stripInvisible(s)
	for _, r := range rules {
		s = r.re.ReplaceAllString(s, r.repl)
	}
	if o.IPs {
		s = ipv4.ReplaceAllString(s, "[IP]")
		s = ipv6.ReplaceAllString(s, "[IP]")
	}
	limit := o.MaxString
	if limit == 0 {
		limit = 2000
	}
	if len(s) > limit {
		cut := limit
		for cut > 0 && !utf8Start(s[cut]) {
			cut--
		}
		s = s[:cut] + "…[truncated]"
	}
	return s
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// stripInvisible removes characters that can hide instructions from a human
// reviewer: Unicode tag characters, bidi controls, zero-width characters and
// other control characters (keeping \n and \t).
func stripInvisible(s string) string {
	if !strings.ContainsFunc(s, invisible) {
		return s
	}
	return strings.Map(func(r rune) rune {
		if invisible(r) {
			return -1
		}
		return r
	}, s)
}

func invisible(r rune) bool {
	switch {
	case r == '\n' || r == '\t':
		return false
	case r >= 0xE0000 && r <= 0xE007F: // tags
		return true
	case r >= 0x200B && r <= 0x200F, r >= 0x202A && r <= 0x202E, r >= 0x2060 && r <= 0x2069, r == 0xFEFF:
		return true
	}
	return unicode.IsControl(r)
}

// Value redacts every string (keys and values) in any JSON-encodable value
// and returns the cleaned generic structure.
func Value(v any, o Options) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var g any
	if err := json.Unmarshal(b, &g); err != nil {
		return nil, err
	}
	return walk(g, o), nil
}

func walk(v any, o Options) any {
	switch t := v.(type) {
	case string:
		return String(t, o)
	case []any:
		for i := range t {
			t[i] = walk(t[i], o)
		}
		return t
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[String(k, o)] = walk(val, o)
		}
		return out
	}
	return v
}
