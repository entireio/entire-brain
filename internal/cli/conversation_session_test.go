package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeSessionNavigationFixture builds a brain with one five-exchange session
// through the real index pipeline, so ordinals, ranges, and digests are the
// production ones.
func writeSessionNavigationFixture(t *testing.T) (brainDir string, records []historyRecord) {
	t.Helper()
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	brainDir = t.TempDir()
	rel := "sessions/main/20260808T090000Z_nav.jsonl"
	var b strings.Builder
	for i := 1; i <= 5; i++ {
		b.WriteString(fmt.Sprintf(`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"step %d: what broke in stage %d"}]}}`, i, i) + "\n")
		b.WriteString(fmt.Sprintf(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Decision %d: stage %d failed on the flag parser."}]}}`, i, i) + "\n")
	}
	full := filepath.Join(brainDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := exportManifest{
		SchemaVersion: brainManifestSchemaVersion, GeneratedAt: now, RepoKey: "test/nav", DefaultBranch: "main",
		Sources: &brainSources{Sessions: &sessionSourceManifest{GeneratedAt: now, DefaultBranch: "main", Sessions: []exportSession{
			{SessionID: "nav-sess", Branch: "main", Agent: "claude", LatestCheckpoint: "cp", TranscriptPath: rel, CreatedAt: now.Add(-time.Hour)},
		}}},
	}
	if err := writeBrainManifestAndReadme(brainDir, manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := writeBrainHistoryIndexAndSource(brainDir, now, nil); err != nil {
		t.Fatal(err)
	}
	onDisk, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	index, err := loadBrainHistoryIndex(brainDir, onDisk.Sources.History)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range index.Records {
		if record.Kind == conversationKind {
			records = append(records, record)
		}
	}
	if len(records) != 5 {
		t.Fatalf("fixture exchanges = %d, want 5", len(records))
	}
	return brainDir, records
}

func TestConversationSessionRefIdentity(t *testing.T) {
	ref1, degraded1 := conversationSessionRef("repo", "main", "sess-a", "sha256:x")
	ref2, _ := conversationSessionRef("repo", "main", "sess-a", "sha256:DIFFERENT")
	if ref1 != ref2 || degraded1 {
		t.Fatalf("session id identity must ignore the digest: %s vs %s", ref1, ref2)
	}
	refBranch, _ := conversationSessionRef("repo", "release", "sess-a", "")
	if refBranch == ref1 {
		t.Fatal("the same raw session id on two branches must be two session views")
	}
	refDegraded, degraded := conversationSessionRef("repo", "main", "", "sha256:x")
	if !degraded || refDegraded == ref1 {
		t.Fatalf("missing session id must degrade to the digest identity: %s", refDegraded)
	}
	if !strings.HasPrefix(ref1, conversationSessionIDPrefix) {
		t.Fatalf("ref format: %s", ref1)
	}
}

func TestConversationSearchCarriesSessionRef(t *testing.T) {
	brainDir, _ := writeSessionNavigationFixture(t)
	results, err := retrieveConversation(brainDir, "flag parser stage", 5, modeLexical, retrievalOptions{})
	if err != nil || len(results) == 0 {
		t.Fatalf("search: %v (%d)", err, len(results))
	}
	for _, result := range results {
		if !strings.HasPrefix(result.SessionRef, conversationSessionIDPrefix) {
			t.Fatalf("search hit missing session_ref: %+v", result)
		}
	}
}

func TestConversationContextPacketExpansion(t *testing.T) {
	brainDir, records := writeSessionNavigationFixture(t)
	target := records[2] // ordinal 3 of 5

	found, missing, err := getUnifiedBatchOptions("", brainDir, "main", []string{target.ID}, getOptions{ContextBefore: 1, ContextAfter: 1, ContextSet: true})
	if err != nil || len(missing) != 0 || len(found) != 1 {
		t.Fatalf("get: err=%v missing=%v found=%d", err, missing, len(found))
	}
	packet := found[0]
	if packet.TargetID != target.ID || packet.SessionRef == "" {
		t.Fatalf("packet identity: %+v", packet)
	}
	if packet.ContextBefore != 1 || packet.ContextAfter != 1 || len(packet.Turns) != 2 {
		t.Fatalf("context counts: before=%d after=%d turns=%d", packet.ContextBefore, packet.ContextAfter, len(packet.Turns))
	}
	if packet.Turns[0].TurnOrdinal != 2 || packet.Turns[1].TurnOrdinal != 4 {
		t.Fatalf("context ordinals: %d, %d", packet.Turns[0].TurnOrdinal, packet.Turns[1].TurnOrdinal)
	}
	if !strings.Contains(packet.Turns[0].Text, "Decision 2") || !strings.Contains(packet.Turns[1].Text, "Decision 4") {
		t.Fatalf("context text: %q / %q", packet.Turns[0].Text, packet.Turns[1].Text)
	}
	if !strings.Contains(packet.Text, "Decision 3") {
		t.Fatalf("target text: %q", packet.Text)
	}

	// First turn: nothing before; the missing neighbors are reported, never
	// invented.
	first, _, err := getUnifiedBatchOptions("", brainDir, "main", []string{records[0].ID}, getOptions{ContextBefore: 3, ContextAfter: 1, ContextSet: true})
	if err != nil || len(first) != 1 {
		t.Fatalf("first-turn get: %v", err)
	}
	if first[0].ContextBefore != 0 || first[0].OmittedBefore != 3 || first[0].ContextAfter != 1 {
		t.Fatalf("first-turn window: %+v", first[0])
	}
}

func TestConversationSessionOutlinePagination(t *testing.T) {
	brainDir, records := writeSessionNavigationFixture(t)
	ref := records[0]
	found, _, err := getUnifiedBatchOptions("", brainDir, "main", []string{ref.ID}, getOptions{})
	if err != nil || len(found) != 1 {
		t.Fatalf("seed get: %v", err)
	}
	sessionID := found[0].SessionRef

	page1, _, err := getUnifiedBatchOptions("", brainDir, "main", []string{sessionID}, getOptions{OutlineLimit: 2, OutlineSet: true})
	if err != nil || len(page1) != 1 {
		t.Fatalf("outline page 1: %v", err)
	}
	outline := page1[0]
	if outline.Heading != "session_outline" || len(outline.Turns) != 2 || outline.NextTurn != 2 {
		t.Fatalf("page 1: heading=%s turns=%d next=%d", outline.Heading, len(outline.Turns), outline.NextTurn)
	}
	if outline.Turns[0].Request == "" || outline.Turns[0].Text != "" {
		t.Fatalf("outline entries carry request excerpts, not bodies: %+v", outline.Turns[0])
	}
	if !outline.VerificationRequired {
		t.Fatal("outline must carry the historical-evidence contract")
	}

	// Page 2 via the cursor; appending to the session must not shift it.
	appendTurn := func() {
		full := filepath.Join(brainDir, filepath.FromSlash(records[0].Path))
		extra := `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"step 6: appended"}]}}` + "\n" +
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Decision 6: appended."}]}}` + "\n"
		data, err := os.ReadFile(full)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, append(data, []byte(extra)...), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := writeBrainHistoryIndexAndSource(brainDir, time.Date(2026, 8, 8, 13, 0, 0, 0, time.UTC), nil); err != nil {
			t.Fatal(err)
		}
	}
	appendTurn()
	page2, _, err := getUnifiedBatchOptions("", brainDir, "main", []string{sessionID}, getOptions{AfterTurn: outline.NextTurn, OutlineLimit: 2, OutlineSet: true})
	if err != nil || len(page2) != 1 {
		t.Fatalf("outline page 2: %v", err)
	}
	if len(page2[0].Turns) != 2 || page2[0].Turns[0].TurnOrdinal != 3 || page2[0].Turns[1].TurnOrdinal != 4 {
		t.Fatalf("page 2 after append: %+v", page2[0].Turns)
	}
	if page2[0].NextTurn != 4 {
		t.Fatalf("page 2 cursor: %d", page2[0].NextTurn)
	}
}

func TestConversationNavigationArgumentContract(t *testing.T) {
	brainDir, records := writeSessionNavigationFixture(t)
	convID := records[0].ID
	found, _, err := getUnifiedBatchOptions("", brainDir, "main", []string{convID}, getOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sessionID := found[0].SessionRef

	cases := []struct {
		name string
		ids  []string
		opts getOptions
	}{
		{"context on a session target", []string{sessionID}, getOptions{ContextBefore: 1, ContextSet: true}},
		{"outline cursor on an exchange target", []string{convID}, getOptions{AfterTurn: 1, OutlineSet: true}},
		{"navigation with two ids", []string{convID, convID}, getOptions{ContextBefore: 1, ContextSet: true}},
		{"session id in a multi-id request", []string{sessionID, convID}, getOptions{}},
		{"context out of range", []string{convID}, getOptions{ContextBefore: conversationContextMax + 1, ContextSet: true}},
	}
	for _, tc := range cases {
		if _, _, err := getUnifiedBatchOptions("", brainDir, "main", tc.ids, tc.opts); err == nil {
			t.Fatalf("%s must be a structured error", tc.name)
		}
	}

	// A tombstoned session is unreadable through navigation immediately.
	stones := loadSessionTombstones(brainDir)
	stones.Excluded["nav-sess"] = sessionTombstone{At: time.Now().UTC()}
	if err := saveSessionTombstones(brainDir, stones); err != nil {
		t.Fatal(err)
	}
	_, missing, err := getUnifiedBatchOptions("", brainDir, "main", []string{sessionID}, getOptions{})
	if err != nil || len(missing) != 1 {
		t.Fatalf("tombstoned session outline must be not-found: err=%v missing=%v", err, missing)
	}
}

func TestConversationLegacyIDCollisionIsAmbiguous(t *testing.T) {
	brainDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(brainDir, historyDirName), 0o700); err != nil {
		t.Fatal(err)
	}
	generated := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	sharedID := conversationIDPrefix + "collide"
	mk := func(branch, path string) historyRecord {
		return historyRecord{
			ID: sharedID, Kind: conversationKind, Path: path, Line: 1, EndLine: 2, TurnOrdinal: 1,
			SessionID: "sess-shared", Branch: branch, Summary: "shared request on " + branch,
			ContentRole: conversationContentRole, SourceDigest: "sha256:" + branch,
		}
	}
	index := historyIndex{GeneratedAt: generated, Records: []historyRecord{
		mk("main", "sessions/main/20260801T000000Z_s.jsonl"),
		mk("release", "sessions/release/20260802T000000Z_s.jsonl"),
	}}
	data, err := json.MarshalIndent(index, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(brainDir, filepath.FromSlash(historyIndexPath)), data, 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := exportManifest{
		SchemaVersion: brainManifestSchemaVersion, GeneratedAt: generated, RepoKey: "test/collide",
		Sources: &brainSources{History: &historySourceManifest{GeneratedAt: generated, IndexPath: historyIndexPath, Records: 2}},
	}
	if err := writeBrainManifestAndReadme(brainDir, manifest); err != nil {
		t.Fatal(err)
	}

	// No selector: structured ambiguity, never a silent winner.
	if _, _, err := getUnifiedBatchOptions("", brainDir, "other-branch", []string{sharedID}, getOptions{}); err == nil || !strings.Contains(err.Error(), "memory_identity_ambiguous") {
		t.Fatalf("colliding id must be ambiguous: %v", err)
	}
	// The branch selector disambiguates.
	found, missing, err := getUnifiedBatchOptions("", brainDir, "release", []string{sharedID}, getOptions{})
	if err != nil || len(missing) != 0 || len(found) != 1 {
		t.Fatalf("branch-selected get: err=%v missing=%v", err, missing)
	}
	if found[0].Branch != "release" {
		t.Fatalf("branch selector picked the wrong scope: %+v", found[0])
	}

	// Search and multi-concept retrieval must preserve both logical session
	// scopes before and after an unrelated short-term overlay activates the
	// cross-tier reconciliation path. A legacy ID collision is ambiguity, not
	// permission to choose a global winner.
	assertBothScopesVisible := func(stage string) {
		t.Helper()
		results, err := retrieveConversation(brainDir, "shared request", 10, modeLexical, retrievalOptions{})
		if err != nil {
			t.Fatalf("%s search: %v", stage, err)
		}
		branches := map[string]bool{}
		for _, result := range results {
			if result.ID == sharedID {
				branches[result.Branch] = true
			}
		}
		if !branches["main"] || !branches["release"] || len(branches) != 2 {
			t.Fatalf("%s search hid a colliding session scope: results=%+v", stage, results)
		}

		coverage, err := retrieveConversationMultiConcept(brainDir, "shared", 10, modeLexical, retrievalOptions{
			Concepts: []string{"request"},
		})
		if err != nil {
			t.Fatalf("%s multi-concept: %v", stage, err)
		}
		refs := map[string]bool{}
		for _, result := range coverage {
			refs[result.SessionRef] = true
		}
		if len(refs) != 2 {
			t.Fatalf("%s multi-concept hid a colliding session scope: results=%+v", stage, coverage)
		}
	}
	assertBothScopesVisible("without overlay")

	unrelatedPath := "sessions/main/20260803T000000Z_unrelated.jsonl"
	overlay := shortTermIndex{
		Version:           historyShortTermVersion,
		ReconcilerVersion: historyShortTermReconcilerVersion,
		BaseGeneratedAt:   generated,
		GeneratedAt:       generated.Add(time.Hour),
		Files: map[string]shortTermFile{
			unrelatedPath: {Records: []historyRecord{{
				ID: conversationIDPrefix + "unrelated", Kind: conversationKind,
				Path: unrelatedPath, Line: 1, EndLine: 2, TurnOrdinal: 1,
				SessionID: "unrelated-session", Branch: "main", Summary: "unrelated overlay exchange",
				ContentRole: conversationContentRole, SourceDigest: "sha256:unrelated",
			}}},
		},
	}
	overlayData, err := json.MarshalIndent(overlay, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(brainDir, filepath.FromSlash(historyShortTermPath)), overlayData, 0o600); err != nil {
		t.Fatal(err)
	}
	assertBothScopesVisible("with unrelated overlay")
}
