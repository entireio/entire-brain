package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDistillDiscoveryCacheExactNoopReconstructsWithoutPlaintext(t *testing.T) {
	session := candidateTestSession()
	content := strings.Join([]string{
		`{"type":"event_msg","payload":{"type":"user_message","message":"Always preserve the guarded writer."}}`,
		`{"type":"event_msg","payload":{"type":"agent_message","message":"Understood."}}`,
	}, "\n")
	normalized := normalizeDistillTranscriptV1(session, "main", content)
	cards := selectDistillCandidateCardsV1(normalized)
	entry := buildDistillDiscoveryEntryV1(session, content, normalized, cards)
	restored, ok := restoreDistillDiscoveryV1(session, "feature", content, entry)
	if !ok || len(restored) != 1 {
		t.Fatalf("exact no-op cache miss: ok=%v cards=%+v", ok, restored)
	}
	if restored[0].Source.Branch != "feature" || restored[0].Source.Transcript != session.TranscriptPath || restored[0].ID != cards[0].ID {
		t.Fatalf("current provenance or stable identity lost: %+v", restored[0])
	}
	root := t.TempDir()
	cache := newDistillDiscoveryCacheV1()
	cache.Entries[session.SessionID] = entry
	if err := saveDistillDiscoveryCacheV1(root, cache); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(distillDiscoveryCacheRelV1)))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "guarded writer") || strings.Contains(string(data), "Understood") {
		t.Fatal("discovery cache persisted transcript text")
	}
	info, err := os.Stat(filepath.Join(root, filepath.FromSlash(distillDiscoveryCacheRelV1)))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("cache mode = %v, err=%v", info.Mode().Perm(), err)
	}
}

func TestDistillDiscoveryCacheEvidenceAndMutationInvalidate(t *testing.T) {
	session := candidateTestSession()
	content := `{"type":"event_msg","payload":{"type":"user_message","message":"Always preserve exact anchors."}}`
	normalized := normalizeDistillTranscriptV1(session, "main", content)
	entry := buildDistillDiscoveryEntryV1(session, content, normalized, selectDistillCandidateCardsV1(normalized))
	if _, ok := restoreDistillDiscoveryV1(session, "main", strings.Replace(content, "anchors", "loci", 1), entry); ok {
		t.Fatal("mid-file mutation reused discovery")
	}
	review := session
	review.Kind = "agent_review"
	if _, ok := restoreDistillDiscoveryV1(review, "main", content, entry); ok {
		t.Fatal("agent-review evidence change reused discovery")
	}
	checkpoint := session
	checkpoint.LatestCheckpoint = "different"
	checkpoint.Summary = &checkpointSummary{Outcome: "success"}
	checkpoint.FilesTouched = []string{"internal/cli/new.go"}
	if _, ok := restoreDistillDiscoveryV1(checkpoint, "main", content, entry); ok {
		t.Fatal("checkpoint corroboration change reused discovery")
	}
}

func TestPrepareDistillSessionInputUsesAndStagesDiscoveryCache(t *testing.T) {
	session := candidateTestSession()
	content := `{"type":"event_msg","payload":{"type":"user_message","message":"Never discard the transaction qualifier."}}`
	cache := newDistillDiscoveryCacheV1()
	dirty := false
	opts := distillCommandOptions{pipeline: distillPipelineCandidates, discoveryCache: &cache, discoveryDirty: &dirty}
	first := prepareDistillSessionInput(session, "main", content, opts)
	if first.Err != nil || !dirty || len(cache.Entries) != 1 {
		t.Fatalf("discovery was not staged: err=%v dirty=%v entries=%d", first.Err, dirty, len(cache.Entries))
	}
	dirty = false
	second := prepareDistillSessionInput(session, "feature", content, opts)
	if second.Err != nil || dirty || len(second.CandidateBatches) != 1 {
		t.Fatalf("exact cache was not reused: err=%v dirty=%v batches=%d", second.Err, dirty, len(second.CandidateBatches))
	}
	if first.CandidateBatches[0].CandidateIDs[0] != second.CandidateBatches[0].CandidateIDs[0] || second.CandidateBatches[0].Anchors[0].Branch != "feature" {
		t.Fatal("cache reuse changed identity or retained stale provenance")
	}
}

func TestPrepareDistillSessionInputExactNoopLeavesPersistedStateByteStable(t *testing.T) {
	root := t.TempDir()
	session := candidateTestSession()
	content := `{"type":"event_msg","payload":{"type":"user_message","message":"Always retain the stable cache bytes."}}`
	cache := newDistillDiscoveryCacheV1()
	dirty := false
	opts := distillCommandOptions{pipeline: distillPipelineCandidates, discoveryCache: &cache, discoveryDirty: &dirty}
	if got := prepareDistillSessionInput(session, "main", content, opts); got.Err != nil {
		t.Fatal(got.Err)
	}
	if err := saveDistillDiscoveryCacheV1(root, cache); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, filepath.FromSlash(distillDiscoveryCacheRelV1))
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	dirty = false
	loaded := loadDistillDiscoveryCacheV1(root)
	opts.discoveryCache = &loaded
	if got := prepareDistillSessionInput(session, "feature", content, opts); got.Err != nil {
		t.Fatal(got.Err)
	}
	if dirty {
		t.Fatal("exact no-op marked discovery state dirty")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("exact no-op changed persisted discovery bytes")
	}
}

