// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// The gate reads a request with encoding/json, which matches object keys
// case-insensitively and lets the last of duplicate keys win, then forwards
// the original bytes. Another parser (the API's) may read the same bytes
// differently: honour the first "tools", or ignore "TOOLS". checkUnambiguous
// rejects any body where that could happen, so what the gate checked is what
// the upstream sees.
//
// Fields the gate decides on, per object it inspects. A key that differs from
// one of these only in case (including Unicode folding, e.g. "max_toKens"
// with a Kelvin sign) is rejected wherever that object appears.
var checkedFields = map[string][]string{
	"top":      {"model", "max_tokens", "stream", "tools", "mcp_servers", "container", "fallbacks"},
	"tool":     {"type", "name"},
	"fallback": {"model"},
}

// checkUnambiguous returns an error if body has invalid UTF-8, a duplicate
// key in any object at any depth, or a case variant of a checked field.
// The body must already have parsed with json.Unmarshal (which also bounds
// nesting depth, and with it this function's recursion).
func checkUnambiguous(body []byte) error {
	if !utf8.Valid(body) {
		return errors.New("body is not valid UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	return walkJSON(dec, "top")
}

// walkJSON consumes one value. ctx names the object kind (a checkedFields
// key) when the value is an object, or the kind of its elements' parent
// ("tools", "fallbacks") when it is an array.
func walkJSON(dec *json.Decoder, ctx string) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '[':
		elem := ""
		switch ctx {
		case "tools":
			elem = "tool"
		case "fallbacks":
			elem = "fallback"
		}
		for dec.More() {
			if err := walkJSON(dec, elem); err != nil {
				return err
			}
		}
	case '{':
		seen := map[string]bool{}
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return err
			}
			key, _ := kt.(string)
			if seen[key] {
				return fmt.Errorf("duplicate key %q", key)
			}
			seen[key] = true
			for _, f := range checkedFields[ctx] {
				if key != f && strings.EqualFold(key, f) {
					return fmt.Errorf("key %q differs from %q only in case", key, f)
				}
			}
			child := ""
			if ctx == "top" && (key == "tools" || key == "fallbacks") {
				child = key
			}
			if err := walkJSON(dec, child); err != nil {
				return err
			}
		}
	}
	_, err = dec.Token() // closing ']' or '}'
	return err
}
