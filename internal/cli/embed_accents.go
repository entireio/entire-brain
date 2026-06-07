package cli

import "unicode"

// stripAccent folds a (lowercased) Latin precomposed letter to its base, or
// reports a combining mark to be dropped. It returns:
//
//	(base, true) — r is an accented Latin letter; emit base instead
//	(0, true)    — r is a combining diacritical mark; emit nothing
//	(0, false)   — r is not an accent; the caller emits r unchanged
//
// This replicates BERT's strip_accents (NFD + drop Mn) for the Latin scripts
// that appear in a code/prose fact corpus, without depending on a full Unicode
// normalization package. Non-Latin scripts pass through untouched.
func stripAccent(r rune) (rune, bool) {
	if r >= 0x0300 && r <= 0x036F { // combining diacritical marks
		return 0, true
	}
	if unicode.Is(unicode.Mn, r) { // any other nonspacing mark
		return 0, true
	}
	if base, ok := latinAccentFold[r]; ok {
		return base, true
	}
	return 0, false
}

// latinAccentFold maps lowercase Latin-1 Supplement and common Latin Extended-A
// accented letters to their unaccented base. Only lowercase entries are needed
// because normalize lowercases before stripping. Letters with no NFD
// decomposition (æ ø ð þ ß) are intentionally absent — BERT leaves them as-is.
var latinAccentFold = map[rune]rune{
	'à': 'a', 'á': 'a', 'â': 'a', 'ã': 'a', 'ä': 'a', 'å': 'a',
	'ç': 'c',
	'è': 'e', 'é': 'e', 'ê': 'e', 'ë': 'e',
	'ì': 'i', 'í': 'i', 'î': 'i', 'ï': 'i',
	'ñ': 'n',
	'ò': 'o', 'ó': 'o', 'ô': 'o', 'õ': 'o', 'ö': 'o',
	'ù': 'u', 'ú': 'u', 'û': 'u', 'ü': 'u',
	'ý': 'y', 'ÿ': 'y',
	// common Latin Extended-A
	'ā': 'a', 'ă': 'a', 'ą': 'a',
	'ć': 'c', 'ĉ': 'c', 'ċ': 'c', 'č': 'c',
	'ď': 'd',
	'ē': 'e', 'ĕ': 'e', 'ė': 'e', 'ę': 'e', 'ě': 'e',
	'ĝ': 'g', 'ğ': 'g', 'ġ': 'g', 'ģ': 'g',
	'ĥ': 'h',
	'ī': 'i', 'ĭ': 'i', 'į': 'i', 'ı': 'i',
	'ĵ': 'j',
	'ķ': 'k',
	'ĺ': 'l', 'ļ': 'l', 'ľ': 'l',
	'ń': 'n', 'ņ': 'n', 'ň': 'n',
	'ō': 'o', 'ŏ': 'o', 'ő': 'o',
	'ŕ': 'r', 'ŗ': 'r', 'ř': 'r',
	'ś': 's', 'ŝ': 's', 'ş': 's', 'š': 's',
	'ţ': 't', 'ť': 't',
	'ũ': 'u', 'ū': 'u', 'ŭ': 'u', 'ů': 'u', 'ű': 'u', 'ų': 'u',
	'ŵ': 'w',
	'ŷ': 'y',
	'ź': 'z', 'ż': 'z', 'ž': 'z',
}
