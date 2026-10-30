package cli

import (
	"github.com/entireio/entire-brain/internal/cli/command/patternscmd"
	"testing"
	"time"

	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
	patterncore "github.com/entireio/entire-brain/internal/cli/patterns"
)

func TestSkillMemoryStatusCounts(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	brainDir := t.TempDir()

	p1 := procedureRecord{ID: "P1", Type: "procedure", Scope: "repo", Commands: []string{"git commit", "git push"},
		Support: 14, StrengthLabel: "high", Reinforcement: reinforcementCounts{Success: 14}}
	p2 := procedureRecord{ID: "P2", Type: "procedure", Scope: "repo", Commands: []string{"go test", "git commit"},
		Support: 8, StrengthLabel: "high", Reinforcement: reinforcementCounts{Success: 8}}
	if err := writeBrainProceduresFile(brainDir, []procedureRecord{p1, p2}); err != nil {
		t.Fatal(err)
	}

	mem := []skillMemoryRecord{
		{PatternID: "P1", Status: patterncore.SkillStatusActive, Fingerprint: patterncore.PatternEvidenceFingerprint(patterncore.ProcedureView(p1))}, // current
		{PatternID: "P2", Status: patterncore.SkillStatusActive, Fingerprint: "sha256:stale"},                                                        // update
		{PatternID: "gone", Status: patterncore.SkillStatusDeclined, Fingerprint: "sha256:whatever"},                                                 // declined/current
	}
	if err := patterncore.WriteBrainSkillMemory(brainDir, mem); err != nil {
		t.Fatal(err)
	}

	manifest := exportManifest{
		SchemaVersion: bmanifest.SchemaVersion, GeneratedAt: now,
		Sources: &brainSources{
			Sessions: &sessionSourceManifest{Sessions: []exportSession{{SessionID: "s1"}}},
			Patterns: &patternSourceManifest{GeneratedAt: now, Episodes: 1, Procedures: 2},
		},
	}
	if err := writeBrainManifestAndReadme(brainDir, manifest); err != nil {
		t.Fatal(err)
	}

	report := patternscmd.BuildStatusReport(brainDir, patternsCmdDeps())
	if !report.Present {
		t.Fatal("status not present")
	}
	if report.AcceptedSkills != 2 {
		t.Errorf("accepted = %d, want 2", report.AcceptedSkills)
	}
	if report.DeclinedPatterns != 1 {
		t.Errorf("declined = %d, want 1", report.DeclinedPatterns)
	}
	if report.UpdatesAvailable != 1 {
		t.Errorf("updates = %d, want 1 (P2 drifted)", report.UpdatesAvailable)
	}
}
