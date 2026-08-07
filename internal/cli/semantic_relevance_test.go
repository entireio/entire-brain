package cli

import (
	"math"
	"testing"
	"time"
)

func semanticCalibrationFacts() []factRecord {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	return []factRecord{
		{ID: "whitespace", Text: "indentation uses space characters rather than tab stops", Paths: []string{"conventions.formatting.whitespace"}, Status: factStatusActive, UpdatedAt: now},
		{ID: "deployment", Text: "the nightly deployment pipeline publishes release artifacts", Paths: []string{"delivery.release.deployment"}, Status: factStatusActive, UpdatedAt: now},
		{ID: "reviews", Text: "code review requires two approvals before merge", Paths: []string{"workflow.code_review.approvals"}, Status: factStatusActive, UpdatedAt: now},
		{ID: "retry", Text: "network retries use exponential backoff with bounded jitter", Paths: []string{"network.resilience.retry"}, Status: factStatusActive, UpdatedAt: now},
		{ID: "database", Text: "database migrations run inside a transaction before startup", Paths: []string{"storage.database.migrations"}, Status: factStatusActive, UpdatedAt: now},
		{ID: "palette", Text: "the dashboard palette uses blue green and yellow accents", Paths: []string{"frontend.visual.palette"}, Status: factStatusActive, UpdatedAt: now},
		{ID: "authentication", Text: "access tokens expire after thirty minutes and require refresh", Paths: []string{"security.authentication.credentials"}, Status: factStatusActive, UpdatedAt: now},
		{ID: "payments", Text: "payment webhooks are verified with an HMAC signature", Paths: []string{"integrations.payments.webhooks"}, Status: factStatusActive, UpdatedAt: now},
		{ID: "cache", Text: "cache entries are invalidated when the schema version changes", Paths: []string{"storage.cache.invalidation"}, Status: factStatusActive, UpdatedAt: now},
		{ID: "logging", Text: "production logs redact email addresses and authorization headers", Paths: []string{"observability.logging.redaction"}, Status: factStatusActive, UpdatedAt: now},
		{ID: "pagination", Text: "list endpoints use cursor pagination instead of numeric offsets", Paths: []string{"api.pagination.cursors"}, Status: factStatusActive, UpdatedAt: now},
		{ID: "localization", Text: "user-facing strings are translated from message catalogs", Paths: []string{"frontend.localization.messages"}, Status: factStatusActive, UpdatedAt: now},
	}
}

func permissiveSemanticBackgrounds(count int) []float64 {
	backgrounds := make([]float64, count)
	for index := range backgrounds {
		backgrounds[index] = -1
	}
	return backgrounds
}

func TestSemanticCalibrationPreservesTermDisjointFactsAndSilencesNeutralTasks(t *testing.T) {
	e := defaultEmbedder()
	if e == nil {
		t.Skip("embedding backend unavailable")
	}
	facts := semanticCalibrationFacts()
	positives := []struct {
		query  string
		target string
	}{
		{"what whitespace convention do we follow", "whitespace"},
		{"how many people must sign off before integration", "reviews"},
		{"where do build outputs get uploaded overnight", "deployment"},
		{"when must a credential be renewed", "authentication"},
	}
	for _, test := range positives {
		got := rankFactsFused(facts, test.query, 3, false, newSemanticReranker(e))
		if len(got) == 0 || got[0].ID != test.target {
			qvec := embedQueryWith(e, test.query)
			vectors := make([][]float32, len(facts))
			cosines := make([]float64, len(facts))
			for index, fact := range facts {
				vectors[index] = e.Embed(factEmbeddingText(fact))
				cosines[index] = cosineFloat32(qvec, vectors[index])
			}
			t.Fatalf(
				"query %q: got %+v, want %s first; cosines=%v backgrounds=%v",
				test.query, got, test.target, cosines,
				semanticLeaveOneOutBackgrounds(qvec, vectors),
			)
		}
	}
	neutral := []string{
		"which module serializes tabular downloads into comma-separated rows",
		"how are image thumbnails resized during upload",
		"why does the command renderer retain escape sequences when stdout is redirected",
		"where is the GraphQL subscription transport configured",
		"how does the parser recover from malformed nested delimiters",
		"where does the scheduler coalesce duplicate timer callbacks",
	}
	for _, query := range neutral {
		if got := rankFactsFused(facts, query, 3, false, newSemanticReranker(e)); len(got) != 0 {
			qvec := embedQueryWith(e, query)
			vectors := make([][]float32, len(facts))
			cosines := make([]float64, len(facts))
			for index, fact := range facts {
				vectors[index] = e.Embed(factEmbeddingText(fact))
				cosines[index] = cosineFloat32(qvec, vectors[index])
			}
			t.Fatalf(
				"neutral query %q returned semantic-only facts: %+v; cosines=%v backgrounds=%v",
				query, got, cosines, semanticLeaveOneOutBackgrounds(qvec, vectors),
			)
		}
	}
}

