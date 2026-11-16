package cli

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
)

const (
	brainBriefProfileSchemaVersion = 1
	brainBriefProfileDurationUnit  = "nanoseconds"
)

// brainBriefProfile is an observation-only, privacy-safe sidecar for the
// `brain brief` hot path. It deliberately contains only durations, counts, and
// fixed enum-like strings. Task/query text, result identifiers, paths, source
// text, environment values, and wall-clock timestamps do not belong here.
//
// Every field is emitted, including zero-valued stages, so benchmark tooling can
// distinguish a stable schema from an accidentally partial profile.
type brainBriefProfile struct {
	SchemaVersion    int                        `json:"schema_version"`
	DurationUnit     string                     `json:"duration_unit"`
	TotalBrief       brainBriefProfileDuration  `json:"total_brief"`
	StatusBuildState brainBriefProfileStage     `json:"status_build_state"`
	Semantic         brainBriefProfileSemantic  `json:"semantic"`
	Facts            brainBriefProfileFacts     `json:"facts"`
	Synthesis        brainBriefProfileSynthesis `json:"synthesis"`
	Knowledge        brainBriefProfileKnowledge `json:"knowledge"`
	Packet           brainBriefProfilePacket    `json:"packet"`

	started time.Time
}

type brainBriefProfileDuration struct {
	DurationNS int64 `json:"duration_ns"`
}

type brainBriefProfileStage struct {
	Invoked     bool  `json:"invoked"`
	DurationNS  int64 `json:"duration_ns"`
	InputCount  int   `json:"input_count"`
	OutputCount int   `json:"output_count"`
	ErrorCount  int   `json:"error_count"`
}

type brainBriefProfileSemantic struct {
	Context       brainBriefProfileStage `json:"context"`
	RuntimeTraces brainBriefProfileStage `json:"runtime_traces"`
	Tests         brainBriefProfileStage `json:"tests"`
}

type brainBriefProfileFacts struct {
	Load       brainBriefProfileStage `json:"load"`
	Rank       brainBriefProfileStage `json:"rank"`
	CacheFlush brainBriefProfileStage `json:"cache_flush"`
}

type brainBriefProfileSynthesis struct {
	LikelyFiles     brainBriefProfileStage `json:"likely_files"`
	ActionChecklist brainBriefProfileStage `json:"action_checklist"`
}

type brainBriefProfileKnowledge struct {
	Patterns       brainBriefProfileStage `json:"patterns"`
	Consolidations brainBriefProfileStage `json:"consolidations"`
	Themes         brainBriefProfileStage `json:"themes"`
}

type brainBriefProfilePacket struct {
	Format        string                  `json:"format"`
	Serialization brainBriefProfileStage  `json:"serialization"`
	ByteCount     int                     `json:"byte_count"`
	Counts        brainBriefProfileCounts `json:"counts"`
}

type brainBriefProfileCounts struct {
	SemanticSymbols     int `json:"semantic_symbols"`
	SemanticRelations   int `json:"semantic_relations"`
	SemanticNeighbors   int `json:"semantic_neighbors"`
	RuntimeTraces       int `json:"runtime_traces"`
	TestRoots           int `json:"test_roots"`
	TestSuggestions     int `json:"test_suggestions"`
	Facts               int `json:"facts"`
	FactsWithLocusDrift int `json:"facts_with_locus_drift"`
	Actions             int `json:"actions"`
	LikelyEditFiles     int `json:"likely_edit_files"`
	LikelyTestFiles     int `json:"likely_test_files"`
	LikelyFiles         int `json:"likely_files"`
	Patterns            int `json:"patterns"`
	Consolidations      int `json:"consolidations"`
	Themes              int `json:"themes"`
	GuidanceItems       int `json:"guidance_items"`
	Warnings            int `json:"warnings"`
}

func newBrainBriefProfiler() *brainBriefProfile {
	now := time.Now()
	return &brainBriefProfile{
		SchemaVersion: brainBriefProfileSchemaVersion,
		DurationUnit:  brainBriefProfileDurationUnit,
		started:       now,
	}
}

// start returns a monotonic-bearing time only when profiling is enabled. The
// nil receiver makes stage instrumentation cheap and side-effect-free on the
// default path.
func (p *brainBriefProfile) start() time.Time {
	if p == nil {
		return time.Time{}
	}
	return time.Now()
}

func (p *brainBriefProfile) elapsed(start time.Time) int64 {
	if p == nil || start.IsZero() {
		return 0
	}
	d := time.Since(start)
	if d < 0 {
		return 0
	}
	return d.Nanoseconds()
}

func (p *brainBriefProfile) finishStage(stage *brainBriefProfileStage, start time.Time, inputs, outputs, errors int) {
	if p == nil {
		return
	}
	stage.Invoked = true
	stage.DurationNS = p.elapsed(start)
	stage.InputCount = inputs
	stage.OutputCount = outputs
	stage.ErrorCount = errors
}

func (p *brainBriefProfile) finishTotal() {
	if p == nil {
		return
	}
	p.TotalBrief.DurationNS = p.elapsed(p.started)
}

func brainBriefProfilePacketCounts(report brainBriefReport, packetFormat brainBriefPacketFormat) brainBriefProfileCounts {
	counts := brainBriefProfileCounts{
		SemanticSymbols:     len(report.Semantic.Context.Symbols),
		SemanticRelations:   len(report.Semantic.Context.Relations),
		SemanticNeighbors:   len(report.Semantic.Context.Neighbors),
		RuntimeTraces:       len(report.Semantic.RuntimeTraces),
		TestRoots:           len(report.Semantic.Tests.Roots),
		TestSuggestions:     len(report.Semantic.Tests.Suggestions),
		Facts:               len(report.Facts),
		FactsWithLocusDrift: len(report.FactsLocusDrift),
		Actions:             len(report.ActionChecklist),
		LikelyEditFiles:     len(report.LikelyEditFiles),
		LikelyTestFiles:     len(report.LikelyTestFiles),
		LikelyFiles:         len(report.LikelyFiles),
		Patterns:            len(report.Patterns),
		Consolidations:      len(report.Consolidations),
		Themes:              len(report.Themes),
		GuidanceItems:       len(report.Guidance),
		Warnings:            len(report.Status.Warnings) + len(report.Status.Live.Warnings) + len(report.Warnings),
	}
	if packetFormat == brainBriefPacketText {
		// The text renderer is intentionally narrower than the JSON packet. Keep
		// counts aligned with records actually serialized in that format.
		counts.SemanticRelations = 0
		counts.SemanticNeighbors = 0
		counts.TestRoots = 0
		counts.Patterns = 0
		counts.GuidanceItems = 0
		counts.Warnings = len(report.Status.Warnings) + len(report.Warnings)
	}
	return counts
}

func writeBrainBriefProfile(path string, profile brainBriefProfile) error {
	data, err := json.MarshalIndent(profile, "", "  ")
	if err != nil {
		return fmt.Errorf("encode brief profile: %w", err)
	}
	data = append(data, '\n')
	if err := brainstore.WriteFileAtomic(path, data, 0o600); err != nil {
		return fmt.Errorf("write brief profile: %w", err)
	}
	return nil
}