func TestPrepareDistillSessionInputAppendReuse(t *testing.T) {
	tests := []struct {
		name, prefix, suffix string
		wantCards            int
	}{
		{"no candidate", `{"type":"event_msg","payload":{"type":"user_message","message":"Status update only."}}` + "\n", `{"type":"event_msg","payload":{"type":"agent_message","message":"Acknowledged."}}` + "\n", 0},
		{"isolated candidate", `{"type":"event_msg","payload":{"type":"user_message","message":"Status update only."}}` + "\n", `{"type":"event_msg","payload":{"type":"user_message","message":"Always preserve append identity."}}` + "\n", 1},
		{"boundary acceptance", `{"type":"event_msg","payload":{"type":"agent_message","message":"We should use PostgreSQL instead of SQLite."}}` + "\n", `{"type":"event_msg","payload":{"type":"user_message","message":"Yes, use that decision."}}` + "\n", 1},
		{"old user gains assistant context", `{"type":"event_msg","payload":{"type":"user_message","message":"Always preserve the selected transaction."}}` + "\n", `{"type":"event_msg","payload":{"type":"agent_message","message":"The transaction includes the rollback qualifier."}}` + "\n", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cache := newDistillDiscoveryCacheV1()
			dirty := false
			stats := distillDiscoveryReuseStatsV1{}
			opts := distillCommandOptions{pipeline: distillPipelineCandidates, discoveryCache: &cache, discoveryDirty: &dirty, discoveryStats: &stats}
			if got := prepareDistillSessionInput(candidateTestSession(), "main", tt.prefix, opts); got.Err != nil {
				t.Fatal(got.Err)
			}
			dirty = false
			stats = distillDiscoveryReuseStatsV1{}
			got := prepareDistillSessionInput(candidateTestSession(), "main", tt.prefix+tt.suffix, opts)
			if got.Err != nil {
				t.Fatal(got.Err)
			}
			if stats.AppendHits != 1 || stats.FullFallbacks != 0 || stats.ParsedRecords != 1 || got.CandidateCards != tt.wantCards {
				t.Fatalf("append stats/input = %+v cards=%d", stats, got.CandidateCards)
			}
			fresh := prepareDistillSessionInput(candidateTestSession(), "main", tt.prefix+tt.suffix, distillCommandOptions{pipeline: distillPipelineCandidates})
			if fresh.Err != nil || len(fresh.CandidateBatches) != len(got.CandidateBatches) {
				t.Fatalf("fresh parity setup failed: %v", fresh.Err)
			}
			for index := range fresh.CandidateBatches {
				if fresh.CandidateBatches[index].Chunk.Text != got.CandidateBatches[index].Chunk.Text || fresh.CandidateBatches[index].CandidateIDs[0] != got.CandidateBatches[index].CandidateIDs[0] {
					t.Fatalf("append differs from fresh discovery at batch %d", index)
				}
			}
		})
	}
}

func TestPrepareDistillSessionInputAppendFallsBackSafely(t *testing.T) {
	prefix := `{"type":"event_msg","payload":{"type":"user_message","message":"Status update only."}}` + "\n"
	cache := newDistillDiscoveryCacheV1()
	dirty := false
	stats := distillDiscoveryReuseStatsV1{}
	opts := distillCommandOptions{pipeline: distillPipelineCandidates, discoveryCache: &cache, discoveryDirty: &dirty, discoveryStats: &stats}
	if got := prepareDistillSessionInput(candidateTestSession(), "main", prefix, opts); got.Err != nil {
		t.Fatal(got.Err)
	}
	stats = distillDiscoveryReuseStatsV1{}
	mutated := strings.Replace(prefix, "Status", "State", 1) + `{"type":"event_msg","payload":{"type":"user_message","message":"Never lose mutation safety."}}` + "\n"
	if got := prepareDistillSessionInput(candidateTestSession(), "main", mutated, opts); got.Err != nil {
		t.Fatal(got.Err)
	}
	if stats.AppendHits != 0 || stats.FullFallbacks != 1 {
		t.Fatalf("prefix mutation did not fall back: %+v", stats)
	}
	stats = distillDiscoveryReuseStatsV1{}
	partial := mutated + `{"type":"event_msg"`
	if got := prepareDistillSessionInput(candidateTestSession(), "main", partial, opts); got.Err == nil {
		t.Fatal("partial JSONL suffix was accepted")
	}
	if stats.AppendHits != 0 || stats.FullFallbacks != 1 {
		t.Fatalf("partial suffix did not fall back: %+v", stats)
	}
	stats = distillDiscoveryReuseStatsV1{}
	unsupported := mutated + `{"future_narrative":{"role":"user","text":"Always hidden."}}` + "\n"
	if got := prepareDistillSessionInput(candidateTestSession(), "main", unsupported, opts); got.Err == nil {
		t.Fatal("unsupported suffix dialect was accepted")
	}
	if stats.AppendHits != 0 || stats.FullFallbacks != 1 {
		t.Fatalf("unsupported suffix did not fall back: %+v", stats)
	}
}