func TestSemanticCalibrationReachesTermDisjointFactWithDeterministicEmbedder(t *testing.T) {
	query := "whitespace convention"
	facts := []factRecord{
		{ID: "target", Text: "indentation uses spaces", Status: factStatusActive},
		{ID: "n1", Text: "release artifacts", Status: factStatusActive},
		{ID: "n2", Text: "review approvals", Status: factStatusActive},
		{ID: "n3", Text: "database migrations", Status: factStatusActive},
		{ID: "n4", Text: "cache invalidation", Status: factStatusActive},
		{ID: "n5", Text: "payment webhooks", Status: factStatusActive},
	}
	e := &fixedEmbedder{dim: 2, vecs: map[string][]float32{
		query:                     {1, 0},
		"indentation uses spaces": {1, 0},
		"release artifacts":       {0, 1},
		"review approvals":        {0, 1},
		"database migrations":     {0, 1},
		"cache invalidation":      {0, -1},
		"payment webhooks":        {-1, 0},
	}}
	got := rankFactsFused(facts, query, 3, false, newSemanticReranker(e))
	if len(got) == 0 || got[0].ID != "target" {
		t.Fatalf("deterministic semantic gate missed term-disjoint target: %+v", got)
	}
}

func TestSemanticCalibrationRequiresCorpusAndPreservesExactTailTies(t *testing.T) {
	if got := calibratedSemanticMask([]float64{1, 0}, permissiveSemanticBackgrounds(2)); got[0] || got[1] {
		t.Fatalf("an uncalibrated two-item corpus must not emit semantic-only hits: %v", got)
	}
	got := calibratedSemanticMask(
		[]float64{1, 1, 0, 0, 0, 0}, permissiveSemanticBackgrounds(6),
	)
	if !got[0] || !got[1] {
		t.Fatalf("equally strong upper-tail candidates should survive together: %v", got)
	}
	for _, keep := range got[2:] {
		if keep {
			t.Fatalf("background candidates escaped the exact-tie tail: %v", got)
		}
	}
}

func TestSemanticCalibrationRejectsFlatNoise(t *testing.T) {
	cosines := []float64{0.101, 0.1, 0.1, 0.1}
	for _, keep := range calibratedSemanticMask(cosines, permissiveSemanticBackgrounds(len(cosines))) {
		if keep {
			t.Fatalf("flat distribution escaped calibration: %v", cosines)
		}
	}
}

func TestSemanticCalibrationRequiresMinimumLeadOverCorpusDirection(t *testing.T) {
	cosines := []float64{0.2, 0.1, 0.1, 0.1}
	tooClose := []float64{0.176, 0, 0, 0}
	if calibratedSemanticMask(cosines, tooClose)[0] {
		t.Fatalf("candidate with only a 0.024 background lead must fail closed")
	}
	separated := []float64{0.174, 0, 0, 0}
	if !calibratedSemanticMask(cosines, separated)[0] {
		t.Fatalf("candidate with a 0.026 background lead should remain eligible")
	}
}

func TestSemanticCalibrationAdmitsNearTiedAndBimodalUpperClusters(t *testing.T) {
	nearTied := calibratedSemanticMask(
		[]float64{0.62, 0.61, 0.1, 0.1, 0.1, 0.1}, permissiveSemanticBackgrounds(6),
	)
	if !nearTied[0] || !nearTied[1] {
		t.Fatalf("near-tied relevant cluster was suppressed: %v", nearTied)
	}
	for _, keep := range nearTied[2:] {
		if keep {
			t.Fatalf("near-tied background escaped calibration: %v", nearTied)
		}
	}

	bimodalScores := []float64{
		0.8, 0.8, 0.8, 0.8, 0.8, 0.8,
		0.1, 0.1, 0.1, 0.1, 0.1, 0.1,
	}
	bimodal := calibratedSemanticMask(
		bimodalScores, permissiveSemanticBackgrounds(len(bimodalScores)),
	)
	for index, keep := range bimodal {
		if keep != (index < 6) {
			t.Fatalf("bimodal upper cluster mismatch at %d: %v", index, bimodal)
		}
	}
}

func TestSemanticCalibrationFailsClosedForMajorityRelevantCorpus(t *testing.T) {
	scores := []float64{
		0.8, 0.8, 0.8, 0.8, 0.8, 0.8, 0.8, 0.8,
		0.1, 0.1, 0.1, 0.1,
	}
	for _, keep := range calibratedSemanticMask(
		scores, permissiveSemanticBackgrounds(len(scores)),
	) {
		if keep {
			t.Fatalf("a broad majority-relevant topic must not become an automatic selective signal")
		}
	}
}

