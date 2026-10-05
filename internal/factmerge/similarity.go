package factmerge

import (
	"math"
	"strings"
	"unicode"
)

// PromoteConfidenceCeiling is the highest confidence Promote will ever stamp on
// a conflict proposal.
//
// ConflictConfidence below is a LEXICAL measure: it is evidence that two
// statements are about the same claim. It is NOT, and cannot be, evidence that
// the candidate should replace the target — no deterministic rule can know
// that, which is why the proposal exists for a human at all. Clamping strictly
// under DefaultConfidenceThreshold keeps a promote-raised proposal permanently
// ineligible for any automatic application, so a high lexical overlap can never
// be mistaken (by code or by a reader) for a machine judgment that the
// supersede is correct.
const PromoteConfidenceCeiling = DefaultConfidenceThreshold - 0.05

// conflictPathWeight / conflictTextWeight split ConflictConfidence between the
// two signals. Text carries most of the weight: every conflicting pair shares a
// taxonomy path by construction, so the path term is close to a constant floor
// and only the wording discriminates.
const (
	conflictPathWeight = 0.3
	conflictTextWeight = 0.7
)

// ConflictConfidence reports, in [0, PromoteConfidenceCeiling], the
// deterministic lexical evidence that two facts state the same claim:
//
//	0.3 * jaccard(paths) + 0.7 * jaccard(text tokens)
//
// clamped to the ceiling and rounded to two decimals (the precision every
// surface displays, so the stored number and the printed one never disagree).
// It is pure, allocation-bounded, and identical on every machine — the same two
// records always score the same, which is what makes a promote reproducible.
func ConflictConfidence(a, b Record) float64 {
	score := conflictPathWeight*jaccard(stringSet(a.Paths), stringSet(b.Paths)) +
		conflictTextWeight*jaccard(textTokens(a.Text), textTokens(b.Text))
	return math.Round(math.Min(score, PromoteConfidenceCeiling)*100) / 100
}

// jaccard is |a ∩ b| / |a ∪ b|, and 0 when both sets are empty (two facts that
// say nothing are not evidence of each other).
func jaccard(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	intersection := 0
	for k := range a {
		if _, ok := b[k]; ok {
			intersection++
		}
	}
	union := len(a) + len(b) - intersection
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

func stringSet(values []string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, v := range values {
		if v != "" {
			out[v] = struct{}{}
		}
	}
	return out
}

// textTokens lowercases and splits on every non-alphanumeric rune. No stopword
// list and no stemming: both would be an arbitrary, language-specific judgment
// baked into a core that is meant to be auditable and LLM-free.
func textTokens(text string) map[string]struct{} {
	fields := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	return stringSet(fields)
}

// conflictComponents is the union-find Promote uses to queue a SPANNING set of
// conflict proposals instead of one per conflicting pair. Facts sharing a
// taxonomy path form a clique, and a clique of n facts needs only n-1 edges to
// stay one reviewable component; emitting all n(n-1)/2 of them is the quadratic
// fanout, not extra information.
type conflictComponents struct {
	parent map[string]string
}

func newConflictComponents() *conflictComponents {
	return &conflictComponents{parent: map[string]string{}}
}

func (c *conflictComponents) root(id string) string {
	root := id
	for {
		p, ok := c.parent[root]
		if !ok {
			c.parent[root] = root
			break
		}
		if p == root {
			break
		}
		root = p
	}
	for id != root {
		p := c.parent[id]
		c.parent[id] = root
		id = p
	}
	return root
}

// connected reports whether a and b are already in one proposal component, i.e.
// whether a reviewer following the queued proposals would already see them
// together.
func (c *conflictComponents) connected(a, b string) bool {
	return c.root(a) == c.root(b)
}

func (c *conflictComponents) union(a, b string) {
	ra, rb := c.root(a), c.root(b)
	if ra == rb {
		return
	}
	// Lowest id wins so the component shape is independent of visit order.
	if ra < rb {
		c.parent[rb] = ra
		return
	}
	c.parent[ra] = rb
}
