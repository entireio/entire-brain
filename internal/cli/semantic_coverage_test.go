package cli

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// The provider is the only witness to its own coverage, and a file it never
// considered is in none of its warnings, none of its partial failures and none
// of its counts. entire-graph applies Go build constraints for the HOST
// platform, so on darwin a //go:build linux file -- or any _linux.go /
// _windows.go file -- is dropped before parsing while the summary still reports
// parsed_files == files, partial_failures == 0 and completeness_level "ok".
//
// Reproduced against the real provider (entire-graph dev, darwin/arm64) with a
// 7-file repository: a_linux.go, b_linux.go, c_tagged.go (//go:build linux) and
// f_windows.go were all absent from `entire graph snapshot`, which reported
// warnings: [] and partial_failures: 0.
//
// This test pins the brain's answer to that: the tracked file the provider
// parsed the NEIGHBOURS of, skipped, and said nothing about is named in the
// manifest. Before the fix the manifest carried no record of it at all.
func TestSemanticIndexNamesAFileTheProviderSilentlySkipped(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)

	runner := semanticFixtureRunner(repoDir, semanticCoverageSnapshot())
	runner.responses[fakeCommandKey("git", "ls-tree", "-r", "--name-only", "-z", "aaa111")] = fakeCommandResponse{
		stdout: strings.Join([]string{
			"internal/shop/pricing.go",
			"internal/shop/pricing_windows.go",
			"go.mod",
			"vendor/other/vendored.go",
		}, "\x00"),
	}

	source := semanticCoverageIndex(t, env, runner, repoDir)

	got := semanticWarningPathsWithCode(source.Warnings, semanticUnreportedSkipCode)
	if len(got) != 1 || got[0] != "internal/shop/pricing_windows.go" {
		t.Fatalf("the silently skipped file is not named in the manifest; unreported-skip warnings = %v\nall warnings: %s", got, semanticWarningDump(source.Warnings))
	}

	// The warning has to say what is LOST, not merely that something happened:
	// a reader who only sees a code cannot tell whether the file mattered.
	for _, warning := range source.Warnings {
		if warning.Code != semanticUnreportedSkipCode {
			continue
		}
		if !strings.Contains(warning.Effect, "not parsed") {
			t.Fatalf("the warning does not say the file went unparsed: %+v", warning)
		}
	}

}

// The control, in the same shape: when the provider emits every tracked file,
// the index must stay silent. A coverage check that cries wolf on a healthy
// repository is worse than none.
func TestSemanticIndexIsSilentWhenTheProviderParsedEverything(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)

	runner := semanticFixtureRunner(repoDir, semanticCoverageSnapshot())
	runner.responses[fakeCommandKey("git", "ls-tree", "-r", "--name-only", "-z", "aaa111")] = fakeCommandResponse{
		stdout: strings.Join([]string{"internal/shop/pricing.go", "go.mod"}, "\x00"),
	}

	source := semanticCoverageIndex(t, env, runner, repoDir)
	if got := semanticWarningPathsWithCode(source.Warnings, semanticUnreportedSkipCode); len(got) != 0 {
		t.Fatalf("a fully parsed repository was flagged: %v", got)
	}
}

// A file the provider DID speak about is accounted for, whatever it said. Only
// silence is a finding -- otherwise every parse error would be reported twice,
// once as a partial failure and once as a phantom silent skip.
func TestSemanticIndexDoesNotDoubleReportAnAnnouncedSkip(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)

	runner := semanticFixtureRunner(repoDir, semanticCoverageSnapshotWithPartialFailure())
	runner.responses[fakeCommandKey("git", "ls-tree", "-r", "--name-only", "-z", "aaa111")] = fakeCommandResponse{
		stdout: strings.Join([]string{"internal/shop/pricing.go", "internal/shop/broken.go"}, "\x00"),
	}

	source := semanticCoverageIndex(t, env, runner, repoDir)
	if got := semanticWarningPathsWithCode(source.Warnings, semanticUnreportedSkipCode); len(got) != 0 {
		t.Fatalf("a file the provider already reported was reported a second time: %v", got)
	}
}

// The policy itself, without a provider or a repository behind it. Each case is
// one of the four filters that keep the check from second-guessing the
// provider's file selection.
func TestSemanticUnreportedSkipsIsNarrow(t *testing.T) {
	parsed := map[string]struct{}{
		"internal/shop/pricing.go": {},
		"web/app.ts":               {},
		// An extensionless parsed file, somewhere else entirely. It is what
		// makes a cross product of {directories} x {extensions} wrong: with
		// independent sets, this one file licenses flagging EVERY
		// extensionless file in every parsed directory.
		"mise-tasks/lint/go": {},
	}
	tracked := []string{
		"internal/shop/pricing.go",         // parsed
		"internal/shop/pricing_windows.go", // SILENT SKIP: same dir, same ext
		"internal/shop/README.md",          // no parsed .md sibling
		"internal/shop/broken.go",          // announced as a partial failure
		"go.mod",                           // no parsed .mod sibling
		"vendor/other/vendored.go",         // dir the provider parsed nothing from
		"node_modules/pkg/index.ts",        // dir the provider parsed nothing from
		"internal/shop/ignored.go",         // filtered by .brainignore
		"web/app.ts",                       // parsed
		"LICENSE",                          // extensionless, no parsed sibling
		"web/LICENSE",                      // extensionless, no parsed sibling
	}
	reported := []semanticWarning{{Code: semanticParseErrorCode, Path: "internal/shop/broken.go"}}
	ignore := brainIgnoreForTest(t, "internal/shop/ignored.go")

	got := semanticUnreportedSkips(tracked, parsed, reported, ignore)
	want := []string{"internal/shop/pricing_windows.go"}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("semanticUnreportedSkips = %v, want %v", got, want)
	}

	// No evidence of what the provider parses means no basis for a finding.
	if got := semanticUnreportedSkips(tracked, map[string]struct{}{}, nil, ignore); got != nil {
		t.Fatalf("an empty parsed set produced findings: %v", got)
	}
	if got := semanticUnreportedSkips(nil, parsed, nil, ignore); got != nil {
		t.Fatalf("an empty tracked set produced findings: %v", got)
	}
}

