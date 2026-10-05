package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func regressionAttributionFile(path, body string) candFile {
	lines := strings.Split(body, "\n")
	norm := make([]string, len(lines))
	for i, line := range lines {
		norm[i] = regressionDespace(line)
	}
	return candFile{clean: path, lines: lines, norm: norm}
}

func TestRegressionDetectorAttributesDeletedAssignmentToRenamedHintedSite(t *testing.T) {
	session := `{"text":"pkg/renamed.go must set state.TranscriptPath = resolved before returning"}`
	body := "package pkg\nfunc renamed() string {\n\treplacement = resolved\n\treturn resolved\n}\n"
	brainDir, repoRoot := writeRegressionFixture(t, session, "pkg/renamed.go", body)
	noise := filepath.Join(repoRoot, "aaa", "unrelated.go")
	if err := os.MkdirAll(filepath.Dir(noise), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(noise, []byte("package aaa\nfunc noise() { replacement = resolved }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	anomalies, _, warnings, _ := detectRegressionAnomaliesCapped(brainDir, repoRoot, nil, "TranscriptPath resolved renamed", 20, true)
	if len(warnings) != 0 {
		t.Fatalf("unexpected detector warnings: %v", warnings)
	}
	var deleted []regressionAnomaly
	for _, anomaly := range anomalies {
		if anomaly.Kind == "deleted" {
			deleted = append(deleted, anomaly)
		}
	}
	if len(deleted) != 1 || deleted[0].File != "pkg/renamed.go" || deleted[0].Line != 3 || deleted[0].Current != "replacement = resolved" || !strings.Contains(strings.ToLower(deleted[0].Expected), "state.transcriptpath") {
		t.Fatalf("deleted assignment attribution=%+v all=%+v", deleted, anomalies)
	}

	if err := os.WriteFile(filepath.Join(repoRoot, "pkg", "renamed.go"), []byte("package pkg\nfunc renamed() string {\n\tstate.TranscriptPath = resolved\n\treturn resolved\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	anomalies, _, _, _ = detectRegressionAnomaliesCapped(brainDir, repoRoot, nil, "TranscriptPath resolved renamed", 20, true)
	for _, anomaly := range anomalies {
		if anomaly.Kind == "deleted" && anomaly.File == "pkg/renamed.go" {
			t.Fatalf("intact assignment produced deletion anomaly: %+v", anomaly)
		}
	}
}

func TestRegressionDeletionAttributionSelectsMeaningfulCurrentHome(t *testing.T) {
	files := []candFile{
		regressionAttributionFile("aaa/unrelated.go", "package aaa\n// State.Token = resolved\n"),
		regressionAttributionFile("pkg/renamed.go", "package pkg\nfunc update() {\n  state.Token = resolved\n}"),
		regressionAttributionFile("pkg/comment.go", "// State.Token = resolved\n"),
	}

	for _, tc := range []struct {
		name               string
		id, rhs            string
		hints              []string
		wantFile, wantText string
		wantLine           int
	}{
		{"hint wins over unrelated comment", "state.token", "resolved", []string{"pkg/renamed.go"}, "pkg/renamed.go", "state.Token = resolved", 3},
		{"rhs and identifier select implementation", "state.token", "resolved", nil, "pkg/renamed.go", "state.Token = resolved", 3},
		{"comment fallback only after code", "state.token", "resolved", []string{"pkg/comment.go"}, "pkg/comment.go", "// State.Token = resolved", 1},
		{"no match reports absent", "missing.symbol", "rhs", nil, "", "(absent)", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file, line, text := regressionHomeFile(files, tc.id, tc.rhs, tc.hints)
			if file != tc.wantFile || line != tc.wantLine || text != tc.wantText {
				t.Fatalf("home=(%q,%d,%q) want=(%q,%d,%q)", file, line, text, tc.wantFile, tc.wantLine, tc.wantText)
			}
		})
	}
}

func TestRegressionDeletionAttributionUsesHintedRHSForRenamedDeclaration(t *testing.T) {
	files := []candFile{
		regressionAttributionFile("pkg/old.go", "package pkg\nfunc unrelated() { value = stale }"),
		regressionAttributionFile("pkg/new.go", "package pkg\nfunc renamed() {\n  replacement = resolved\n}"),
	}
	file, line, text := regressionHomeFile(files, "deletedname", "resolved", []string{"pkg/new.go"})
	if file != "pkg/new.go" || line != 3 || text != "replacement = resolved" {
		t.Fatalf("renamed/deleted declaration home=(%q,%d,%q)", file, line, text)
	}
}
