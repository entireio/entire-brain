package cli

import (
	"fmt"
	"sort"
)

// history_vec.go is the production wiring of the history semantic arm the
// 2026-06-12 capstone validated (docs/eval_ledger.md): EmbeddingGemma-fused
// history retrieval beat BM25 by ~13% useful/1k on two repos, while the
// bundled Model2Vec measured significantly *below* BM25 — a closed negative.
// Three consequences shape this file:
//
//   - The arm is gated on a fusion-eligible embedder (the historyFusionEligible
//     marker, implemented by the external ollama/EmbeddingGemma embedder and
//     never by Model2Vec).
//   - Record vectors are embedded at refresh time into a persistent vec0 store
//     (syncHistoryVectors), never lazily at query time: a large repo holds
//     hundreds of thousands of records, hours of embedding that must not land
//     on a query. Records newer than the last refresh simply have no vector
//     until the next one — the lexical arm still sees them.
//   - The store is brain_cgo-only (newHistoryVectorStore returns ok=false on
//     the pure-Go build): at this row count the vectors.bin flat file +
//     brute-force scan would be loaded whole on every query, so without vec0
//     the gate stays closed and history stays BM25-only.

// historyVectorStore is the persistent store for history record vectors.
// Unlike the facts vectorStore (a full-rewrite cache sized for hundreds of
// rows) it must scale to hundreds of thousands, so its writes are incremental:
// upsert new ids, drop departed ones, never rewrite the present set.
type historyVectorStore interface {
	// ids returns the stored record-id set; ok=false means the store is absent
	// or was built for another model/dim (callers treat both as empty: every
	// wanted vector is missing and a sync rebuilds from scratch).
	ids() (map[string]struct{}, bool)
	upsert(add map[string][]float32, drop []string) error
	knnCos(qvec []float32) (map[string]float64, bool)
}

// historyFusionEligible marks embedders validated for history fusion. The
// capstone measured Model2Vec fusion significantly below BM25 on history
// precision at scale, so eligibility is opt-in per embedder type — a new
// embedder joins the history semantic arm by implementing this marker after
// it has an eval-ledger row, not by existing.
type historyFusionEligible interface {
	historyFusionEligible() bool
}

// historySemanticEmbedder filters e down to the history-fusion gate: it
// returns e only when e is fusion-eligible, nil otherwise (including nil e).
func historySemanticEmbedder(e Embedder) Embedder {
	if fe, ok := e.(historyFusionEligible); ok && fe.historyFusionEligible() {
		return e
	}
	return nil
}

// historyVectorStoreFor resolves the persistent store for a gate-filtered
// embedder. A nil e (gate closed) reports no store without consulting the
// build, so callers can branch on "why" — gate vs build — for messaging.
func historyVectorStoreFor(brainDir string, e Embedder) (historyVectorStore, bool) {
	if e == nil {
		return nil, false
	}
	return newHistoryVectorStore(brainDir, e.ID(), e.Dim())
}

// historySemanticScores embeds the query and runs one KNN over the persisted
// history vectors. nil means the semantic arm is unavailable for this query
// (gate closed upstream, no store, model/dim mismatch, embedder down) — every
// caller falls back to lexical-only, so a degraded arm never breaks ranking.
func historySemanticScores(brainDir string, e Embedder, query string) map[string]float64 {
	if e == nil {
		return nil
	}
	store, ok := newHistoryVectorStore(brainDir, e.ID(), e.Dim())
	if !ok {
		return nil
	}
	qvec := embedQueryWith(e, query)
	if len(qvec) == 0 {
		return nil
	}
	scores, ok := store.knnCos(qvec)
	if !ok {
		return nil
	}
	return scores
}

// rankHistorySemantic orders the index's rankable records by the cosine scores
// of one historySemanticScores call. Request records are excluded and duplicate
// summaries collapsed, exactly like the lexical scorers, so the semantic arm
// can never resurface what they deliberately hide. Records without a stored
// vector (added since the last refresh) are skipped, not zero-scored.
func rankHistorySemantic(index historyIndex, scores map[string]float64, limit int) []scoredHistoryRecord {
	if len(scores) == 0 || limit <= 0 {
		return nil
	}
	type cand struct {
		rec   historyRecord
		cos   float64
		order int
	}
	cands := make([]cand, 0, min(limit, len(scores)))
	seen := map[string]struct{}{}
	for i, r := range index.Records {
		if r.Kind == "request" {
			continue
		}
		cos, ok := scores[r.ID]
		if !ok {
			continue
		}
		key := normalizeHistorySearchText(r.Summary)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		cands = append(cands, cand{rec: r, cos: cos, order: i})
	}
	sort.SliceStable(cands, func(a, b int) bool {
		if cands[a].cos != cands[b].cos {
			return cands[a].cos > cands[b].cos
		}
		return cands[a].rec.ID < cands[b].rec.ID
	})
	if len(cands) > limit {
		cands = cands[:limit]
	}
	out := make([]scoredHistoryRecord, len(cands))
	for i, c := range cands {
		// Same float→int display scaling as rankHistoryViaFTS; the list is
		// already ordered, Score is informational.
		out[i] = scoredHistoryRecord{Record: c.rec, Score: int(c.cos*1000 + 0.5), Order: c.order}
	}
	return out
}

