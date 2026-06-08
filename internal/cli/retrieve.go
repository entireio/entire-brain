package cli

import (
	"sort"
	"strings"
)

// retrieve.go is the unified retrieval layer behind the qmd-aligned verbs
// (search/vsearch/query). It ranks across the brain's text layers — facts,
// history, docs — and merges the per-source ranked lists with RRF. Each layer
// keeps its own best ranker; RRF fuses them by rank so their incomparable raw
// scores don't fight.

type unifiedResult struct {
	Source  string  `json:"source"` // fact | history | doc
	ID      string  `json:"id"`     // prefixed, addressable by get/multi-get
	Path    string  `json:"path,omitempty"`
	Heading string  `json:"heading,omitempty"`
	Line    int     `json:"line,omitempty"`
	Text    string  `json:"text"`
	Score   float64 `json:"score"`
}

type retrievalMode int

const (
	modeLexical retrievalMode = iota // search
	modeVector                       // vsearch
	modeHybrid                       // query
)

func embedQueryWith(e Embedder, q string) []float32 {
	if qe, ok := e.(queryEmbedder); ok {
		return qe.EmbedQuery(q)
	}
	return e.Embed(q)
}

// retrieveUnified ranks across facts + history + docs. Lexical (search) and
// hybrid (query) include all three; vector (vsearch) covers facts + docs —
// history vectors are Model2Vec noise and embedding tens of thousands of records
// per query is too slow, so they join the vector arm once Stage 1b's embedder
// lands. Missing layers are skipped, not errors.
func retrieveUnified(brainDir, branch, query string, limit int, mode retrievalMode) []unifiedResult {
	if limit <= 0 {
		limit = 10
	}
	all, _ := loadFacts(brainDir, branch)
	active := make([]factRecord, 0, len(all))
	for _, f := range all {
		if f.Status == factStatusActive {
			active = append(active, f)
		}
	}
	var e Embedder
	if mode != modeLexical {
		e = defaultEmbedder()
	}

	var lists [][]unifiedResult

	// Facts.
	if len(active) > 0 {
		switch mode {
		case modeLexical:
			lists = append(lists, factsToUnified(rankFacts(active, query, limit*2, false)))
		case modeVector:
			if e != nil {
				lists = append(lists, factsToUnified(factsVectorRanked(active, query, e, limit*2)))
			}
		case modeHybrid:
			var rr *semanticReranker
			if e != nil {
				rr = newSemanticReranker(e)
			}
			lists = append(lists, factsToUnified(rankFactsFused(active, query, limit*2, false, rr)))
		}
	}

	// History — BM25 (lexical / hybrid only).
	if mode != modeVector {
		if manifest, err := loadBrainManifest(brainDir); err == nil && manifest.Sources != nil && manifest.Sources.History != nil {
			if index, err := loadBrainHistoryIndex(brainDir, manifest.Sources.History); err == nil {
				if scored, ok := rankHistoryViaFTS(brainDir, index, "history", query, limit*2); ok {
					lists = append(lists, historyToUnified(scored))
				}
			}
		}
	}

	// Docs — all modes.
	if index, err := loadDocIndex(brainDir); err == nil && len(index.Records) > 0 {
		if mode == modeVector {
			if e != nil {
				lists = append(lists, docsVectorRanked(index, query, e, limit*2))
			}
		} else if scored, ok := rankDocsViaFTS(brainDir, index, query, limit*2); ok {
			lists = append(lists, docsToUnified(scored))
		}
	}

	return rrfMergeUnified(lists, limit)
}

func factsVectorRanked(facts []factRecord, query string, e Embedder, limit int) []factRecord {
	qv := embedQueryWith(e, query)
	type sc struct {
		i   int
		cos float64
	}
	scored := make([]sc, len(facts))
	for i := range facts {
		scored[i] = sc{i, cosineFloat32(qv, e.Embed(facts[i].Text))}
	}
	sort.Slice(scored, func(a, b int) bool { return scored[a].cos > scored[b].cos })
	out := make([]factRecord, 0, min(limit, len(scored)))
	for _, s := range scored {
		out = append(out, facts[s.i])
		if len(out) >= limit {
			break
		}
	}
	return out
}

