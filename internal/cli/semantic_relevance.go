package cli

import (
	"math"
	"sort"
	"strings"
)

const (
	semanticTailMADMultiplier       = 2.0
	semanticMarginMADMultiplier     = 1.0
	semanticClusterMADMultiplier    = 0.5
	semanticMinimumTailSeparation   = 0.02
	semanticMinimumClusterWidth     = 0.02
	semanticMinimumWinnerSeparation = 0.02
	semanticMinimumBackgroundLead   = 0.025
	minimumCalibrationCorpus        = 3
	historySemanticCalibrationK     = 4096
	factEmbeddingDocumentVersion    = "fact-context-v2"
)

func factEmbeddingModelID(modelID string) string {
	return modelID + ":" + factEmbeddingDocumentVersion
}

func factEmbeddingText(f factRecord) string {
	if len(f.Paths) == 0 {
		return f.Text
	}
	return f.Text + "\nTopic: " + strings.Join(f.Paths, " ")
}

// semanticResultMask structurally keeps explicit vsearch as nearest-neighbor
// retrieval while requiring corpus-relative evidence for semantic-only
// candidates injected by automatic hybrid query, recall, and brief.
func semanticResultMask(
	rawCosines []float64, requireRelevance bool, corpusBackgrounds []float64,
) []bool {
	if requireRelevance {
		return calibratedSemanticMask(rawCosines, corpusBackgrounds)
	}
	keep := make([]bool, len(rawCosines))
	for index, cosine := range rawCosines {
		keep[index] = isFinite(cosine)
	}
	return keep
}

// calibratedSemanticMask admits semantic-only candidates when they clear three
// independent checks: a robust upper-tail threshold, a leave-one-out corpus
// background, and separation between the admitted upper cluster and the
// remaining corpus. Explicit vsearch remains available when a corpus is too
// small to calibrate.
// Lexical hits are handled independently and never depend on this mask.
func calibratedSemanticMask(rawCosines, corpusBackgrounds []float64) []bool {
	keep := make([]bool, len(rawCosines))
	if len(corpusBackgrounds) != len(rawCosines) {
		return keep
	}
	valid := make([]int, 0, len(rawCosines))
	distribution := make([]float64, 0, len(rawCosines))
	for index, cosine := range rawCosines {
		if !isFinite(cosine) || !isFinite(corpusBackgrounds[index]) {
			continue
		}
		valid = append(valid, index)
		distribution = append(distribution, cosine)
	}
	if len(valid) < minimumCalibrationCorpus {
		return keep
	}
	// The median is intentionally the background model. If most of the corpus is
	// semantically close, automatic semantic-only injection fails closed instead
	// of treating a broad topic as a selective retrieval signal; explicit vector
	// search remains available for that use case.
	median, fullMAD := medianAbsoluteDeviation(distribution)
	sortedDistribution := append([]float64(nil), distribution...)
	sort.Float64s(sortedDistribution)
	lowerEnd := (len(sortedDistribution) + 1) / 2
	_, lowerMAD := medianAbsoluteDeviation(sortedDistribution[:lowerEnd])
	tailSeparation := math.Max(
		semanticTailMADMultiplier*lowerMAD,
		semanticMinimumTailSeparation,
	)
	threshold := median + tailSeparation
	sort.Slice(valid, func(left, right int) bool {
		if rawCosines[valid[left]] != rawCosines[valid[right]] {
			return rawCosines[valid[left]] > rawCosines[valid[right]]
		}
		return valid[left] < valid[right]
	})
	top := rawCosines[valid[0]]
	// The top candidate anchors the admitted cluster. If it cannot clear the
	// corpus direction and robust tail, no lower-scoring candidate can establish
	// a stronger retrieval signal.
	if top-corpusBackgrounds[valid[0]] <= semanticMinimumBackgroundLead || top <= threshold {
		return keep
	}
	clusterWidth := math.Max(
		semanticClusterMADMultiplier*fullMAD,
		semanticMinimumClusterWidth,
	)
	clusterEnd := 1
	for clusterEnd < len(valid) && top-rawCosines[valid[clusterEnd]] <= clusterWidth {
		clusterEnd++
	}
	if clusterEnd < len(valid) {
		winnerSeparation := math.Max(
			semanticMarginMADMultiplier*fullMAD,
			semanticMinimumWinnerSeparation,
		)
		clusterFloor := rawCosines[valid[clusterEnd-1]]
		bestBackground := rawCosines[valid[clusterEnd]]
		if clusterFloor-bestBackground <= winnerSeparation {
			return keep
		}
	}
	for _, index := range valid[:clusterEnd] {
		if rawCosines[index]-corpusBackgrounds[index] > semanticMinimumBackgroundLead && rawCosines[index] > threshold {
			keep[index] = true
		}
	}
	return keep
}