// A repository that trips this in bulk needs the count, not one manifest entry
// per file.
func TestSemanticUnreportedSkipWarningsAreBounded(t *testing.T) {
	missing := make([]string, semanticUnreportedSkipLimit+7)
	for i := range missing {
		missing[i] = filepath.ToSlash(filepath.Join("pkg", "f"+string(rune('a'+i%26))+".go"))
	}
	warnings := semanticUnreportedSkipWarnings(missing)
	if len(warnings) != semanticUnreportedSkipLimit+1 {
		t.Fatalf("warnings = %d, want %d listed plus one overflow", len(warnings), semanticUnreportedSkipLimit+1)
	}
	overflow := warnings[len(warnings)-1]
	if overflow.Path != "" {
		t.Fatalf("the overflow warning names a single path: %+v", overflow)
	}
	// The total has to survive truncation, or the reader is told a smaller
	// number than the truth.
	if !strings.Contains(overflow.Detail, "32 tracked source file(s)") {
		t.Fatalf("the overflow warning loses the true total: %q", overflow.Detail)
	}
	if got := semanticUnreportedSkipWarnings(nil); got != nil {
		t.Fatalf("no missing files produced warnings: %v", got)
	}
}

// ---- helpers ----

func semanticCoverageIndex(t *testing.T, env EntireEnv, runner *fakeCommandRunner, repoDir string) *semanticSourceManifest {
	t.Helper()
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time {
		return time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	}}
	cmd := &cobra.Command{Use: "index"}
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	// semanticFixtureRunner scripts an origin remote, so the store lands under
	// the remote-derived key rather than a local one.
	manifest, err := loadBrainManifest(filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo"))
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if manifest.Sources == nil || manifest.Sources.Semantic == nil {
		t.Fatal("the manifest records no semantic source")
	}
	return manifest.Sources.Semantic
}

func semanticWarningPathsWithCode(warnings []semanticWarning, code string) []string {
	var out []string
	for _, warning := range warnings {
		if warning.Code == code && warning.Path != "" {
			out = append(out, warning.Path)
		}
	}
	return out
}

func semanticWarningDump(warnings []semanticWarning) string {
	data, _ := json.Marshal(warnings)
	return string(data)
}

func brainIgnoreForTest(t *testing.T, patterns ...string) brainIgnore {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".brainignore"), []byte(strings.Join(patterns, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ignore, err := loadBrainIgnore(dir)
	if err != nil {
		t.Fatal(err)
	}
	return ignore
}

func semanticCoverageSnapshot() string {
	return `{"schema_version":"` + semanticSchemaVersion + `","provider":"entire-graph","provider_version":"0.1.0","repo_key":"gh/example/repo","commit":"aaa111","tree":"tree111","warnings":[],"partial_failures":[]}
{"record_type":"file","path":"internal/shop/pricing.go","language":"Go"}
{"record_type":"symbol","id":"gh/example/repo:Go:internal/shop/pricing.go:function:ApplyTax","kind":"function","name":"ApplyTax","qualified_name":"ApplyTax","file_path":"internal/shop/pricing.go","start_line":1,"end_line":3,"language":"Go","stable_id_version":"1"}
{"record_type":"summary","warnings":[],"partial_failures":[],"stats":{"files":1,"parsed_files":1,"symbols":1,"relations":0,"partial_failures":0,"completeness_level":"ok"}}
`
}

func semanticCoverageSnapshotWithPartialFailure() string {
	return `{"schema_version":"` + semanticSchemaVersion + `","provider":"entire-graph","provider_version":"0.1.0","repo_key":"gh/example/repo","commit":"aaa111","tree":"tree111","warnings":[],"partial_failures":[]}
{"record_type":"file","path":"internal/shop/pricing.go","language":"Go"}
{"record_type":"symbol","id":"gh/example/repo:Go:internal/shop/pricing.go:function:ApplyTax","kind":"function","name":"ApplyTax","qualified_name":"ApplyTax","file_path":"internal/shop/pricing.go","start_line":1,"end_line":3,"language":"Go","stable_id_version":"1"}
{"record_type":"summary","warnings":[],"partial_failures":[{"code":"` + semanticParseErrorCode + `","severity":"warning","file_path":"internal/shop/broken.go","effect_on_semantic_completeness":"file parsed with syntax errors"}],"stats":{"files":2,"parsed_files":1,"symbols":1,"relations":0,"partial_failures":1,"completeness_level":"degraded"}}
`
}
