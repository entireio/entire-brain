package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestBriefOverlayPreservesPayloadWithCorruptJSONIndex(t *testing.T) {
	fixture := newBrainBriefProfileFixture(t)
	manifest, err := loadBrainManifest(fixture.brainDir)
	if err != nil {
		t.Fatal(err)
	}
	const query = "OVERLAY_PAYLOAD_EVIDENCE"
	index := historyIndex{GeneratedAt: manifest.Sources.History.GeneratedAt, Records: []historyRecord{
		{ID: "obsolete", Kind: "decision", Branch: "main", Path: "sessions/main/changed.jsonl", Line: 1, Summary: query + " obsolete decision"},
		{ID: "retained", Kind: "decision", Branch: "main", Path: "sessions/main/indexed-only.jsonl", Line: 17, Summary: query + " retained decision"},
	}}
	source := *manifest.Sources.History
	source.Records = len(index.Records)
	source.IndexPath = historyIndexPath
	source.IndexDigest = ""
	if err := publishHistoryComparisonIndex(fixture.brainDir, index, &source); err != nil {
		t.Fatal(err)
	}
	manifest, err = loadBrainManifest(fixture.brainDir)
	if err != nil {
		t.Fatal(err)
	}
	source = *manifest.Sources.History
	if _, _, _, err := rankHistoryLexicalFromSource(fixture.brainDir, &source, "history", query, 3); err != nil {
		t.Fatal(err)
	}
	if err := saveHistoryShortTerm(fixture.brainDir, shortTermIndex{Version: historyShortTermVersion, ReconcilerVersion: historyShortTermReconcilerVersion, BaseGeneratedAt: source.GeneratedAt, Files: map[string]shortTermFile{"sessions/main/changed.jsonl": {}}}); err != nil {
		t.Fatal(err)
	}
	// Neither the indexed-only retained row nor the empty replacement exists in
	// the raw transcript fixture. Only the validated FTS payload can return it.
	indexPath := filepath.Join(fixture.brainDir, filepath.FromSlash(source.IndexPath))
	data, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	for i := range data {
		data[i] = '!'
	}
	if err := os.WriteFile(indexPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, used, err := rankHistoryViaFreshFTSCutoffDetailed(fixture.brainDir, &source, "history", query, 3, historyFTSRelevanceCutoff); err != nil || !used {
		t.Fatalf("payload fixture unavailable: used=%v err=%v", used, err)
	}
	out, err := execute(t, NewRootCommand(fixture.opts), "brief", query, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var report brainBriefReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, hit := range report.History.Matches {
		if hit.Path == "sessions/main/changed.jsonl" {
			t.Fatalf("obsolete replacement survived: %+v", hit)
		}
		if hit.Path == "sessions/main/indexed-only.jsonl" && hit.Line == 17 {
			found = true
		}
	}
	if !found {
		t.Fatalf("validated indexed history discarded: %s", out)
	}
}

func TestFreshHistoryLexicalPredicatePrecedesLimit(t *testing.T) {
	for _, backend := range []string{"payload", "json"} {
		t.Run(backend, func(t *testing.T) {
			index := historyIndex{Records: []historyRecord{
				{ID: "blocked", Kind: "decision", Path: "blocked", Summary: "guard predicate evidence"},
				{ID: "kept", Kind: "decision", Path: "kept", Summary: "guard predicate evidence"},
			}}
			dir, source := writeDirectHistoryFTSFixture(t, index)
			if backend == "json" {
				if err := os.Remove(historyFTSDBPath(dir)); err != nil {
					t.Fatal(err)
				}
			}
			got, _, _, err := rankFreshHistoryLexicalFromSource(dir, source, "history", "guard predicate evidence", 1, func(r historyRecord) bool { return r.ID != "blocked" })
			if err != nil || len(got) != 1 || got[0].Record.ID != "kept" {
				t.Fatalf("guarded result=%+v err=%v", got, err)
			}
		})
	}
}
