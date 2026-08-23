package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCandidateShadowV2PacksCachesAndLeavesActiveBrainUntouched(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	transcript := strings.Join([]string{
		`{"type":"event_msg","payload":{"type":"user_message","message":"Always keep migrations reversible."}}`,
		`{"type":"event_msg","payload":{"type":"agent_message","message":"Understood."}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"I prefer explicit transactions over implicit ones."}}`,
		`{"type":"event_msg","payload":{"type":"agent_message","message":"Understood."}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"Never skip race tests."}}`,
	}, "\n")
	brainDir := writeSingleSessionFixture(t, now, transcript)
	if err := writeFactTaxonomy(brainDir, defaultFactTaxonomy(now)); err != nil {
		t.Fatal(err)
	}
	if err := writeFacts(brainDir, "main", []factRecord{{
		ID: factRecordID("An authored fact.", []string{"project.tooling.stack"}), Text: "An authored fact.",
		Paths: []string{"project.tooling.stack"}, Branch: "main", Origin: factOriginAuthored,
		Status: factStatusActive, CreatedAt: now, UpdatedAt: now,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := saveDistillCache(brainDir, distillCache{Version: distillCacheVersion, Sessions: map[string]string{"main/legacy": "sha256:legacy"}}); err != nil {
		t.Fatal(err)
	}

	activePaths := []string{
		exportManifestFileName,
		exportReadmeFileName,
		factsTaxonomyPath,
		factsFileRelPath("main"),
		distillCachePath,
	}
	before := readDistillShadowFilesV2(t, brainDir, activePaths)
	var calls atomic.Int64
	run := func(ctx context.Context, _ string, _ []string, input []byte, _ time.Duration) (string, error) {
		calls.Add(1)
		ids := candidateIDsFromPackedInputV2(t, input)
		var lines []string
		for index, id := range ids {
			if index == len(ids)-1 {
				lines = append(lines, id+"\tNO_FACTS")
				continue
			}
			lines = append(lines, id+"\tconvention\tpreferences.coding.style\tKeep this durable rule.")
		}
		recordDistillProviderUsage(ctx, distillProviderUsage{Source: "test", Reported: true, InputReported: true, OutputReported: true, InputTokens: 100, OutputTokens: 10})
		return strings.Join(lines, "\n"), nil
	}
	opts := distillCommandOptions{
		agent: "command", agentCommand: []string{"fake"}, run: run,
		pipeline: distillPipelineCandidates, shadow: true, model: "fixed-model", effort: "low",
		maxChunkBytes: defaultDistillChunkSize, timeout: time.Minute, concurrency: 2,
	}
	plan, err := buildDistillDryRunReportContext(context.Background(), brainDir, opts, now)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Shadow || plan.CandidatePacks != 1 || plan.CandidatePacksIfUncached != 1 || plan.ExtractionAgentCalls != 1 || plan.CandidateCacheHits != 0 || plan.CandidateCacheMisses != 3 {
		t.Fatalf("first shadow plan did not predict packed work: %+v", plan)
	}
	first, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now)
	if err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("first shadow provider calls = %d, want one packed call", got)
	}
	if !first.Shadow || first.CandidateCards != 3 || first.CandidateMembers != 3 || first.CandidatePacks != 1 || first.ShadowFacts != 2 || first.CandidateEmptyResults != 1 {
		t.Fatalf("unexpected first shadow summary: %+v", first)
	}
	if first.TokenUsage == nil || !first.TokenUsage.Complete || first.TokenUsage.Calls != 1 {
		t.Fatalf("shadow usage accounting = %+v", first.TokenUsage)
	}
	if after := readDistillShadowFilesV2(t, brainDir, activePaths); !reflect.DeepEqual(after, before) {
		t.Fatalf("shadow changed active Brain state\nbefore=%q\nafter=%q", before, after)
	}
	if _, err := os.Stat(filepath.Join(brainDir, filepath.FromSlash(distillCandidateResultCacheV2Path))); err != nil {
		t.Fatalf("v2 member cache was not persisted: %v", err)
	}
	plan, err = buildDistillDryRunReportContext(context.Background(), brainDir, opts, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if plan.CandidateCards != 3 || plan.CandidatePacks != 0 || plan.ExtractionAgentCalls != 0 || plan.CandidateCacheHits != 3 || plan.CandidateCacheMisses != 0 {
		t.Fatalf("cached shadow plan did not predict zero calls: %+v", plan)
	}

	second, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("no-op shadow rerun made %d total calls, want one", got)
	}
	if second.ExtractionCalls != 0 || second.CandidateCacheHits != 3 || second.CandidateCacheMisses != 0 || second.ShadowFacts != 2 {
		t.Fatalf("member cache did not make rerun zero-call: %+v", second)
	}
	if after := readDistillShadowFilesV2(t, brainDir, activePaths); !reflect.DeepEqual(after, before) {
		t.Fatal("cached shadow rerun changed active Brain state")
	}
}

func TestSortDistillSessionsDeterministicallyBreaksTimestampTies(t *testing.T) {
	created := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	sessions := []exportSession{
		{SessionID: "s2", Branch: "main", TranscriptPath: "z", CreatedAt: created},
		{SessionID: "s1", Branch: "feature", TranscriptPath: "b", CreatedAt: created},
		{SessionID: "s1", Branch: "feature", TranscriptPath: "a", CreatedAt: created},
		{SessionID: "later", Branch: "main", TranscriptPath: "c", CreatedAt: created.Add(time.Second)},
		{SessionID: "earlier", Branch: "main", TranscriptPath: "d", CreatedAt: created.Add(-time.Second)},
	}
	sortDistillSessionsDeterministically(sessions, func(session exportSession) string { return session.Branch })
	want := []string{"earlier/d", "s1/a", "s1/b", "s2/z", "later/c"}
	got := make([]string, len(sessions))
	for index, session := range sessions {
		got[index] = session.SessionID + "/" + session.TranscriptPath
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("deterministic session order = %v, want %v", got, want)
	}
}

func TestCandidateShadowV2BisectsFailedPackExactlyOnce(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	brainDir := writeSingleSessionFixture(t, now, strings.Join([]string{
		`{"type":"event_msg","payload":{"type":"user_message","message":"Always keep migrations reversible."}}`,
		`{"type":"event_msg","payload":{"type":"agent_message","message":"Understood."}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"Never skip race tests."}}`,
	}, "\n"))
	var calls atomic.Int64
	opts := distillCommandOptions{
		agent: "command", agentCommand: []string{"fake"}, pipeline: distillPipelineCandidates, shadow: true,
		model: "fixed-model", effort: "low", maxChunkBytes: defaultDistillChunkSize, timeout: time.Minute, concurrency: 1,
		run: func(_ context.Context, _ string, _ []string, input []byte, _ time.Duration) (string, error) {
			calls.Add(1)
			ids := candidateIDsFromPackedInputV2(t, input)
			if len(ids) > 1 {
				return "malformed provider prose", nil
			}
			return ids[0] + "\tNO_FACTS", nil
		},
	}
	source, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now)
	if err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("provider calls = %d, want one parent plus two split calls", got)
	}
	if source.CandidatePacks != 1 || source.CandidatePacksDone != 1 || source.CandidateSplitCalls != 2 || source.FailedChunks != 1 || source.CandidateEmptyResults != 2 {
		t.Fatalf("split accounting = %+v", source)
	}
}

