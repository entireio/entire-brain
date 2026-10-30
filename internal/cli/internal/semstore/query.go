package semstore

import (
	"math"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
)

// QueryTokens reduces a free-text query to the distinct identifier-ish
// tokens worth searching the symbol index for. It powers the tokenized fallback
// so a natural-language task ("change how feeds are refreshed") matches symbols
// on its content words instead of only as a verbatim substring.
const (
	semanticQueryTokenCandidateLimit = 32
	semanticQueryTokenSearchLimit    = 8
	semanticQueryLeadingTokenReserve = 4
)

func QueryTokens(query string) []string {
	seen := map[string]struct{}{}
	var tokens []string
	add := func(token string) bool {
		if len(token) < 3 || brainstore.QueryStopword(token) {
			return true
		}
		if _, ok := seen[token]; ok {
			return true
		}
		seen[token] = struct{}{}
		tokens = append(tokens, token)
		return len(tokens) < semanticQueryTokenCandidateLimit
	}
	for _, raw := range strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_')
	}) {
		raw = strings.Trim(raw, "_")
		if !add(raw) {
			break
		}
		for _, variant := range QueryMorphologyVariants(raw) {
			if !add(variant) {
				return tokens
			}
		}
	}
	return tokens
}

// QueryMorphologyVariants bridges common task nouns and past-tense
// descriptions to identifier verbs without turning semantic lookup into an
// unrestricted stemmer. Code commonly uses Normalize/Validate/Authenticate
// while issue text says normalization/validation/authentication; these exact
// derivations preserve meaning and give IDF ranking the identifier-shaped term.
func QueryMorphologyVariants(token string) []string {
	switch token {
	case "environment", "environments", "environmental":
		// Environment is almost universally shortened to env in identifiers and
		// filenames (pluginEnv, cmd.Env, plugin_env.go). Preserve the full noun
		// and add its conventional code form so natural-language tasks can find
		// those symbols without requiring callers to guess the abbreviation.
		return []string{"env"}
	}
	for _, rule := range []struct {
		suffix      string
		replacement string
	}{
		{suffix: "ization", replacement: "ize"},
		{suffix: "isation", replacement: "ise"},
		{suffix: "ized", replacement: "ize"},
		{suffix: "ised", replacement: "ise"},
		{suffix: "ation", replacement: "ate"},
	} {
		if strings.HasSuffix(token, rule.suffix) && len(token) > len(rule.suffix)+2 {
			return []string{strings.TrimSuffix(token, rule.suffix) + rule.replacement}
		}
	}
	return nil
}

// TokenIDFWeight scores a token by inverse document frequency: a token matching
// few symbols (e.g. "authentication") is far more discriminating than one
// matching many (e.g. "handler", "work"), so it must contribute more to a
// symbol's relevance. Returns a small positive integer for use as a SQL literal
// weight. Smoothed so a token matching nothing still ranks above none.
func TokenIDFWeight(total, df int) int {
	if total < 0 {
		total = 0
	}
	if df < 0 {
		df = 0
	}
	idf := math.Log(float64(total+1)/float64(df+1)) + 1
	weight := int(math.Round(idf * 10))
	if weight < 1 {
		weight = 1
	}
	return weight
}

// QueryLooksLikePath reports whether a query is a file path rather than
// free text. Path queries must resolve exactly: tokenizing "missing/file.ts"
// into ["missing","file"] would otherwise match half the index on the common
// token "file" and report a bogus blast radius for a path that does not exist.
func QueryLooksLikePath(query string) bool {
	query = strings.TrimSpace(query)
	if query == "" {
		return false
	}
	// A slash alone is not enough: natural-language concepts, URLs, and prose
	// commonly contain one. Only explicit relative/absolute paths or values with
	// a recognized source extension suppress semantic token fallback.
	if strings.Contains(query, "://") || strings.ContainsAny(query, " \t\r\n") {
		return false
	}
	if strings.HasPrefix(query, "./") || strings.HasPrefix(query, "../") ||
		strings.HasPrefix(query, "/") || strings.HasPrefix(query, `\\`) {
		return true
	}
	switch strings.ToLower(filepath.Ext(query)) {
	case ".go", ".ts", ".tsx", ".js", ".jsx", ".py", ".rs", ".sql", ".java", ".rb", ".mjs", ".cjs":
		return true
	}
	return false
}
