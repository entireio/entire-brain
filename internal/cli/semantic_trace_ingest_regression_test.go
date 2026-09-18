package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestSemanticRuntimeTraceFactsPreserveMatchEvidenceAndRanking(t *testing.T) {
	brainDir, _, opts := indexFixtureBrain(t, semanticBoundaryFixtureSnapshot())
	tracePath := filepath.Join(t.TempDir(), "runtime-traces.ndjson")
	data := []byte(`{"from":"ValidateToken","to":"GET /tokens/{id}","type":"HANDLES_ROUTE"}
{"from":"ValidateToken","to":"missing target","type":"OBSERVED_CALL"}
`)
	if err := os.WriteFile(tracePath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	cmd := &cobra.Command{Use: "ingest-traces"}
	cmd.SetOut(&out)
	if err := runSemanticIngestTraces(cmd, opts, semanticTraceIngestOptions{json: true}, tracePath); err != nil {
		t.Fatalf("ingest traces: %v", err)
	}
	var report semanticTraceIngestReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("decode ingest report: %v\n%s", err, out.String())
	}
	if report.Total != 2 || report.Matched != 1 || report.Unmatched != 1 || report.Path == "" {
		t.Fatalf("ingest report = %+v", report)
	}

	manifest, err := loadBrainManifest(brainDir)
	if err != nil || manifest.Sources == nil || manifest.Sources.Semantic == nil {
		t.Fatalf("semantic manifest: %+v err=%v", manifest.Sources, err)
	}
	records, err := semanticRuntimeTraceFacts(brainDir, manifest.Sources.Semantic, "ValidateToken", 10)
	if err != nil {
		t.Fatalf("runtime trace facts: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("runtime trace records = %d, want 2: %+v", len(records), records)
	}
	if records[0].FromID != "ValidateToken" || !strings.HasSuffix(records[0].ToID, ":route:GET /tokens/{id}") || records[0].Confidence != 1 || records[0].Evidence[0].FilePath != report.Path {
		t.Fatalf("matched trace ordering/identity = %+v, report=%+v", records[0], report)
	}
	if records[1].FromID != "ValidateToken" || records[1].ToID != "missing target" || records[1].Confidence != 0.5 || records[1].Evidence[0].FilePath != report.Path {
		t.Fatalf("unmatched trace ordering/identity = %+v, report=%+v", records[1], report)
	}
	seenMatched, seenUnmatched := false, false
	for _, record := range records {
		if record.RecordType != "runtime_trace" || record.Type != "RUNTIME_TRACE" || len(record.Evidence) != 1 || record.Evidence[0].Kind != "runtime_trace_import" {
			t.Fatalf("runtime trace identity/evidence = %+v", record)
		}
		if record.Confidence == 1 {
			seenMatched = true
			if len(record.WarningCodes) != 0 || record.Reason != "runtime trace observed HANDLES_ROUTE" {
				t.Fatalf("matched trace metadata = %+v", record)
			}
		} else if record.Confidence == 0.5 {
			seenUnmatched = true
			if len(record.WarningCodes) != 1 || record.WarningCodes[0] != "UNMATCHED_STATIC_EDGE" {
				t.Fatalf("unmatched trace metadata = %+v", record)
			}
		} else {
			t.Fatalf("unexpected runtime trace confidence = %+v", record)
		}
	}
	if !seenMatched || !seenUnmatched {
		t.Fatalf("did not retain matched and unmatched trace evidence: %+v", records)
	}
	if limited, err := semanticRuntimeTraceFacts(brainDir, manifest.Sources.Semantic, "ValidateToken", 1); err != nil || len(limited) != 1 || limited[0].ToID != records[0].ToID {
		t.Fatalf("limited runtime trace query = %+v err=%v", limited, err)
	}
}
