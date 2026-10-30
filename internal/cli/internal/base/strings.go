package base

import (
	"strings"
	"unicode/utf8"
)

func TruncateString(value string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(value) <= max {
		return value
	}
	// Cut on a rune boundary so we never split a multi-byte UTF-8 rune in
	// real (non-ASCII) transcripts, which would emit invalid UTF-8.
	limit := max
	suffix := ""
	if max > 3 {
		limit = max - 3
		suffix = "..."
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut] + suffix
}

func FirstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// NonEmptyOr returns s, or fallback when s is empty.
func NonEmptyOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