func TestCandidatePackAttemptV2UsesProtocolOutputBudget(t *testing.T) {
	member := distillCandidatePackMemberV2{
		CandidateID:  "candidate-v1:" + strings.Repeat("a", 64),
		RenderedCard: "card\n",
		Anchor:       distillCandidateAnchorV1{CandidateID: "candidate-v1:" + strings.Repeat("a", 64)},
	}
	pack := distillCandidatePackFromMembersV2([]distillCandidatePackMemberV2{member})
	opts := distillCommandOptions{
		timeout: time.Minute,
		run: func(ctx context.Context, _ string, _ []string, _ []byte, _ time.Duration) (string, error) {
			if got := distillDecodedOutputLimit(ctx); got != distillCandidateMaxOutputBytesV2 {
				t.Fatalf("candidate decoded-output limit = %d, want %d", got, distillCandidateMaxOutputBytesV2)
			}
			return member.CandidateID + "\tNO_FACTS", nil
		},
	}
	attempt := runDistillCandidatePackAttemptV2(context.Background(), t.TempDir(), []string{"fake"}, pack, defaultFactTaxonomy(time.Now()), opts)
	if attempt.Err != nil || len(attempt.Results) != 1 || !attempt.Results[0].NoFacts {
		t.Fatalf("candidate output-budget attempt = %+v", attempt)
	}
}

func TestCandidateShadowV2ConfidenceDoesNotInvalidateMemberCache(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	brainDir := writeSingleSessionFixture(t, now, `{"type":"event_msg","payload":{"type":"user_message","message":"Always keep migrations reversible."}}`)
	calls := 0
	opts := distillCommandOptions{
		agent: "command", agentCommand: []string{"fake"}, pipeline: distillPipelineCandidates, shadow: true,
		model: "fixed-model", effort: "low", maxChunkBytes: defaultDistillChunkSize, timeout: time.Minute,
		run: func(_ context.Context, _ string, _ []string, input []byte, _ time.Duration) (string, error) {
			calls++
			return candidateIDsFromPackedInputV2(t, input)[0] + "\tNO_FACTS", nil
		},
	}
	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now); err != nil {
		t.Fatal(err)
	}
	opts.confidenceThreshold = 0.99
	if source, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	} else if source.ExtractionCalls != 0 || source.CandidateCacheHits != 1 {
		t.Fatalf("confidence-only change invalidated extraction: %+v", source)
	}
	if calls != 1 {
		t.Fatalf("confidence-only change made %d provider calls, want one total", calls)
	}
}