func docsVectorRanked(index docIndex, query string, e Embedder, limit int) []unifiedResult {
	qv := embedQueryWith(e, query)
	type sc struct {
		i   int
		cos float64
	}
	scored := make([]sc, len(index.Records))
	for i := range index.Records {
		scored[i] = sc{i, cosineFloat32(qv, e.Embed(index.Records[i].Text))}
	}
	sort.Slice(scored, func(a, b int) bool { return scored[a].cos > scored[b].cos })
	out := make([]unifiedResult, 0, min(limit, len(scored)))
	for _, s := range scored {
		out = append(out, docToUnified(index.Records[s.i]))
		if len(out) >= limit {
			break
		}
	}
	return out
}

func factsToUnified(facts []factRecord) []unifiedResult {
	// fact ids are already prefixed "fact:" (factRecordID); history likewise.
	out := make([]unifiedResult, len(facts))
	for i, f := range facts {
		out[i] = unifiedResult{Source: "fact", ID: f.ID, Path: strings.Join(f.Paths, ","), Text: f.Text}
	}
	return out
}

func historyToUnified(scored []scoredHistoryRecord) []unifiedResult {
	out := make([]unifiedResult, len(scored))
	for i, s := range scored {
		out[i] = unifiedResult{Source: "history", ID: s.Record.ID, Path: s.Record.Path, Line: s.Record.Line, Heading: s.Record.Kind, Text: s.Record.Summary}
	}
	return out
}

func docToUnified(r docRecord) unifiedResult {
	return unifiedResult{Source: "doc", ID: "doc:" + r.ID, Path: r.Path, Line: r.Line, Heading: r.Heading, Text: r.Text}
}

func docsToUnified(scored []scoredDocRecord) []unifiedResult {
	out := make([]unifiedResult, len(scored))
	for i, s := range scored {
		out[i] = docToUnified(s.Record)
	}
	return out
}

func rrfMergeUnified(lists [][]unifiedResult, limit int) []unifiedResult {
	fused := map[string]float64{}
	rec := map[string]unifiedResult{}
	for _, list := range lists {
		for rank, r := range list {
			fused[r.ID] += 1.0 / (rrfK + float64(rank+1))
			if _, ok := rec[r.ID]; !ok {
				rec[r.ID] = r
			}
		}
	}
	out := make([]unifiedResult, 0, len(rec))
	for id, r := range rec {
		r.Score = fused[id]
		out = append(out, r)
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Score != out[b].Score {
			return out[a].Score > out[b].Score
		}
		return out[a].ID < out[b].ID
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// getUnified resolves a prefixed id (fact:/history:/doc:) to its full record.
func getUnified(brainDir, branch, id string) (unifiedResult, bool) {
	switch {
	case strings.HasPrefix(id, "fact:"):
		facts, _ := loadFacts(brainDir, branch)
		for _, f := range facts {
			if f.ID == id {
				return factsToUnified([]factRecord{f})[0], true
			}
		}
	case strings.HasPrefix(id, "history:"):
		if manifest, err := loadBrainManifest(brainDir); err == nil && manifest.Sources != nil && manifest.Sources.History != nil {
			if index, err := loadBrainHistoryIndex(brainDir, manifest.Sources.History); err == nil {
				for _, r := range index.Records {
					if r.ID == id {
						return historyToUnified([]scoredHistoryRecord{{Record: r}})[0], true
					}
				}
			}
		}
	case strings.HasPrefix(id, "doc:"):
		want := strings.TrimPrefix(id, "doc:")
		if index, err := loadDocIndex(brainDir); err == nil {
			for _, r := range index.Records {
				if r.ID == want {
					return docToUnified(r), true
				}
			}
		}
	}
	return unifiedResult{}, false
}