// rankHistoryFused is rankHistoryViaFTS with the validated semantic arm fused
// in via RRF (the capstone's exact shape: lexical ranks over-fetched 4×,
// semantic ranks over the full stored candidate set, k=60, equal weight). When
// the gate is closed or the store is unavailable it degrades to exactly
// rankHistoryViaFTS — same list, same ok contract — so call sites need no
// fallback of their own beyond what they already have.
func rankHistoryFused(brainDir string, index historyIndex, kind, query string, limit int, e Embedder) ([]scoredHistoryRecord, bool) {
	scores := historySemanticScores(brainDir, historySemanticEmbedder(e), query)
	if len(scores) == 0 {
		return rankHistoryViaFTS(brainDir, index, kind, query, limit)
	}
	lex, lexOK := rankHistoryViaFTS(brainDir, index, kind, query, limit*4)
	if !lexOK {
		// FTS down but vectors up: rank on the semantic arm alone rather than
		// reporting the whole indexed path unavailable (which would drop the
		// caller to the substring scorer the eval retired).
		sem := rankHistorySemantic(index, scores, limit)
		return sem, len(sem) > 0
	}
	sem := rankHistorySemantic(index, scores, limit*4)
	type fusedRec struct {
		s     scoredHistoryRecord
		score float64
	}
	fused := map[string]*fusedRec{}
	for _, list := range [][]scoredHistoryRecord{lex, sem} {
		for rank, s := range list {
			f, ok := fused[s.Record.ID]
			if !ok {
				f = &fusedRec{s: s}
				fused[s.Record.ID] = f
			}
			f.score += 1.0 / (rrfK + float64(rank+1))
		}
	}
	out := make([]*fusedRec, 0, len(fused))
	for _, f := range fused {
		out = append(out, f)
	}
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].score != out[b].score {
			return out[a].score > out[b].score
		}
		return out[a].s.Record.ID < out[b].s.Record.ID
	})
	if len(out) > limit {
		out = out[:limit]
	}
	result := make([]scoredHistoryRecord, len(out))
	for i, f := range out {
		// Same float→int display scaling as rankHistoryViaFTS (RRF scores are
		// small; ordering is carried by list position, Score is informational).
		result[i] = scoredHistoryRecord{Record: f.s.Record, Score: int(f.score*1000 + 0.5), Order: f.s.Order}
	}
	return result, true
}

// syncHistoryVectors brings the persisted store in line with the index:
// embed records that are rankable but unstored, drop stored ids the index no
// longer contains. Writes are batched so an interrupted backfill (the first
// sync of a large repo embeds every record and can run for hours) resumes
// where it stopped instead of starting over. Identical summaries are embedded
// once per sync and the vector shared across their record ids — templated
// session lines repeat heavily, and the store is keyed by record id so KNN
// results map straight back to records.
func syncHistoryVectors(store historyVectorStore, index historyIndex, e Embedder, progress func(done, total int)) (added, dropped, total int, err error) {
	existing, _ := store.ids() // !ok reads as empty: a fresh or mismatched store rebuilds
	want := map[string]struct{}{}
	for _, r := range index.Records {
		if r.Kind == "request" {
			continue
		}
		want[r.ID] = struct{}{}
	}
	var drop []string
	for id := range existing {
		if _, ok := want[id]; !ok {
			drop = append(drop, id)
		}
	}
	var missing []historyRecord
	for _, r := range index.Records {
		if r.Kind == "request" {
			continue
		}
		if _, ok := existing[r.ID]; !ok {
			missing = append(missing, r)
		}
	}
	if len(drop) > 0 {
		if err := store.upsert(nil, drop); err != nil {
			return 0, 0, 0, err
		}
	}
	const flushEvery = 1024
	bySummary := map[string][]float32{}
	batch := map[string][]float32{}
	for i, r := range missing {
		v, ok := bySummary[r.Summary]
		if !ok {
			v = e.Embed(r.Summary)
			if len(v) != e.Dim() {
				// Embedder fault mid-sync (server died, transient error). Flush
				// what we have so the next sync resumes here, then surface it.
				if ferr := store.upsert(batch, nil); ferr != nil {
					return added, len(drop), len(want), ferr
				}
				return added + len(batch), len(drop), len(want), fmt.Errorf("embedder returned no vector for record %s (embed server down mid-sync?); progress saved, re-run refresh to resume", r.ID)
			}
			bySummary[r.Summary] = v
		}
		batch[r.ID] = v
		if len(batch) >= flushEvery {
			if err := store.upsert(batch, nil); err != nil {
				return added, len(drop), len(want), err
			}
			added += len(batch)
			batch = map[string][]float32{}
			if progress != nil {
				progress(i+1, len(missing))
			}
		}
	}
	if len(batch) > 0 {
		if err := store.upsert(batch, nil); err != nil {
			return added, len(drop), len(want), err
		}
		added += len(batch)
	}
	if progress != nil && len(missing) > 0 {
		progress(len(missing), len(missing))
	}
	return added, len(drop), len(want), nil
}
