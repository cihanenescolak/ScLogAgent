package prefilter

import "unicode/utf8"

// MaxLineChars is the server's hard limit on line length, in UTF-16 code units
// (JS string length); longer lines are cut before hashing.
const MaxLineChars = 16384

// TruncateLine cuts text to MaxLineChars UTF-16 code units, like JS
// text.slice(0, MaxLineChars). If the cut splits a surrogate pair, the lone
// high surrogate becomes U+FFFD, which is exactly what JS UTF-8 encoding
// produces for it, so hashes match on both sides.
func TruncateLine(text string) string {
	// Fast path: every rune is at most 2 UTF-16 units and at least 1 byte.
	if len(text) <= MaxLineChars {
		return text
	}
	units := 0
	for i, r := range text {
		w := 1
		if r >= 0x10000 {
			w = 2
		}
		if units+w > MaxLineChars {
			if units < MaxLineChars { // half of a surrogate pair fits
				return text[:i] + string(utf8.RuneError)
			}
			return text[:i]
		}
		units += w
	}
	return text
}
