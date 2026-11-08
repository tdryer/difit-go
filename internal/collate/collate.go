// Package collate approximates JavaScript's String.prototype.localeCompare
// (ICU root collation) closely enough for sorting branch names, IDs and ISO
// timestamps the way the original difit server did.
package collate

import (
	"strings"
	"unicode"
)

// asciiOrder lists ASCII punctuation and symbols in CLDR root collation order;
// they all sort before digits, which sort before letters.
const asciiOrder = " _-,;:!?.'\"()[]{}@*/\\&#%`^+<=>|~$"

func primaryWeight(r rune) int {
	if i := strings.IndexRune(asciiOrder, r); i >= 0 {
		return i
	}
	if r >= '0' && r <= '9' {
		return 100 + int(r-'0')
	}
	if r < 128 {
		if unicode.IsLetter(r) {
			return 200 + int(unicode.ToLower(r)-'a')
		}
		return 50 + int(r) // control characters and anything unlisted
	}
	return 1000 + int(unicode.ToLower(r))
}

// Compare returns -1, 0 or 1. Strings are compared case-insensitively first;
// ties are broken with lowercase before uppercase, as ICU does.
func Compare(a, b string) int {
	ar, br := []rune(a), []rune(b)
	for i := 0; i < len(ar) && i < len(br); i++ {
		wa, wb := primaryWeight(ar[i]), primaryWeight(br[i])
		if wa != wb {
			return sign(wa - wb)
		}
	}
	if len(ar) != len(br) {
		return sign(len(ar) - len(br))
	}
	for i := range ar {
		if ar[i] == br[i] {
			continue
		}
		la, lb := unicode.IsLower(ar[i]), unicode.IsLower(br[i])
		if la && !lb {
			return -1
		}
		if lb && !la {
			return 1
		}
		return sign(int(ar[i]) - int(br[i]))
	}
	return 0
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}