func TestCandidateShadowV2CachesSuccessfulSplitNeighborOnly(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	brainDir := writeSingleSessionFixture(t, now, strings.Join([]string{
		`{"type":"event_msg","payload":{"type":"user_message","message":"Always keep migrations reversible."}}`,
		`{"type":"event_msg","payload":{"type":"agent_message","message":"Understood."}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"Never skip race tests."}}`,
	}, "\n"))
	var calls atomic.Int64
	failID := ""
	opts := distillCommandOptions{
		agent: "command", agentCommand: []string{"fake"}, pipeline: distillPipelineCandidates, shadow: true,
		model: "fixed-model", effort: "low", maxChunkBytes: defaultDistillChunkSize, timeout: time.Minute,
		run: func(_ context.Context, _ string, _ []string, input []byte, _ time.Duration) (string, error) {
			calls.Add(1)
			ids := candidateIDsFromPackedInputV2(t, input)
			if len(ids) > 1 {
				failID = ids[0]
				return "malformed parent", nil
			}
			if ids[0] == failID {
				return "malformed isolated member", nil
			}
			return ids[0] + "\tNO_FACTS", nil
		},
	}
	first, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now)
	if err != nil {
		t.Fatal(err)
	}
	if first.CandidatePacksDone != 0 || first.CandidateCacheHits != 0 || first.CandidateCacheMisses != 2 || first.CandidateEmptyResults != 1 || calls.Load() != 3 {
		t.Fatalf("partial split did not isolate cacheable neighbor: calls=%d source=%+v", calls.Load(), first)
	}

	opts.run = func(_ context.Context, _ string, _ []string, input []byte, _ time.Duration) (string, error) {
		calls.Add(1)
		ids := candidateIDsFromPackedInputV2(t, input)
		if len(ids) != 1 || ids[0] != failID {
			t.Fatalf("retry input IDs = %v, want only failed member %q", ids, failID)
		}
		return ids[0] + "\tNO_FACTS", nil
	}
	second, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if second.CandidateCacheHits != 1 || second.CandidateCacheMisses != 1 || second.ExtractionCalls != 1 || calls.Load() != 4 {
		t.Fatalf("retry did not reuse successful split neighbor: calls=%d source=%+v", calls.Load(), second)
	}
}

func TestCandidateShadowV2ReusesRelocatedAndAppendedCards(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	base := `{"type":"event_msg","payload":{"type":"user_message","message":"Always keep migrations reversible."}}`
	brainDir := writeSingleSessionFixture(t, now, base)
	calls := 0
	opts := distillCommandOptions{
		agent: "command", agentCommand: []string{"fake"}, pipeline: distillPipelineCandidates, shadow: true,
		model: "fixed-model", effort: "low", maxChunkBytes: defaultDistillChunkSize, timeout: time.Minute,
		run: func(_ context.Context, _ string, _ []string, input []byte, _ time.Duration) (string, error) {
			calls++
			ids := candidateIDsFromPackedInputV2(t, input)
			lines := make([]string, len(ids))
			for i, id := range ids {
				lines[i] = id + "\tNO_FACTS"
			}
			return strings.Join(lines, "\n"), nil
		},
	}
	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now); err != nil {
		t.Fatal(err)
	}
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	rel := manifest.Sources.Sessions.Sessions[0].TranscriptPath
	relocated := `{"type":"session_meta","payload":{"cwd":"/tmp"}}` + "\n" + base
	if err := os.WriteFile(filepath.Join(brainDir, filepath.FromSlash(rel)), []byte(relocated), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if second.ExtractionCalls != 0 || second.CandidateCacheHits != 1 || calls != 1 {
		t.Fatalf("mechanics relocation invalidated extraction: calls=%d source=%+v", calls, second)
	}
	appended := relocated + "\n" + `{"type":"event_msg","payload":{"type":"user_message","message":"Never skip race tests."}}`
	if err := os.WriteFile(filepath.Join(brainDir, filepath.FromSlash(rel)), []byte(appended), 0o600); err != nil {
		t.Fatal(err)
	}
	third, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if third.ExtractionCalls != 1 || third.CandidateCacheHits != 1 || third.CandidateCacheMisses != 1 || calls != 2 {
		t.Fatalf("one appended candidate did not cost exactly one pack call: calls=%d source=%+v", calls, third)
	}
}

func candidateIDsFromPackedInputV2(t *testing.T, input []byte) []string {
	t.Helper()
	var ids []string
	seen := map[string]bool{}
	for _, line := range bytes.Split(input, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var header struct {
			Type        string `json:"type"`
			CandidateID string `json:"candidate_id"`
		}
		if err := json.Unmarshal(line, &header); err != nil {
			t.Fatalf("packed candidate input is not JSONL: %v", err)
		}
		if header.Type == "distill_candidate_v1" && !seen[header.CandidateID] {
			seen[header.CandidateID] = true
			ids = append(ids, header.CandidateID)
		}
	}
	if len(ids) == 0 {
		t.Fatal("packed candidate input contains no candidate headers")
	}
	return ids
}

func readDistillShadowFilesV2(t *testing.T, brainDir string, rels []string) map[string][]byte {
	t.Helper()
	out := make(map[string][]byte, len(rels))
	for _, rel := range rels {
		data, err := os.ReadFile(filepath.Join(brainDir, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		out[rel] = data
	}
	return out
}
