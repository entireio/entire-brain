package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

const brainBriefRawHistoryBenchmarkTask = "ALPHA_IDENTIFIER BETA_IDENTIFIER GAMMA_IDENTIFIER DELTA_IDENTIFIER EPSILON_IDENTIFIER ZETA_IDENTIFIER ETA_IDENTIFIER THETA_IDENTIFIER"

var brainBriefRawHistoryBenchmarkSink []brainTextMatch
var brainBriefPacketBenchmarkSink int

func TestBrainBriefRawHistorySingleScanMatchesMultiScan(t *testing.T) {
	brainDir := writeBrainBriefRawHistoryComparisonFixture(t)
	base, err := brainBriefRawHistoryMatchesMultiScan(brainDir, brainBriefRawHistoryBenchmarkTask, nil, 8, nil)
	if err != nil {
		t.Fatalf("reference raw history: %v", err)
	}
	if len(base) == 0 {
		t.Fatal("comparison fixture produced no raw history matches")
	}

	tasks := []string{
		brainBriefRawHistoryBenchmarkTask,
		"GAMMA_IDENTIFIER ALPHA_IDENTIFIER",
		"No identifier-like query here",
		"DELTA_IDENTIFIER caféToken",
	}
	for _, task := range tasks {
		for _, limit := range []int{-1, 0, 1, 2, 4, 8, 1 << 30} {
			for _, existing := range [][]brainTextMatch{nil, base[:1]} {
				want, wantErr := brainBriefRawHistoryMatchesMultiScan(brainDir, task, existing, limit, nil)
				got, gotErr := brainBriefRawHistoryMatchesSingleScan(brainDir, task, existing, limit)
				if errorText(gotErr) != errorText(wantErr) {
					t.Fatalf("task=%q limit=%d existing=%d error=%q, want %q", task, limit, len(existing), errorText(gotErr), errorText(wantErr))
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("task=%q limit=%d existing=%d\nsingle: %#v\nreference: %#v", task, limit, len(existing), got, want)
				}
			}
		}
	}
}

func TestBrainBriefRawHistorySingleScanPreservesDefaultPacket(t *testing.T) {
	brainDir := writeBrainBriefRawHistoryComparisonFixture(t)
	want, err := brainBriefRawHistoryMatchesMultiScan(brainDir, brainBriefRawHistoryBenchmarkTask, nil, 8, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := brainBriefRawHistoryMatches(brainDir, brainBriefRawHistoryBenchmarkTask, nil, 8)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("default path changed raw-history packet inputs\nsingle: %#v\nreference: %#v", got, want)
	}
}

