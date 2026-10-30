package brainstore

// GenericQueryStopwords are generic English fillers and generic task verbs with
// no retrieval signal. It is the SINGLE source of truth shared by every query
// tokenizer — history/semantic/facts specialist search (QueryStopword) and
// cli's brain-brief filename matching (brainBriefFileMatchTerms) — so the
// common list never drifts between them.
// Each call site layers only its own domain-specific noise words on top; those
// legitimately differ (e.g. "regression"/"preserve" are noise for free-text
// history search but valid filename-match terms like regression.go for brief).
var GenericQueryStopwords = map[string]bool{
	"and": true, "are": true, "but": true, "can": true,
	"did": true, "does": true, "fix": true, "for": true, "from": true,
	"has": true, "have": true, "how": true, "into": true, "its": true,
	"make": true, "must": true, "need": true, "new": true, "not": true,
	"run": true, "runs": true, "should": true, "than": true, "that": true,
	"the": true, "then": true, "this": true, "use": true, "via": true,
	"want": true, "what": true, "when": true, "where": true, "which": true,
	"why": true, "with": true, "would": true,
}

func QueryStopword(term string) bool {
	if GenericQueryStopwords[term] {
		return true
	}
	// History/semantic-search-specific noise words layered on the generic set:
	// natural-language filler plus domain verbs/nouns ("regression", "preserve",
	// "restore", ...) that carry no signal for a free-text record search.
	switch term {
	case "a", "an", "as", "at", "be", "been", "by", "current", "do",
		"during", "existing", "in", "is", "it", "keep", "local", "of", "on",
		"only", "or", "other", "previous", "prior", "preserve", "regression",
		"restore", "same", "task", "to", "without":
		return true
	case "behavior", "entire", "fresh", "parent", "setting", "value", "values":
		return true
	case "done", "over", "instead", "we", "our", "could", "about":
		return true
	default:
		return false
	}
}