func TestMarshalDistillDiscoveryCacheBoundsPersistedIndentedBytes(t *testing.T) {
	entry := distillDiscoveryCacheEntryV1{SourceSHA256: strings.Repeat("a", 64), EvidenceSHA256: strings.Repeat("b", 64)}
	one := newDistillDiscoveryCacheV1()
	one.Entries["z-session"] = entry
	oneBytes, err := marshalDistillDiscoveryCacheForWriteV1(one, distillDiscoveryCacheMaxBytesV1)
	if err != nil {
		t.Fatal(err)
	}
	two := newDistillDiscoveryCacheV1()
	two.Entries["a-session"] = entry
	two.Entries["z-session"] = entry
	bounded, err := marshalDistillDiscoveryCacheForWriteV1(two, len(oneBytes))
	if err != nil {
		t.Fatal(err)
	}
	if len(bounded) > len(oneBytes) || bounded[len(bounded)-1] != '\n' {
		t.Fatalf("persisted bytes exceed injected bound: %d > %d", len(bounded), len(oneBytes))
	}
	parsed, err := parseDistillDiscoveryCacheV1(bounded)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Entries) != 1 {
		t.Fatalf("bounded entries = %d, want 1", len(parsed.Entries))
	}
	if _, ok := parsed.Entries["z-session"]; !ok {
		t.Fatalf("deterministic eviction kept wrong entry: %+v", parsed.Entries)
	}
}

func TestDistillDiscoveryTelemetryAndCorruptRebuildStatus(t *testing.T) {
	stats := distillDiscoveryReuseStatsV1{ExactHits: 1, AppendHits: 2, FullFallbacks: 3, ParsedRecords: 4, CorruptRebuilds: 5}
	source := &factSourceManifest{}
	applyDistillDiscoveryStatsV1(source, stats)
	if source.DiscoveryExactHits != 1 || source.DiscoveryAppendResumed != 2 || source.DiscoveryFullRescans != 3 || source.DiscoveryNormalizedRecords != 4 || source.DiscoveryCorruptRebuilds != 5 {
		t.Fatalf("manifest telemetry = %+v", source)
	}
	root := t.TempDir()
	path := filepath.Join(root, filepath.FromSlash(distillDiscoveryCacheRelV1))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	cache, corrupt := loadDistillDiscoveryCacheV1WithStatus(root)
	if !corrupt || cache.CandidateSchema != distillCandidateSchemaVersion || len(cache.Entries) != 0 {
		t.Fatalf("corrupt rebuild status/cache = %v %+v", corrupt, cache)
	}
}

func TestDistillDiscoveryTelemetryCommandJSONAndManifestScrub(t *testing.T) {
	fields := []string{"discovery_exact_hits", "discovery_append_resumed", "discovery_full_rescans", "discovery_normalized_records", "discovery_corrupt_rebuilds"}
	source := factSourceManifest{Pipeline: distillPipelineCandidates, DiscoveryExactHits: 1, DiscoveryAppendResumed: 2, DiscoveryFullRescans: 3, DiscoveryNormalizedRecords: 4, DiscoveryCorruptRebuilds: 5}
	data, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range fields {
		if !strings.Contains(string(data), `"`+field+`":`) {
			t.Fatalf("candidate command JSON omitted %s: %s", field, data)
		}
	}
	for _, pipeline := range []string{distillPipelineCandidates, distillPipelineLegacy, ""} {
		source.Pipeline = pipeline
		persisted := factSourceForManifestV3(&source)
		data, err = json.Marshal(persisted)
		if err != nil {
			t.Fatal(err)
		}
		for _, field := range fields {
			if strings.Contains(string(data), `"`+field+`"`) {
				t.Fatalf("persisted v3 manifest for pipeline %q contains %s: %s", pipeline, field, data)
			}
		}
	}
	dry, err := json.Marshal(distillDryRunReport{Pipeline: distillPipelineCandidates})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range fields {
		if !strings.Contains(string(dry), `"`+field+`":0`) {
			t.Fatalf("dry-run JSON omitted zero %s: %s", field, dry)
		}
	}
}