func TestBrainBriefRawHistorySingleScanHandlesMoreThan256Files(t *testing.T) {
	brainDir := t.TempDir()
	sessionDir := filepath.Join(brainDir, "sessions", "main")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 320; i++ {
		contents := "ordinary history line\n"
		if i == 319 {
			contents = "late ALPHA_IDENTIFIER match\n"
		}
		path := filepath.Join(sessionDir, fmt.Sprintf("20260716T010203Z_%04d.jsonl", i))
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	want, err := brainBriefRawHistoryMatchesMultiScan(brainDir, "ALPHA_IDENTIFIER", nil, 8, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := brainBriefRawHistoryMatchesSingleScan(brainDir, "ALPHA_IDENTIFIER", nil, 8)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("single scan changed after 320 files: got=%#v want=%#v", got, want)
	}
	if len(got) != 1 {
		t.Fatalf("matches = %d, want 1", len(got))
	}
}

func TestBrainBriefRawHistorySingleScanPreservesEndToEndPacket(t *testing.T) {
	fixture := newBrainBriefRawHistoryEndToEndFixture(t)
	want := runBrainBriefRawHistoryEndToEnd(t, fixture, brainBriefRawHistoryMatchesMultiScan)
	got := runBrainBriefRawHistoryEndToEnd(t, fixture, brainBriefRawHistoryMatchesObserved)
	if got != want {
		t.Fatalf("single scan changed end-to-end brief packet bytes\nsingle (%d):\n%s\nreference (%d):\n%s", len(got), got, len(want), want)
	}
}

func BenchmarkBrainBriefRawHistoryScanPaths(b *testing.B) {
	brainDir := b.TempDir()
	sessionDir := filepath.Join(brainDir, "sessions", "main")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		b.Fatal(err)
	}
	line := strings.Repeat("ordinary deterministic history payload for component behavior and regression coverage ", 14) + "\n"
	contents := strings.Repeat(line, 12)
	for i := 0; i < 48; i++ {
		path := filepath.Join(sessionDir, fmt.Sprintf("20260716T010203Z_%04d.jsonl", i))
		fileContents := contents
		if i == 47 {
			fileContents += strings.Join([]string{
				"late ALPHA_IDENTIFIER match",
				"late BETA_IDENTIFIER match",
				"late GAMMA_IDENTIFIER match",
				"late DELTA_IDENTIFIER match",
				"late EPSILON_IDENTIFIER match",
				"late ZETA_IDENTIFIER match",
				"late ETA_IDENTIFIER match",
				"late THETA_IDENTIFIER match",
			}, "\n") + "\n"
		}
		if err := os.WriteFile(path, []byte(fileContents), 0o600); err != nil {
			b.Fatal(err)
		}
	}
	if got := len(brainBriefRawHistoryQueries(brainBriefRawHistoryBenchmarkTask)); got != 8 {
		b.Fatalf("benchmark query count = %d, want 8", got)
	}
	want, err := brainBriefRawHistoryMatchesMultiScan(brainDir, brainBriefRawHistoryBenchmarkTask, nil, 8, nil)
	if err != nil {
		b.Fatal(err)
	}
	got, err := brainBriefRawHistoryMatchesSingleScan(brainDir, brainBriefRawHistoryBenchmarkTask, nil, 8)
	if err != nil {
		b.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		b.Fatalf("candidate output differs from reference: got=%#v want=%#v", got, want)
	}

	b.Run("multi_scan_reference", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			matches, runErr := brainBriefRawHistoryMatchesMultiScan(brainDir, brainBriefRawHistoryBenchmarkTask, nil, 8, nil)
			if runErr != nil {
				b.Fatal(runErr)
			}
			brainBriefRawHistoryBenchmarkSink = matches
		}
	})
	b.Run("single_scan_candidate", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			matches, runErr := brainBriefRawHistoryMatchesSingleScan(brainDir, brainBriefRawHistoryBenchmarkTask, nil, 8)
			if runErr != nil {
				b.Fatal(runErr)
			}
			brainBriefRawHistoryBenchmarkSink = matches
		}
	})
}

func BenchmarkBrainBriefEndToEndRawHistoryScanPaths(b *testing.B) {
	fixture := newBrainBriefRawHistoryEndToEndFixture(b)
	want := runBrainBriefRawHistoryEndToEnd(b, fixture, brainBriefRawHistoryMatchesMultiScan)
	got := runBrainBriefRawHistoryEndToEnd(b, fixture, brainBriefRawHistoryMatchesObserved)
	if got != want {
		b.Fatalf("candidate packet differs from reference: got=%s want=%s", got, want)
	}

	b.Run("multi_scan_reference", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			brainBriefPacketBenchmarkSink = runBrainBriefRawHistoryEndToEndLen(b, fixture, brainBriefRawHistoryMatchesMultiScan)
		}
	})
	b.Run("single_scan_candidate", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			brainBriefPacketBenchmarkSink = runBrainBriefRawHistoryEndToEndLen(b, fixture, brainBriefRawHistoryMatchesObserved)
		}
	})
}

type brainBriefRawHistoryEndToEndFixture struct {
	opts Options
}

func newBrainBriefRawHistoryEndToEndFixture(tb testing.TB) brainBriefRawHistoryEndToEndFixture {
	tb.Helper()
	repoDir := tb.TempDir()
	env := EntireEnv{
		RepoRoot:        repoDir,
		PluginConfigDir: tb.TempDir(),
		PluginDataDir:   tb.TempDir(),
		PluginStateDir:  tb.TempDir(),
		PluginCacheDir:  tb.TempDir(),
	}
	runner := semanticFixtureRunner(repoDir, semanticBoundaryFixtureSnapshot())
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time { return now }}
	storage, err := repoStoragePaths(context.Background(), runner, env, repoDir)
	if err != nil {
		tb.Fatalf("storage: %v", err)
	}
	writeBrainBriefRawHistoryEndToEndCorpus(tb, storage.BrainDir)
	if _, err := writeBrainHistoryIndexAndSource(storage.BrainDir, now, nil); err != nil {
		tb.Fatalf("write history index: %v", err)
	}
	runner.responses[fakeCommandKey("git", "status", "--porcelain")] = fakeCommandResponse{}
	runner.responses[fakeCommandKey("git", "status", "--porcelain", "--untracked-files=all")] = fakeCommandResponse{}
	runner.responses[fakeCommandKey("git", "diff", "--shortstat", "HEAD")] = fakeCommandResponse{}
	runner.responses[fakeCommandKey("git", "diff", "--name-status", "-M", "-C", "HEAD")] = fakeCommandResponse{}
	return brainBriefRawHistoryEndToEndFixture{opts: opts}
}

