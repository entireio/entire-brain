package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func procView(id, label string, support, success, corrected int) patternView {
	return patternView{
		ID: id, Type: "procedure", Scope: "repo",
		Title: label, StrengthLabel: "high", Support: support,
		Reinforcement: &reinforcementCounts{Success: success, Corrected: corrected},
	}
}

func TestEvaluateSkillMemoryEvidenceStates(t *testing.T) {
	cur := procView("P1", "git commit → git push", 14, 14, 0)
	fp := patternEvidenceFingerprint(cur)

	// active + matching fingerprint + no installs -> current.
	if e := evaluateSkillMemory(skillMemoryRecord{PatternID: "P1", Status: skillStatusActive, Fingerprint: fp}, &cur); e.Sub != skillSubCurrent {
		t.Errorf("active matching = %q, want current", e.Sub)
	}
	// active + drifted fingerprint -> update.
	if e := evaluateSkillMemory(skillMemoryRecord{PatternID: "P1", Status: skillStatusActive, Fingerprint: "sha256:old"}, &cur); e.Sub != skillSubUpdate {
		t.Errorf("active drifted = %q, want update", e.Sub)
	}
	// declined + matching -> current (suppress); drifted -> reconsider.
	if e := evaluateSkillMemory(skillMemoryRecord{PatternID: "P1", Status: skillStatusDeclined, Fingerprint: fp}, &cur); e.Sub != skillSubCurrent {
		t.Errorf("declined matching = %q, want current", e.Sub)
	}
	if e := evaluateSkillMemory(skillMemoryRecord{PatternID: "P1", Status: skillStatusDeclined, Fingerprint: "sha256:old"}, &cur); e.Sub != skillSubReconsider {
		t.Errorf("declined drifted = %q, want reconsider", e.Sub)
	}
}

func TestEvaluateSkillMemoryFileStates(t *testing.T) {
	cur := procView("P1", "x", 5, 5, 0)
	fp := patternEvidenceFingerprint(cur)
	dir := t.TempDir()
	skillPath := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(skillPath, []byte("# skill v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sha, _ := fileContentSHA(skillPath)
	rec := skillMemoryRecord{PatternID: "P1", Status: skillStatusActive, Fingerprint: fp,
		Installs: []skillInstall{{Path: skillPath, ContentSHA: sha}}}

	// File present and unchanged -> current.
	if e := evaluateSkillMemory(rec, &cur); e.Sub != skillSubCurrent {
		t.Errorf("unchanged file = %q, want current", e.Sub)
	}
	// File edited -> edited (file integrity beats evidence drift).
	if err := os.WriteFile(skillPath, []byte("# skill v2 (hand-edited)\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if e := evaluateSkillMemory(rec, &cur); e.Sub != skillSubEdited {
		t.Errorf("edited file = %q, want edited", e.Sub)
	}
	// File gone -> missing.
	os.Remove(skillPath)
	if e := evaluateSkillMemory(rec, &cur); e.Sub != skillSubMissing {
		t.Errorf("missing file = %q, want missing", e.Sub)
	}
}

func TestPatternEvidenceFingerprintStability(t *testing.T) {
	base := procView("P1", "x", 10, 10, 0)
	// One more supporting episode within the same magnitude bucket -> same fp.
	near := procView("P1", "x", 11, 11, 0)
	if patternEvidenceFingerprint(base) != patternEvidenceFingerprint(near) {
		t.Error("minor support drift should not change the fingerprint")
	}
	// A strength re-tier -> different fp.
	retier := base
	retier.StrengthLabel = "medium"
	if patternEvidenceFingerprint(base) == patternEvidenceFingerprint(retier) {
		t.Error("strength re-tier should change the fingerprint")
	}
}

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
		{PatternID: "P1", Status: skillStatusActive, Fingerprint: patternEvidenceFingerprint(procedureView(p1))}, // current
		{PatternID: "P2", Status: skillStatusActive, Fingerprint: "sha256:stale"},                                // update
		{PatternID: "gone", Status: skillStatusDeclined, Fingerprint: "sha256:whatever"},                         // declined/current
	}
	if err := writeBrainSkillMemory(brainDir, mem); err != nil {
		t.Fatal(err)
	}

	manifest := exportManifest{
		SchemaVersion: brainManifestSchemaVersion, GeneratedAt: now,
		Sources: &brainSources{
			Sessions: &sessionSourceManifest{Sessions: []exportSession{{SessionID: "s1"}}},
			Patterns: &patternSourceManifest{GeneratedAt: now, Episodes: 1, Procedures: 2},
		},
	}
	if err := writeBrainManifestAndReadme(brainDir, manifest); err != nil {
		t.Fatal(err)
	}

	report := buildPatternsStatusReport(brainDir)
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

func TestSkillMemoryRoundTrip(t *testing.T) {
	brainDir := t.TempDir()
	in := []skillMemoryRecord{{PatternID: "P1", Status: skillStatusActive, SkillName: "verify-release",
		Installs:    []skillInstall{{Agents: []string{"claude-code"}, Path: "~/.claude/skills/verify-release/SKILL.md", ContentSHA: "sha256:abc"}},
		Fingerprint: "sha256:fp"}}
	if err := writeBrainSkillMemory(brainDir, in); err != nil {
		t.Fatal(err)
	}
	out, err := loadBrainSkillMemory(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].PatternID != "P1" || len(out[0].Installs) != 1 || out[0].Installs[0].ContentSHA != "sha256:abc" {
		t.Errorf("round-trip mismatch: %+v", out)
	}
}