func TestSemanticBackgroundLeavesEachCandidateOut(t *testing.T) {
	backgrounds := semanticLeaveOneOutBackgrounds(
		[]float32{1, 0},
		[][]float32{{1, 0}, {0, 1}, {0, 1}},
	)
	if math.Abs(backgrounds[0]) > 1e-12 {
		t.Fatalf("target contaminated its own background: %v", backgrounds)
	}
	if math.Abs(backgrounds[1]-1/math.Sqrt(2)) > 1e-12 {
		t.Fatalf("leave-one-out background mismatch: %v", backgrounds)
	}
}

func TestDocumentVectorCalibrationUsesTheSameNeutralGate(t *testing.T) {
	e := defaultEmbedder()
	if e == nil {
		t.Skip("embedding backend unavailable")
	}
	facts := semanticCalibrationFacts()
	index := docIndex{Records: make([]docRecord, len(facts))}
	for i, fact := range facts {
		index.Records[i] = docRecord{
			ID:      fact.ID,
			Path:    fact.Paths[0] + ".md",
			Heading: fact.Paths[0],
			Text:    fact.Text,
		}
	}
	positive := docsVectorRanked(
		t.TempDir(), index, "what whitespace convention do we follow", e, 3, true, nil,
	).ranked
	if len(positive) == 0 || positive[0].ID != "doc:whitespace" {
		t.Fatalf("term-disjoint doc was not recovered: %+v", positive)
	}
	neutral := docsVectorRanked(
		t.TempDir(), index, "how are image thumbnails resized before upload", e, 3, true, nil,
	).ranked
	if len(neutral) != 0 {
		t.Fatalf("neutral query returned semantic-only docs: %+v", neutral)
	}
}

func TestDocumentCalibrationPreservesSemanticOrderingForLexicalHits(t *testing.T) {
	query := "checkpoint policy"
	index := docIndex{Records: []docRecord{
		{ID: "both", Text: "checkpoint policy alpha"},
		{ID: "lexical", Text: "checkpoint policy beta"},
		{ID: "middle", Text: "checkpoint policy gamma"},
		{ID: "semantic-only", Text: "save state rule"},
		{ID: "neutral-a", Text: "release artifact"},
		{ID: "neutral-b", Text: "payment webhook"},
	}}
	e := &fixedEmbedder{dim: 2, vecs: map[string][]float32{
		query:                     {1, 0},
		"checkpoint policy alpha": {1, 0},
		"checkpoint policy beta":  {0, 1},
		"checkpoint policy gamma": {0.8, 0.6},
		"save state rule":         {1, 0},
		"release artifact":        {0, 1},
		"payment webhook":         {-1, 0},
	}}
	lexical := map[string]struct{}{"both": {}, "lexical": {}, "middle": {}}
	got := docsVectorRanked(t.TempDir(), index, query, e, 4, true, lexical)
	if len(got.ranked) != 4 || got.ranked[0].ID != "doc:both" {
		t.Fatalf("doc semantic arm lost lexical-hit reordering: %+v", got.ranked)
	}
	if len(got.calibratedSemanticOnly) != 1 || got.calibratedSemanticOnly[0].ID != "doc:semantic-only" {
		t.Fatalf("doc calibration arm did not isolate semantic-only evidence: %+v", got.calibratedSemanticOnly)
	}
}

func TestDocumentCalibrationArmHasAnIndependentLimit(t *testing.T) {
	query := "durable retrieval policy"
	index := docIndex{Records: []docRecord{
		{ID: "lex-1", Text: "lex one"},
		{ID: "lex-2", Text: "lex two"},
		{ID: "lex-3", Text: "lex three"},
		{ID: "semantic", Text: "semantic only"},
	}}
	for i := 0; i < 8; i++ {
		index.Records = append(index.Records, docRecord{
			ID: "neutral-" + string(rune('a'+i)), Text: "neutral " + string(rune('a'+i)),
		})
	}
	unit := func(cosine float64) []float32 {
		return []float32{float32(cosine), float32(math.Sqrt(1 - cosine*cosine))}
	}
	vectors := map[string][]float32{
		query:           {1, 0},
		"lex one":       unit(1),
		"lex two":       unit(0.999),
		"lex three":     unit(0.998),
		"semantic only": unit(0.997),
	}
	for i := 0; i < 8; i++ {
		vectors["neutral "+string(rune('a'+i))] = []float32{0, 1}
	}
	e := &fixedEmbedder{dim: 2, vecs: vectors}
	lexical := map[string]struct{}{"lex-1": {}, "lex-2": {}, "lex-3": {}}
	got := docsVectorRanked(t.TempDir(), index, query, e, 3, true, lexical)
	if len(got.ranked) != 3 {
		t.Fatalf("primary vector list length = %d, want 3", len(got.ranked))
	}
	if len(got.calibratedSemanticOnly) != 1 || got.calibratedSemanticOnly[0].ID != "doc:semantic" {
		t.Fatalf("semantic-only arm was truncated by primary list limit: %+v", got.calibratedSemanticOnly)
	}
}

