package cli

import (
	"fmt"
	"sort"
	"strings"
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
	// knnCos returns cosine similarity for the k nearest records only — unlike
	// the facts store's all-rows variant. vec0 caps MATCH k (and a history KNN
	// over hundreds of thousands of rows shouldn't materialize them all anyway);
	// callers over-fetch enough for downstream dedup, nothing more.
	knnCos(qvec []float32, k int) (map[string]float64, bool)
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
// history vectors. Automatic retrieval requests a fixed, bounded calibration
// neighborhood rather than tying its score distribution to the display limit:
// calibrating only the nearest limit*4 rows becomes increasingly flat as a
// history corpus grows and can suppress a real upper tail. Explicit vector
// search does not calibrate and retains its smaller limit*4 budget. The vec0
// store clamps either request to its row count and backend ceiling; downstream
// rankers apply the user-facing limit.
// nil means the semantic arm is unavailable for this query (gate closed
// upstream, no store, model/dim mismatch, embedder down) — every caller falls
// back to lexical-only, so a degraded arm never breaks ranking.
func historySemanticScores(brainDir string, e Embedder, query string, limit int, calibrate bool) map[string]float64 {
	if e == nil || limit <= 0 || !memoryProjectionVectorsCurrent(brainDir, e) {
		return nil
	}
	store, ok := newHistoryVectorStore(brainDir, e.ID(), e.Dim())
	if !ok {
		return nil
	}
	return historySemanticScoresWithStore(store, e, query, limit, calibrate)
}

func historySemanticScoresWithStore(store historyVectorStore, e Embedder, query string, limit int, calibrate bool) map[string]float64 {
	if store == nil || e == nil || limit <= 0 {
		return nil
	}
	qvec := embedQueryWith(e, query)
	if e.Dim() <= 0 || len(qvec) != e.Dim() || !vectorHasMagnitude(qvec) {
		return nil
	}
	scoreBudget := limit * 4
	if scoreBudget < limit { // integer overflow guard for unreasonable inputs
		scoreBudget = limit
	}
	if calibrate && scoreBudget < historySemanticCalibrationK {
		scoreBudget = historySemanticCalibrationK
	}
	scores, ok := store.knnCos(qvec, scoreBudget)
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
	return rankHistorySemanticFiltered(index, scores, limit, false, nil, false).ranked
}

// rankHistorySemanticRelevant is the automatic history arm. Unlike explicit
// vector search, it admits only a corpus-relative upper cluster.
func rankHistorySemanticRelevant(index historyIndex, scores map[string]float64, limit int) []scoredHistoryRecord {
	return rankHistorySemanticFiltered(index, scores, limit, true, nil, false).ranked
}

// rankHistorySemanticHybrid keeps every lexical hit in the semantic ranking so
// cosine can still reorder evidence already reached by lexical search. Only
// semantic-only candidates must clear corpus-relative calibration.
func rankHistorySemanticHybrid(index historyIndex, scores map[string]float64, limit int, lexicalIDs map[string]struct{}) []scoredHistoryRecord {
	return rankHistorySemanticHybridRanks(index, scores, limit, lexicalIDs).ranked
}

type historyVectorRanks struct {
	ranked                 []scoredHistoryRecord
	calibratedSemanticOnly []scoredHistoryRecord
}

// rankHistorySemanticHybridRanks gives a high-confidence semantic-only record
// the same two-list RRF opportunity as a record reached by both lexical and
// semantic search. The calibration-only list excludes lexical hits, so no
// record can receive three votes. Its independent limit also prevents lexical
// hits at the top of ranked from crowding all term-disjoint evidence out.
func rankHistorySemanticHybridRanks(index historyIndex, scores map[string]float64, limit int, lexicalIDs map[string]struct{}) historyVectorRanks {
	return rankHistorySemanticFiltered(index, scores, limit, true, lexicalIDs, true)
}

func rankHistorySemanticFiltered(
	index historyIndex,
	scores map[string]float64,
	limit int,
	requireRelevance bool,
	alwaysKeep map[string]struct{},
	collectCalibrationArm bool,
) historyVectorRanks {
	return rankSemanticFilteredKinds(index, scores, limit, requireRelevance, alwaysKeep, collectCalibrationArm, historyGeneralRankingHiddenKind)
}

// rankConversationSemantic ranks ONLY exchange records by cosine; the
// explicit conversation vector arm. Mirrors rankHistorySemantic: no relevance
// calibration for explicit vector search.
func rankConversationSemantic(index historyIndex, scores map[string]float64, limit int) []scoredHistoryRecord {
	return rankSemanticFilteredKinds(index, scores, limit, false, nil, false, conversationSemanticHiddenKind).ranked
}

// rankConversationSemanticHybridRanks is the conversation arm's calibrated
// hybrid ranking, mirroring rankHistorySemanticHybridRanks over exchanges only.
func rankConversationSemanticHybridRanks(index historyIndex, scores map[string]float64, limit int, lexicalIDs map[string]struct{}) historyVectorRanks {
	return rankSemanticFilteredKinds(index, scores, limit, true, lexicalIDs, true, conversationSemanticHiddenKind)
}

// conversationSemanticHiddenKind inverts the general gate: for the
// conversation arm, everything EXCEPT exchange records is hidden.
func conversationSemanticHiddenKind(kind string) bool {
	return kind != conversationKind
}

func rankSemanticFilteredKinds(
	index historyIndex,
	scores map[string]float64,
	limit int,
	requireRelevance bool,
	alwaysKeep map[string]struct{},
	collectCalibrationArm bool,
	hiddenKind func(string) bool,
) historyVectorRanks {
	if len(scores) == 0 || limit <= 0 {
		return historyVectorRanks{}
	}
	type cand struct {
		rec     historyRecord
		cos     float64
		order   int
		keep    bool
		lexical bool
	}
	cands := make([]cand, 0, min(limit, len(scores)))
	seen := map[string]struct{}{}
	for i, r := range index.Records {
		if hiddenKind(r.Kind) {
			continue
		}
		cos, ok := scores[r.ID]
		if !ok || !isFinite(cos) {
			continue
		}
		key := normalizeHistorySearchText(r.Summary)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		cands = append(cands, cand{rec: r, cos: cos, order: i})
	}
	if requireRelevance {
		cosines := make([]float64, len(cands))
		for i := range cands {
			cosines[i] = cands[i].cos
		}
		mask := semanticResultMask(
			cosines, true, semanticMedianBackgrounds(cosines),
		)
		for i := range cands {
			_, lexical := alwaysKeep[cands[i].rec.ID]
			cands[i].keep = mask[i]
			cands[i].lexical = lexical
		}
	} else {
		for i := range cands {
			cands[i].keep = true
		}
	}
	sort.SliceStable(cands, func(a, b int) bool {
		if cands[a].cos != cands[b].cos {
			return cands[a].cos > cands[b].cos
		}
		return cands[a].rec.ID < cands[b].rec.ID
	})
	out := historyVectorRanks{
		ranked:                 make([]scoredHistoryRecord, 0, min(limit, len(cands))),
		calibratedSemanticOnly: make([]scoredHistoryRecord, 0),
	}
	for _, c := range cands {
		// Same float→int display scaling as rankHistoryViaFTS; the list is
		// already ordered, Score is informational.
		record := scoredHistoryRecord{Record: c.rec, Score: int(c.cos*1000 + 0.5), Order: c.order}
		if (c.keep || c.lexical) && len(out.ranked) < limit {
			out.ranked = append(out.ranked, record)
		}
		if collectCalibrationArm && c.keep && !c.lexical && len(out.calibratedSemanticOnly) < limit {
			out.calibratedSemanticOnly = append(out.calibratedSemanticOnly, record)
		}
		if len(out.ranked) >= limit && (!collectCalibrationArm || len(out.calibratedSemanticOnly) >= limit) {
			break
		}
	}
	return out
}

// rankHistoryFused is rankHistoryViaFTS with validated semantic rankings fused
// in via RRF: lexical ranks over-fetched 4×, semantic ranks over the full
// stored candidate set, and a calibration-only list that restores two-list
// parity for term-disjoint evidence without triple-counting lexical hits. All
// lists use k=60 and equal weight. When
// the gate is closed or the store is unavailable it degrades to exactly
// rankHistoryViaFTS — same list, same ok contract — so call sites need no
// fallback of their own beyond what they already have.
func rankHistoryFused(brainDir string, index historyIndex, kind, query string, limit int, e Embedder) ([]scoredHistoryRecord, bool) {
	scores := historySemanticScores(brainDir, historySemanticEmbedder(e), query, limit, true)
	if len(scores) == 0 {
		return rankHistoryViaFTS(brainDir, index, kind, query, limit)
	}
	lex, lexOK := rankHistoryViaFTS(brainDir, index, kind, query, limit*4)
	if !lexOK {
		// FTS down but vectors up: rank on the semantic arm alone rather than
		// reporting the whole indexed path unavailable (which would drop the
		// caller to the substring scorer the eval retired).
		sem := rankHistorySemanticRelevant(index, scores, limit)
		return sem, len(sem) > 0
	}
	lexicalIDs := make(map[string]struct{}, len(lex))
	for _, scored := range lex {
		lexicalIDs[scored.Record.ID] = struct{}{}
	}
	sem := rankHistorySemanticHybridRanks(index, scores, limit*4, lexicalIDs)
	return fuseScoredRankLists([][]scoredHistoryRecord{lex, sem.ranked, sem.calibratedSemanticOnly}, limit), true
}

// fuseScoredRankLists is the shared RRF merge over already-ranked record lists
// (k=60, equal weight). Conversation records fuse by replacement-scoped
// identity, not globally by a potentially legacy/colliding ID.
func fuseScoredRankLists(lists [][]scoredHistoryRecord, limit int) []scoredHistoryRecord {
	type fusedRec struct {
		s     scoredHistoryRecord
		score float64
	}
	fused := map[historyRecordReplacementKey]*fusedRec{}
	for _, list := range lists {
		for rank, s := range list {
			key := recordReplacementKey(s.Record)
			f, ok := fused[key]
			if !ok {
				f = &fusedRec{s: s}
				fused[key] = f
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
		left, right := out[a].s.Record, out[b].s.Record
		if left.ID != right.ID {
			return left.ID < right.ID
		}
		if left.Branch != right.Branch {
			return left.Branch < right.Branch
		}
		if left.SessionID != right.SessionID {
			return left.SessionID < right.SessionID
		}
		if left.Path != right.Path {
			return left.Path < right.Path
		}
		if left.Line != right.Line {
			return left.Line < right.Line
		}
		// Kind and SourceDigest are the two remaining components of
		// historyRecordReplacementKey. Without them the comparator is not a
		// total order over its own fusion key: two entries the key deliberately
		// keeps distinct (degraded, session-less copies of one exchange, whose
		// key.Session falls back to the source digest; or ID-less legacy records
		// differing only by digest) compare equal, and sort.SliceStable then
		// leaves them in the order the fused map happened to range in — so the
		// same query returns a different ranking from one call to the next.
		if left.Kind != right.Kind {
			return left.Kind < right.Kind
		}
		return left.SourceDigest < right.SourceDigest
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
	return result
}

// conversationEmbeddingTextVersion versions the exchange embedding scheme.
// Bumping it changes the store identity, so a scheme change rebuilds the
// vectors cleanly instead of silently mixing embeddings of different texts.
// reqw1: request-weighted; the request line plus a bounded response head,
// instead of the full mixed 2 KiB projection (the 2026-08-07 calibration's
// reopen avenue: response tails diluted the exchange embeddings).
const conversationEmbeddingTextVersion = "reqw1"

func conversationVectorModelID(modelID string) string {
	return modelID + ":" + conversationEmbeddingTextVersion
}

// conversationEmbeddingText is the text an exchange embeds as: the request
// (the summary's first line) plus at most 512 bytes of response head. Queries
// are usually about what was asked or concluded; the long response tail mostly
// dilutes the vector.
func conversationEmbeddingText(r historyRecord) string {
	request, response, _ := strings.Cut(r.Summary, "\n")
	if strings.TrimSpace(response) == "" {
		return request
	}
	head, _ := truncateUTF8Bytes(response, 512)
	return request + "\n" + head
}

// conversationSemanticScores is historySemanticScores against the separate
// conversation vector store. nil means the conversation semantic arm is
// unavailable (gate closed, pure-Go build, absent/mismatched store, embedder
// down) and callers stay lexical.
func conversationSemanticScores(brainDir string, e Embedder, query string, limit int, calibrate bool) map[string]float64 {
	if e == nil || limit <= 0 || !memoryProjectionVectorsCurrent(brainDir, e) {
		return nil
	}
	store, ok := newConversationVectorStore(brainDir, conversationVectorModelID(e.ID()), e.Dim())
	if !ok {
		return nil
	}
	return historySemanticScoresWithStore(store, e, query, limit, calibrate)
}

// conversationSemanticScoresExhaustive scores a KNN neighborhood large enough
// to cover every stored conversation vector up to the filtered-scan ceiling.
// Structured filters require it: a bounded semantic candidate window has the
// same false-empty defect as the bounded lexical window. Honesty
// bound: the vec0 backend clamps K (vec0KnnMaxK, 4096), so past that many
// stored vectors the filtered vector arm is explicitly approximate, exactly
// as the plan's vector-mode contract allows; exact-filter completeness is
// carried by the lexical arm.
func conversationSemanticScoresExhaustive(brainDir string, e Embedder, query string, calibrate bool) map[string]float64 {
	if e == nil || !memoryProjectionVectorsCurrent(brainDir, e) {
		return nil
	}
	store, ok := newConversationVectorStore(brainDir, conversationVectorModelID(e.ID()), e.Dim())
	if !ok {
		return nil
	}
	return historySemanticScoresWithStore(store, e, query, historyFTSFilteredScanCeiling, calibrate)
}

// rankConversationFused is the conversation arm's hybrid ranking: exchange-kind
// BM25 fused with calibrated exchange vectors via the shared RRF merge. When
// the semantic arm is unavailable it degrades to exactly the lexical ranking
// (same list, same ok contract), so lexical-only operation stays fully
// supported. A non-nil pred pushes structured filters into both arms: the
// lexical arm filters during candidate generation against the full index (the
// FTS store identity must never see a filtered view), the semantic arms rank
// a filtered in-memory record slice over an exhaustive score neighborhood.
// complete=false reports a lexical scan that hit the filtered-scan ceiling.
func rankConversationFused(brainDir string, index historyIndex, query string, limit int, e Embedder, pred func(historyRecord) bool) ([]scoredHistoryRecord, bool, bool) {
	semIndex := index
	var scores map[string]float64
	if pred != nil {
		semIndex = historyIndex{GeneratedAt: index.GeneratedAt, Records: filterHistoryRecords(index.Records, pred)}
		scores = conversationSemanticScoresExhaustive(brainDir, historySemanticEmbedder(e), query, true)
	} else {
		scores = conversationSemanticScores(brainDir, historySemanticEmbedder(e), query, limit, true)
	}
	if len(scores) == 0 {
		return rankHistoryViaFTSFiltered(brainDir, index, conversationKind, query, limit, historyFTSRelevanceCutoff, pred)
	}
	lex, complete, lexOK := rankHistoryViaFTSFiltered(brainDir, index, conversationKind, query, limit*4, historyFTSRelevanceCutoff, pred)
	if !lexOK {
		sem := rankConversationSemantic(semIndex, scores, limit)
		return sem, true, len(sem) > 0
	}
	lexicalIDs := make(map[string]struct{}, len(lex))
	for _, scored := range lex {
		lexicalIDs[scored.Record.ID] = struct{}{}
	}
	sem := rankConversationSemanticHybridRanks(semIndex, scores, limit*4, lexicalIDs)
	return fuseScoredRankLists([][]scoredHistoryRecord{lex, sem.ranked, sem.calibratedSemanticOnly}, limit), complete, true
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
	return syncVectorsForKinds(store, index, e, historyGeneralRankingHiddenKind, func(r historyRecord) string { return r.Summary }, progress)
}

// syncConversationVectors maintains the separate conversation vector store:
// exchange records only, request-weighted embedding text, same batching/resume
// semantics as history vectors.
func syncConversationVectors(store historyVectorStore, index historyIndex, e Embedder, progress func(done, total int)) (added, dropped, total int, err error) {
	return syncVectorsForKinds(store, index, e, conversationSemanticHiddenKind, conversationEmbeddingText, progress)
}

func syncVectorsForKinds(store historyVectorStore, index historyIndex, e Embedder, hiddenKind func(string) bool, embedText func(historyRecord) string, progress func(done, total int)) (added, dropped, total int, err error) {
	existing, _ := store.ids() // !ok reads as empty: a fresh or mismatched store rebuilds
	want := map[string]struct{}{}
	for _, r := range index.Records {
		if hiddenKind(r.Kind) {
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
		if hiddenKind(r.Kind) {
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
		text := embedText(r)
		v, ok := bySummary[text]
		if !ok {
			v = e.Embed(text)
			if len(v) != e.Dim() {
				// Embedder fault mid-sync (server died, transient error). Flush
				// what we have so the next sync resumes here, then surface it.
				if ferr := store.upsert(batch, nil); ferr != nil {
					return added, len(drop), len(want), ferr
				}
				return added + len(batch), len(drop), len(want), fmt.Errorf("embedder returned no vector for record %s (embed server down mid-sync?); progress saved, re-run refresh to resume", r.ID)
			}
			bySummary[text] = v
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
