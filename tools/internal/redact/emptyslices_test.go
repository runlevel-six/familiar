// SPDX-License-Identifier: Apache-2.0

package redact

import (
	"encoding/json"
	"testing"
	"time"
)

func TestNilSlicesBecomeEmptyLists(t *testing.T) {
	type inner struct {
		Names []string `json:"names"`
		Gone  []string `json:"gone,omitempty"`
	}
	type out struct {
		Items  []inner           `json:"items"`
		Nested []inner           `json:"nested"`
		Ptr    *inner            `json:"ptr"`
		NilPtr *inner            `json:"nil_ptr"`
		Map    map[string]any    `json:"map"`
		NilMap map[string]string `json:"nil_map"`
		When   time.Time         `json:"when"`
		hidden []string
	}
	when := time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)
	v := map[string]any{
		"results": []int(nil),
		"struct":  out{Nested: []inner{{}}, Ptr: &inner{}, Map: map[string]any{"x": []string(nil)}, When: when, hidden: nil},
		"null":    nil,
		"bytes":   []byte(nil),
	}
	got, err := Value(v, Options{})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(got)
	want := `{"bytes":"","null":null,"results":[],"struct":{"items":[],"map":{"x":[]},"nested":[{"names":[]}],"nil_map":null,"nil_ptr":null,"ptr":{"names":[]},"when":"2026-11-23T00:00:00Z"}}`
	if string(b) != want {
		t.Errorf("got  %s\nwant %s", b, want)
	}
}