func writeBrainBriefRawHistoryEndToEndCorpus(tb testing.TB, brainDir string) {
	tb.Helper()
	sessionDir := filepath.Join(brainDir, "sessions", "main")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		tb.Fatal(err)
	}
	payload := strings.Repeat("ordinary deterministic history payload for component behavior and regression coverage ", 14)
	line := `{"type":"agent_message","message":"` + payload + `"}` + "\n"
	contents := strings.Repeat(line, 12)
	for i := 0; i < 48; i++ {
		fileContents := contents
		if i == 47 {
			for _, identifier := range strings.Fields(brainBriefRawHistoryBenchmarkTask) {
				fileContents += `{"type":"agent_message","message":"late ` + identifier + ` match"}` + "\n"
			}
		}
		path := filepath.Join(sessionDir, fmt.Sprintf("20260716T010203Z_%04d.jsonl", i))
		if err := os.WriteFile(path, []byte(fileContents), 0o600); err != nil {
			tb.Fatal(err)
		}
	}
}

func runBrainBriefRawHistoryEndToEnd(tb testing.TB, fixture brainBriefRawHistoryEndToEndFixture, matcher brainBriefRawHistoryMatcher) string {
	tb.Helper()
	var out bytes.Buffer
	if err := runBrainBriefRawHistoryEndToEndInto(&out, fixture, matcher); err != nil {
		tb.Fatalf("brain brief: %v", err)
	}
	return out.String()
}

func runBrainBriefRawHistoryEndToEndLen(tb testing.TB, fixture brainBriefRawHistoryEndToEndFixture, matcher brainBriefRawHistoryMatcher) int {
	tb.Helper()
	var out bytes.Buffer
	if err := runBrainBriefRawHistoryEndToEndInto(&out, fixture, matcher); err != nil {
		tb.Fatalf("brain brief: %v", err)
	}
	return out.Len()
}

func runBrainBriefRawHistoryEndToEndInto(out *bytes.Buffer, fixture brainBriefRawHistoryEndToEndFixture, matcher brainBriefRawHistoryMatcher) error {
	cmd := &cobra.Command{}
	cmd.SetOut(out)
	briefOpts := brainBriefOptions{json: true, limit: brainBriefDefaultLimit, noSemantic: true}
	return runBrainBriefWithRawHistoryMatcher(context.Background(), cmd, fixture.opts, briefOpts, brainBriefRawHistoryBenchmarkTask, matcher)
}

func writeBrainBriefRawHistoryComparisonFixture(t *testing.T) string {
	t.Helper()
	brainDir := t.TempDir()
	files := map[string]string{
		"notes.md": "unrelated\nDELTA_IDENTIFIER appears in a root note\n",
		"sessions/main/20260716T010203Z_0001.jsonl": strings.Join([]string{
			"unrelated first line",
			"ALPHA_IDENTIFIER handles cache invalidation",
			"BETA_IDENTIFIER and ALPHA_IDENTIFIER share one line",
			strings.Repeat("prefix ", 70) + "GAMMA_IDENTIFIER" + strings.Repeat(" suffix", 120),
		}, "\n") + "\n",
		"sessions/main/20260716T020304Z_0002.txt": strings.Join([]string{
			"EPSILON_IDENTIFIER uses unicode café behavior",
			"ZETA_IDENTIFIER validates normalized_punctuation",
			"ETA_IDENTIFIER and THETA_IDENTIFIER close the fixture",
		}, "\n") + "\n",
		"history/index.json": "ALPHA_IDENTIFIER excluded derived index\n",
		"manifest.json":      "BETA_IDENTIFIER excluded manifest\n",
		"seed/source.txt":    "GAMMA_IDENTIFIER excluded seed\n",
		"semantic/data.txt":  "DELTA_IDENTIFIER excluded semantic data\n",
		".git/config.txt":    "EPSILON_IDENTIFIER excluded git data\n",
	}
	for rel, contents := range files {
		path := filepath.Join(brainDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return brainDir
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