// semanticMedianBackgrounds supplies a score-only background for stores that
// expose nearest-neighbor cosines but not vectors (history vec0). It preserves
// the same tail, cluster, and minimum-lead gates while using the finite candidate
// median as the anisotropy baseline.
func semanticMedianBackgrounds(rawCosines []float64) []float64 {
	finite := make([]float64, 0, len(rawCosines))
	for _, cosine := range rawCosines {
		if isFinite(cosine) {
			finite = append(finite, cosine)
		}
	}
	backgrounds := make([]float64, len(rawCosines))
	if len(finite) == 0 {
		for index := range backgrounds {
			backgrounds[index] = math.NaN()
		}
		return backgrounds
	}
	median, _ := medianAbsoluteDeviation(finite)
	for index, cosine := range rawCosines {
		if isFinite(cosine) {
			backgrounds[index] = median
		} else {
			backgrounds[index] = math.NaN()
		}
	}
	return backgrounds
}

// semanticLeaveOneOutBackgrounds compares the query with the normalized mean
// direction of every other valid corpus vector. Removing the candidate avoids
// letting a relevant answer inflate its own rejection floor. Float64
// accumulation keeps large or cancelling corpora stable.
func semanticLeaveOneOutBackgrounds(qvec []float32, vectors [][]float32) []float64 {
	backgrounds := make([]float64, len(vectors))
	for index := range backgrounds {
		backgrounds[index] = math.NaN()
	}
	if !vectorHasMagnitude(qvec) {
		return backgrounds
	}
	dim := len(qvec)
	qnormSquared := 0.0
	for _, value := range qvec {
		qnormSquared += float64(value) * float64(value)
	}
	if qnormSquared == 0 || !isFinite(qnormSquared) {
		return backgrounds
	}
	inverseNorms := make([]float64, len(vectors))
	accumulator := make([]float64, dim)
	valid := 0
	for index, vector := range vectors {
		if len(vector) != dim || !vectorHasMagnitude(vector) {
			continue
		}
		normSquared := 0.0
		for _, value := range vector {
			normSquared += float64(value) * float64(value)
		}
		if normSquared == 0 || !isFinite(normSquared) {
			continue
		}
		inverseNorms[index] = 1 / math.Sqrt(normSquared)
		for dimension, value := range vector {
			accumulator[dimension] += float64(value) * inverseNorms[index]
		}
		valid++
	}
	if valid < minimumCalibrationCorpus {
		return backgrounds
	}
	qnorm := math.Sqrt(qnormSquared)
	for index, vector := range vectors {
		if inverseNorms[index] == 0 {
			continue
		}
		dot := 0.0
		backgroundNormSquared := 0.0
		for dimension, value := range vector {
			component := accumulator[dimension] - float64(value)*inverseNorms[index]
			dot += float64(qvec[dimension]) * component
			backgroundNormSquared += component * component
		}
		if backgroundNormSquared > 0 && isFinite(backgroundNormSquared) {
			backgrounds[index] = dot / (qnorm * math.Sqrt(backgroundNormSquared))
		}
	}
	return backgrounds
}

func isFinite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func vectorHasMagnitude(vector []float32) bool {
	hasMagnitude := false
	for _, value := range vector {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return false
		}
		if value != 0 {
			hasMagnitude = true
		}
	}
	return hasMagnitude
}

func medianAbsoluteDeviation(values []float64) (float64, float64) {
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	median := medianSorted(sorted)
	deviations := make([]float64, len(sorted))
	for index, value := range sorted {
		deviations[index] = math.Abs(value - median)
	}
	sort.Float64s(deviations)
	return median, medianSorted(deviations)
}

func medianSorted(sorted []float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	middle := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[middle]
	}
	return (sorted[middle-1] + sorted[middle]) / 2
}
