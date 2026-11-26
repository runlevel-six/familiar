// SPDX-License-Identifier: Apache-2.0

package redact

import (
	"strings"
	"unicode"
)

// PlainLabel cleans a short name that something untrusted chose (a LAN
// client's name, a process's comm): letters, digits, spaces and a little
// punctuation, at most max of them, "…" if cut; anything else becomes "_".
// No link, markup, control or invisible character survives.
func PlainLabel(v string, max int) string {
	var b strings.Builder
	n := 0
	for _, r := range v {
		if n == max {
			b.WriteString("…")
			break
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune(" .-_'’()#&+", r) {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
		n++
	}
	return b.String()
}
