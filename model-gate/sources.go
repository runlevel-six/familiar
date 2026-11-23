// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Image and document content blocks name where their data comes from. A
// "url" source makes the API fetch the URL itself, from Anthropic's network:
// a way out that no network policy here can see, the same reason server
// tools are refused. A "file" source reads from the Files API, which the
// gate also refuses. Only inline sources are allowed; an unknown type is
// refused until it is understood (allowlist, like tool types).
var allowedSourceTypes = map[string]bool{
	"base64":  true, // inline image or PDF bytes
	"text":    true, // inline plain-text document
	"content": true, // document made of inline content blocks (checked recursively)
}

// checkSources returns an error if any content block in system or messages,
// at any depth (tool_result content, documents made of content blocks), has a
// source the gate doesn't allow. tool_use input is the model's own arguments,
// never fetched, and isn't inspected. The body has already passed
// checkUnambiguous, so there are no duplicate keys.
func checkSources(body []byte) error {
	var top struct {
		System   json.RawMessage `json:"system"`
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &top); err != nil {
		return fmt.Errorf("system or messages not in the expected shape")
	}
	if err := checkContent(top.System); err != nil {
		return err
	}
	for _, m := range top.Messages {
		if err := checkContent(m.Content); err != nil {
			return err
		}
	}
	return nil
}

// checkContent checks a content value: a string (nothing to check), null, or
// an array of blocks.
func checkContent(raw json.RawMessage) error {
	if len(raw) == 0 || raw[0] != '[' {
		return nil
	}
	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return fmt.Errorf("content is not an array of blocks")
	}
	for _, b := range blocks {
		if err := checkBlock(b); err != nil {
			return err
		}
	}
	return nil
}

func checkBlock(b map[string]json.RawMessage) error {
	if err := noCaseVariants(b, "source", "content", "type"); err != nil {
		return err
	}
	var kind string
	_ = json.Unmarshal(b["type"], &kind)
	if src, ok := b["source"]; ok {
		switch {
		case len(src) > 0 && src[0] == '"':
			// search_result: source is a citation label, not fetched
		case len(src) > 0 && src[0] == '{':
			var s map[string]json.RawMessage
			if err := json.Unmarshal(src, &s); err != nil {
				return fmt.Errorf("%s block has an unreadable source", kind)
			}
			if err := noCaseVariants(s, "type", "content"); err != nil {
				return err
			}
			var st string
			_ = json.Unmarshal(s["type"], &st)
			if !allowedSourceTypes[st] {
				return fmt.Errorf("%s block with a %q source is not allowed (only inline data)", kind, st)
			}
			if st == "content" {
				if err := checkContent(s["content"]); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("%s block has a source that is neither an object nor a string", kind)
		}
	}
	return checkContent(b["content"]) // tool_result, search_result
}

// noCaseVariants rejects a key that differs from one of names only in case,
// which the gate would skip but another parser might honour.
func noCaseVariants(obj map[string]json.RawMessage, names ...string) error {
	for k := range obj {
		for _, n := range names {
			if k != n && strings.EqualFold(k, n) {
				return fmt.Errorf("key %q differs from %q only in case", k, n)
			}
		}
	}
	return nil
}
