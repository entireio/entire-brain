package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Forming must route the three verifier-backed skill sources through their
// accepted records when invoked from the root command. Keep this at the command
// boundary: direct loader tests cannot catch a Cobra routing regression.
func TestRootCommandPatternsSkillsFormRoutesAcceptedKnowledgeSources(t *testing.T) {
	opts, repoDir, brainDir := commandRegressionFixture(t)
	newCorpusAtDir(t, brainDir)

	db, err := openPatternCorpusMutableDB(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO themes (id, scope, title, description, shape, member_keys, support, fingerprint, strength, status, verdict, created_at, updated_at)
		VALUES ('theme:review', 'repo', 'Review branch changes', 'Audit branch changes for regressions', 'conversation', '[]', 3, 'sha256:theme', 0.8, 'active', 'accepted', 't', 't')`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	seedAcceptedDeepDossier(t, db, "lesson:release-evidence", deployDeepDossier(), nil)
	seedAcceptedDeepDossier(t, db, "convention:release-evidence", deployDeepDossier(), nil)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	fakeCodexProvider(t, regressionSkill)
	isolateSkillFormHome(t)

	for _, taskID := range []string{"theme:review", "lesson:release-evidence", "convention:release-evidence"} {
		t.Run(taskID, func(t *testing.T) {
			out, stderr, err := executeSplit(t, NewRootCommand(opts), "patterns", "skills", "form", taskID, repoDir, "--scope", "repo", "--agent", "codex", "--draft-only", "--json")
			if err != nil {
				t.Fatalf("form %s: %v\nstdout=%s\nstderr=%s", taskID, err, out, stderr)
			}
			var preview struct {
				TaskID  string `json:"task_id"`
				IsSkill bool   `json:"is_skill"`
				Name    string `json:"name"`
			}
			if err := json.Unmarshal([]byte(out), &preview); err != nil {
				t.Fatalf("decode preview: %v\n%s", err, out)
			}
			if preview.TaskID != taskID || !preview.IsSkill || preview.Name != "deploy-release" {
				t.Fatalf("form %s preview = %+v", taskID, preview)
			}
		})
	}

	assertNoSkillFormWrites(t, brainDir, repoDir)
}

// Rejected root-command routes must fail before synthesis and leave both the
// installation tree and the user-curated skill decision store untouched.
func TestRootCommandPatternsSkillsFormRejectsMissingProposalAndAgentNoneWithoutWrites(t *testing.T) {
	opts, repoDir, brainDir := commandRegressionFixture(t)
	newCorpusAtDir(t, brainDir)

	db, err := openPatternCorpusMutableDB(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO themes (id, scope, title, description, shape, member_keys, support, fingerprint, strength, status, verdict, created_at, updated_at)
		VALUES ('theme:available', 'repo', 'Review branch changes', 'Audit branch changes for regressions', 'conversation', '[]', 3, 'sha256:theme', 0.8, 'active', 'accepted', 't', 't')`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	seedAcceptedDeepDossier(t, db, "lesson:available", deployDeepDossier(), nil)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// Keep failures hermetic even if a guard regresses and a codex invocation is
	// reached. The isolated home also makes any accidental global target visible.
	fakeCodexProvider(t, regressionSkill)
	isolateSkillFormHome(t)

	for _, tc := range []struct {
		name     string
		args     []string
		wantErr  string
		wantHint string
	}{
		{
			name:     "missing accepted lesson",
			args:     []string{"patterns", "skills", "form", "lesson:missing", repoDir, "--scope", "repo", "--agent", "codex", "--yes"},
			wantErr:  `no accepted proposal "lesson:missing"`,
			wantHint: "--lessons",
		},
		{
			name:     "missing accepted theme",
			args:     []string{"patterns", "skills", "form", "theme:missing", repoDir, "--scope", "repo", "--agent", "codex", "--yes"},
			wantErr:  `no accepted theme "theme:missing"`,
			wantHint: "--themes",
		},
		{
			name:     "missing accepted convention",
			args:     []string{"patterns", "skills", "form", "convention:missing", repoDir, "--scope", "repo", "--agent", "codex", "--yes"},
			wantErr:  `no accepted proposal "convention:missing"`,
			wantHint: "--conventions",
		},
		{
			name:    "lesson with no synthesis agent",
			args:    []string{"patterns", "skills", "form", "lesson:available", repoDir, "--scope", "repo", "--agent", "none", "--yes"},
			wantErr: "skill synthesis requires an agent",
		},
		{
			name:    "theme with no synthesis agent",
			args:    []string{"patterns", "skills", "form", "theme:available", repoDir, "--scope", "repo", "--agent", "none", "--yes"},
			wantErr: "skill synthesis requires an agent",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := execute(t, NewRootCommand(opts), tc.args...)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v\n%s", err, out)
			}
			if tc.wantHint != "" && !strings.Contains(err.Error(), tc.wantHint) {
				t.Fatalf("error %q omitted verification hint %q", err, tc.wantHint)
			}
			assertNoSkillFormWrites(t, brainDir, repoDir)
		})
	}
}

func isolateSkillFormHome(t *testing.T) {
	t.Helper()
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("USERPROFILE", homeDir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(homeDir, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(homeDir, "data"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(homeDir, "state"))
}

func assertNoSkillFormWrites(t *testing.T, brainDir, repoDir string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(brainDir, filepath.FromSlash(patternsSkillMemoryPath))); !os.IsNotExist(err) {
		t.Fatalf("rejected form persisted skill memory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repoDir, ".agents", "skills")); !os.IsNotExist(err) {
		t.Fatalf("rejected form created skill files: %v", err)
	}
}
