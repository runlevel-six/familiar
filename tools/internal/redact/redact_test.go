// SPDX-License-Identifier: Apache-2.0

package redact

import (
	"strings"
	"testing"
)

func TestString(t *testing.T) {
	o := Options{IPs: true}
	for in, want := range map[string]string{
		"key sk-ant-api03-abcdefghijklmnop":                         "key [REDACTED:anthropic-key]",
		"Authorization: Bearer abcdefghijklmnopqrstuv":              "Authorization: Bearer [REDACTED]",
		"tok eyJhbGciOiJIUzI1.eyJzdWIiOiIxMjM0.SflKxwRJSMeKKF2QT4f": "tok [REDACTED:jwt]",
		"password=hunter2 next":                                     "password=[REDACTED] next",
		`api_key: "abc def"`:                                        "api_key: [REDACTED]",
		"postgres://app:s3cret@db:5432/x":                           "postgres://app:[REDACTED]@db:5432/x", // gitleaks:allow (synthetic credential: tests the redactor)
		"mail alice@example.com now":                                "mail [REDACTED:email] now",
		"Get http://172.16.0.5:8080/healthz failed":                 "Get http://[IP]:8080/healthz failed",
		"AKIAABCDEFGHIJKLMNOP":                                      "[REDACTED:aws-key]",
		"hidden\U000E0041\U000E0042text​!":                          "hiddentext!",
		"bidi ‮evil":                                                "bidi evil",
		"keep\nnewlines\tand tabs":                                  "keep\nnewlines\tand tabs",
		"image sha256:0123456789abcdef0123456789abcdef":             "image sha256:0123456789abcdef0123456789abcdef",
		"version v1.37.2 is fine":                                   "version v1.37.2 is fine",
	} {
		if got := String(in, o); got != want {
			t.Errorf("String(%q) = %q, want %q", in, got, want)
		}
	}
	if got := String("10.0.0.1", Options{}); got != "10.0.0.1" {
		t.Errorf("IPs redacted without the option: %q", got)
	}
	long := strings.Repeat("é", 3000)
	if got := String(long, Options{MaxString: 100}); !strings.HasSuffix(got, "…[truncated]") || len(got) > 120 {
		t.Errorf("truncation: len %d", len(got))
	}
}

func TestValueWalksEverything(t *testing.T) {
	in := map[string]any{
		"msg":   "token=abc123",
		"list":  []any{"user@example.com", 3, true},
		"inner": map[string]any{"password: x": "ok"},
	}
	out, err := Value(in, Options{})
	if err != nil {
		t.Fatal(err)
	}
	m := out.(map[string]any)
	if m["msg"] != "token=[REDACTED]" || m["list"].([]any)[0] != "[REDACTED:email]" {
		t.Errorf("values not redacted: %v", m)
	}
	if _, ok := m["inner"].(map[string]any)["password: [REDACTED]"]; !ok {
		t.Errorf("keys not redacted: %v", m["inner"])
	}
}
