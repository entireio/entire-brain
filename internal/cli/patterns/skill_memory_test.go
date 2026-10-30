package patterns

import (
	"os"
	"path/filepath"
	"testing"
)

func procView(id, label string, support, success, corrected int) PatternView {
	return PatternView{
		ID: id, Type: "procedure", Scope: "repo",
		Title: label, StrengthLabel: "high", Support: support,
		Reinforcement: &ReinforcementCounts{Success: success, Corrected: corrected},
	}
}

func TestEvaluateSkillMemoryEvidenceStates(t *testing.T) {
	cur := procView("P1", "git commit → git push", 14, 14, 0)
	fp := PatternEvidenceFingerprint(cur)

	// active + matching fingerprint + no installs -> current.
	if e := EvaluateSkillMemory(SkillMemoryRecord{PatternID: "P1", Status: SkillStatusActive, Fingerprint: fp}, &cur); e.Sub != SkillSubCurrent {
		t.Errorf("active matching = %q, want current", e.Sub)
	}
	// active + drifted fingerprint -> update.
	if e := EvaluateSkillMemory(SkillMemoryRecord{PatternID: "P1", Status: SkillStatusActive, Fingerprint: "sha256:old"}, &cur); e.Sub != SkillSubUpdate {
		t.Errorf("active drifted = %q, want update", e.Sub)
	}
	// declined + matching -> current (suppress); drifted -> reconsider.
	if e := EvaluateSkillMemory(SkillMemoryRecord{PatternID: "P1", Status: SkillStatusDeclined, Fingerprint: fp}, &cur); e.Sub != SkillSubCurrent {
		t.Errorf("declined matching = %q, want current", e.Sub)
	}
	if e := EvaluateSkillMemory(SkillMemoryRecord{PatternID: "P1", Status: SkillStatusDeclined, Fingerprint: "sha256:old"}, &cur); e.Sub != SkillSubReconsider {
		t.Errorf("declined drifted = %q, want reconsider", e.Sub)
	}
}

func TestEvaluateSkillMemoryFileStates(t *testing.T) {
	cur := procView("P1", "x", 5, 5, 0)
	fp := PatternEvidenceFingerprint(cur)
	dir := t.TempDir()
	skillPath := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(skillPath, []byte("# skill v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sha, _ := fileContentSHA(skillPath)
	rec := SkillMemoryRecord{PatternID: "P1", Status: SkillStatusActive, Fingerprint: fp,
		Installs: []SkillInstall{{Path: skillPath, ContentSHA: sha}}}

	// File present and unchanged -> current.
	if e := EvaluateSkillMemory(rec, &cur); e.Sub != SkillSubCurrent {
		t.Errorf("unchanged file = %q, want current", e.Sub)
	}
	// File edited -> edited (file integrity beats evidence drift).
	if err := os.WriteFile(skillPath, []byte("# skill v2 (hand-edited)\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if e := EvaluateSkillMemory(rec, &cur); e.Sub != SkillSubEdited {
		t.Errorf("edited file = %q, want edited", e.Sub)
	}
	// File gone -> missing.
	os.Remove(skillPath)
	if e := EvaluateSkillMemory(rec, &cur); e.Sub != SkillSubMissing {
		t.Errorf("missing file = %q, want missing", e.Sub)
	}
}

func TestPatternEvidenceFingerprintStability(t *testing.T) {
	base := procView("P1", "x", 10, 10, 0)
	// One more supporting episode within the same magnitude bucket -> same fp.
	near := procView("P1", "x", 11, 11, 0)
	if PatternEvidenceFingerprint(base) != PatternEvidenceFingerprint(near) {
		t.Error("minor support drift should not change the fingerprint")
	}
	// A strength re-tier -> different fp.
	retier := base
	retier.StrengthLabel = "medium"
	if PatternEvidenceFingerprint(base) == PatternEvidenceFingerprint(retier) {
		t.Error("strength re-tier should change the fingerprint")
	}
}

func TestSkillMemoryRoundTrip(t *testing.T) {
	brainDir := t.TempDir()
	in := []SkillMemoryRecord{{PatternID: "P1", Status: SkillStatusActive, SkillName: "verify-release",
		Installs:    []SkillInstall{{Agents: []string{"claude-code"}, Path: "~/.claude/skills/verify-release/SKILL.md", ContentSHA: "sha256:abc"}},
		Fingerprint: "sha256:fp"}}
	if err := WriteBrainSkillMemory(brainDir, in); err != nil {
		t.Fatal(err)
	}
	out, err := LoadBrainSkillMemory(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].PatternID != "P1" || len(out[0].Installs) != 1 || out[0].Installs[0].ContentSHA != "sha256:abc" {
		t.Errorf("round-trip mismatch: %+v", out)
	}
}