func TestExplicitVectorSearchPreservesSmallCorpusNearestNeighbors(t *testing.T) {
	e := defaultEmbedder()
	if e == nil {
		t.Skip("embedding backend unavailable")
	}
	facts := []factRecord{
		{ID: "alpha", Text: "qmd retrieval alpha contract", Status: factStatusActive},
		{ID: "beta", Text: "qmd retrieval beta contract", Status: factStatusActive},
	}
	got := factsVectorRanked(
		t.TempDir(), "main", facts, "qmd retrieval beta", e, 1,
	)
	if len(got) != 1 || got[0].ID != "beta" {
		t.Fatalf("explicit vector search lost its nearest neighbor: %+v", got)
	}
	if automatic := rankFactsFused(
		facts, "unrelated image resizing", 1, false, newSemanticReranker(e),
	); len(automatic) != 0 {
		t.Fatalf("automatic semantic injection must fail closed without calibration: %+v", automatic)
	}
}

func TestExplicitDocumentVectorSearchIsRawCosineNearestNeighbor(t *testing.T) {
	query := "nearest"
	index := docIndex{Records: []docRecord{
		{ID: "best", Text: "alpha"},
		{ID: "middle", Text: "beta"},
		{ID: "last", Text: "gamma"},
	}}
	e := &fixedEmbedder{dim: 2, vecs: map[string][]float32{
		query:   {1, 0},
		"alpha": {1, 0},
		"beta":  {0.8, 0.6},
		"gamma": {0, 1},
	}}
	got := docsVectorRanked(t.TempDir(), index, query, e, 3, false, nil)
	if len(got.ranked) != 3 || got.ranked[0].ID != "doc:best" || got.ranked[1].ID != "doc:middle" || got.ranked[2].ID != "doc:last" {
		t.Fatalf("explicit doc vsearch changed raw cosine order: %+v", got.ranked)
	}
	if len(got.calibratedSemanticOnly) != 0 {
		t.Fatalf("explicit doc vsearch must not create a calibration arm: %+v", got.calibratedSemanticOnly)
	}
}

func TestSemanticCalibrationRejectsNonFiniteVectors(t *testing.T) {
	cosines := []float64{1, math.NaN(), 0, 0, 0}
	keep := calibratedSemanticMask(cosines, permissiveSemanticBackgrounds(len(cosines)))
	if !keep[0] || keep[1] {
		t.Fatalf("finite signal should survive while non-finite candidate is rejected: %v", keep)
	}
}

func TestRankFactsFusedReusesFactVectors(t *testing.T) {
	query := "whitespace convention"
	facts := []factRecord{
		{ID: "target", Text: "indentation uses spaces", Status: factStatusActive},
		{ID: "deploy", Text: "release artifacts publish nightly", Status: factStatusActive},
		{ID: "review", Text: "reviews need two approvals", Status: factStatusActive},
	}
	e := &fakeFusionEmbedder{vecs: map[string][]float32{
		query:                               {1, 0},
		"indentation uses spaces":           {1, 0},
		"release artifacts publish nightly": {0, 1},
		"reviews need two approvals":        {-1, 0},
	}}
	rr := newSemanticReranker(e)
	_ = rankFactsFused(facts, query, 3, false, rr)
	firstCalls := len(e.embeds)
	_ = rankFactsFused(facts, query, 3, false, rr)
	if added := len(e.embeds) - firstCalls; added != 1 {
		t.Fatalf("warm rerank should embed only its query, added calls = %d (%v)", added, e.embeds)
	}
}

func TestEmbeddingDocumentsCarryStableContext(t *testing.T) {
	fact := factRecord{Text: "tokens expire", Paths: []string{"security.authentication.credentials"}}
	if got := factEmbeddingText(fact); got != "tokens expire\nTopic: security.authentication.credentials" {
		t.Fatalf("fact embedding text = %q", got)
	}
	if factEmbeddingModelID("model") == "model" {
		t.Fatal("context-aware embeddings must use a new cache namespace")
	}
}

func TestFactEmbeddingCacheIdentityChangesWithContextPaths(t *testing.T) {
	text := "tokens expire"
	security := factRecordID(text, normalizeFactPaths([]string{"security.authentication.credentials"}))
	storage := factRecordID(text, normalizeFactPaths([]string{"storage.database.migrations"}))
	if security == storage {
		t.Fatalf("paths used by factEmbeddingText must also participate in cache identity")
	}
}
