package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

const briefOutputPrivateCanary = "PRIVATE_BRIEF_CANARY_739"

// These tests enter through the public root command. The profile fixture gives
// the brief real semantic and history stores; the changed-file overlay supplies
// the two current source files whose action contracts are asserted below.
func TestBriefOutputRootMetadataAndPreviousResponseActions(t *testing.T) {
	fixture := newBrainBriefOutputFixture(t)
	t.Setenv(envBrainActionChecklist, "true")
	task := "repair Responses API agentic_decision metadata string contract"
	packet, err := execute(t, NewRootCommand(fixture.opts), "brief", task, "--json", "--limit", "8")
	if err != nil {
		t.Fatalf("brief json: %v\n%s", err, packet)
	}
	var report brainBriefJSONReport
	if err := json.Unmarshal([]byte(packet), &report); err != nil {
		t.Fatalf("decode: %v\n%s", err, packet)
	}
	if report.Task != task || len(report.Guidance) == 0 || report.Status.Brain.ManifestState == "" {
		t.Fatalf("public report missing task/status/guidance: %#v", report)
	}
	if !slices.Contains(report.LikelyEditFiles, "src/agentic-decider-metadata.ts") {
		t.Fatalf("likely_edit_files omitted metadata provider: %#v", report.LikelyEditFiles)
	}
	if !briefOutputHasAction(report.ActionChecklist, "src/agentic-decider-metadata.ts", "Stringify the agentic_decision metadata step") {
		t.Fatalf("public metadata action omitted: %#v", report.ActionChecklist)
	}
	if len(report.ActionChecklist) > 20 {
		t.Fatalf("action budget exceeded: %d", len(report.ActionChecklist))
	}
	assertBriefOutputUniqueActions(t, report.ActionChecklist)
	if strings.Contains(packet, briefOutputPrivateCanary) {
		t.Fatalf("JSON packet leaked private fixture data: %s", packet)
	}

	text, err := execute(t, NewRootCommand(fixture.opts), "brief", task, "--limit", "8")
	if err != nil {
		t.Fatalf("brief text: %v\n%s", err, text)
	}
	for _, want := range []string{"task: " + task, "edit_file src/agentic-decider-metadata.ts", "action ", "Stringify the agentic_decision metadata step"} {
		if !strings.Contains(text, want) {
			t.Fatalf("text packet omitted %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, briefOutputPrivateCanary) {
		t.Fatalf("text packet leaked private fixture data: %s", text)
	}

	previousTask := "repair self-contained browser decision turns and previous_response_id carry forward"
	previousPacket, err := execute(t, NewRootCommand(fixture.opts), "brief", previousTask, "--json", "--limit", "8")
	if err != nil {
		t.Fatalf("previous-response brief: %v\n%s", err, previousPacket)
	}
	var previous brainBriefJSONReport
	if err := json.Unmarshal([]byte(previousPacket), &previous); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(previous.LikelyEditFiles, "packages/automation/agentic-decider.ts") {
		t.Fatalf("previous-response file absent: %#v", previous.LikelyEditFiles)
	}
	for _, action := range []string{"Delete the loop-scoped browser response-id accumulator", "literal property `previousResponseId: null`", "Delete this browser-loop response-id carry-forward assignment"} {
		if !briefOutputHasAction(previous.ActionChecklist, "packages/automation/agentic-decider.ts", action) {
			t.Fatalf("previous-response action %q absent: %#v", action, previous.ActionChecklist)
		}
	}
	assertBriefOutputUniqueActions(t, previous.ActionChecklist)
	if strings.Contains(previousPacket, briefOutputPrivateCanary) {
		t.Fatalf("previous-response packet leaked private fixture data: %s", previousPacket)
	}
}

func TestBriefOutputRootProvenanceOrderingDedupAndBudgets(t *testing.T) {
	fixture := newBrainBriefOutputFixture(t)
	task := "ValidateToken public history provenance ordering budget"
	fullPacket, err := execute(t, NewRootCommand(fixture.opts), "brief", task, "--json", "--limit", "8")
	if err != nil {
		t.Fatalf("full brief: %v\n%s", err, fullPacket)
	}
	var full brainBriefJSONReport
	if err := json.Unmarshal([]byte(fullPacket), &full); err != nil {
		t.Fatal(err)
	}
	if len(full.History.Matches) < 3 {
		t.Fatalf("fixture produced %d history matches, want >=3: %#v", len(full.History.Matches), full.History.Matches)
	}
	packet, err := execute(t, NewRootCommand(fixture.opts), "brief", task, "--json", "--limit", "2")
	if err != nil {
		t.Fatalf("brief: %v\n%s", err, packet)
	}
	var report brainBriefJSONReport
	if err := json.Unmarshal([]byte(packet), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Semantic.Context.Symbols) > 2 || len(report.Semantic.Tests.Suggestions) > 2 || len(report.History.Matches) > 2 {
		t.Fatalf("limit not honored: symbols=%d suggestions=%d history=%d", len(report.Semantic.Context.Symbols), len(report.Semantic.Tests.Suggestions), len(report.History.Matches))
	}
	if len(report.History.Matches) != 2 || !reflect.DeepEqual(report.History.Matches, full.History.Matches[:2]) {
		t.Fatalf("history budget/order mismatch\nlimited=%#v\nfull=%#v", report.History.Matches, full.History.Matches)
	}
	if len(report.LikelyEditFiles) > 8 || len(report.LikelyTestFiles) > 6 || len(report.LikelyFiles) > 12 {
		t.Fatalf("file budgets exceeded: edit=%d test=%d all=%d", len(report.LikelyEditFiles), len(report.LikelyTestFiles), len(report.LikelyFiles))
	}
	assertBriefOutputUniqueStrings(t, "likely_edit_files", report.LikelyEditFiles)
	assertBriefOutputUniqueStrings(t, "likely_test_files", report.LikelyTestFiles)
	assertBriefOutputUniqueStrings(t, "likely_files", report.LikelyFiles)
	seenHistory := map[string]struct{}{}
	for _, match := range report.History.Matches {
		if match.Path == "" || match.Line <= 0 || match.Excerpt == "" {
			t.Fatalf("history match lost provenance: %#v", match)
		}
		key := match.Path + "\x00" + match.Excerpt
		if _, duplicate := seenHistory[key]; duplicate {
			t.Fatalf("duplicate history evidence: %#v", report.History.Matches)
		}
		seenHistory[key] = struct{}{}
	}
	for i, file := range report.LikelyEditFiles {
		if i >= len(report.LikelyFiles) || report.LikelyFiles[i] != file {
			t.Fatalf("combined likely-file order does not preserve edit order: edits=%#v all=%#v", report.LikelyEditFiles, report.LikelyFiles)
		}
	}
	if strings.Contains(packet, briefOutputPrivateCanary) || strings.Contains(packet, "private-session.jsonl") {
		t.Fatalf("packet leaked unrelated hidden history: %s", packet)
	}
}

func TestBriefOutputRootMissingAndMalformedInputs(t *testing.T) {
	t.Run("missing indexes are explicit", func(t *testing.T) {
		opts, _, _ := commandRegressionFixture(t)
		out, err := execute(t, NewRootCommand(opts), "brief", "metadata", "--json", "--limit", "3")
		if err != nil {
			t.Fatalf("missing optional indexes: %v\n%s", err, out)
		}
		for _, warning := range []string{"semantic index missing", "history index missing"} {
			if !strings.Contains(out, warning) {
				t.Fatalf("missing input warning %q absent: %s", warning, out)
			}
		}
	})
	t.Run("malformed derived corpus fails closed", func(t *testing.T) {
		fixture := newBrainBriefOutputFixture(t)
		path := filepath.Join(fixture.brainDir, filepath.FromSlash(patternCorpusPath))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("not a sqlite database"), 0o600); err != nil {
			t.Fatal(err)
		}
		out, stderr, err := executeSplit(t, NewRootCommand(fixture.opts), "brief", "metadata", "--json", "--limit", "3")
		if err == nil {
			t.Fatalf("malformed corpus unexpectedly succeeded: %s", out)
		}
		if out != "" {
			t.Fatalf("malformed corpus emitted stdout %q (stderr=%q)", out, stderr)
		}
	})
	t.Run("malformed manifest fails closed before output", func(t *testing.T) {
		fixture := newBrainBriefOutputFixture(t)
		if err := os.WriteFile(filepath.Join(fixture.brainDir, exportManifestFileName), []byte("{not-json"), 0o600); err != nil {
			t.Fatal(err)
		}
		profile := filepath.Join(t.TempDir(), "profile.json")
		stdout, stderr, err := executeSplit(t, NewRootCommand(fixture.opts), "brief", "metadata", "--json", "--profile-json", profile)
		if err == nil {
			t.Fatalf("malformed manifest unexpectedly succeeded: %s", stdout)
		}
		if stdout != "" {
			t.Fatalf("malformed manifest emitted stdout %q (stderr=%q)", stdout, stderr)
		}
	})
	t.Run("malformed tombstones fail closed before output", func(t *testing.T) {
		fixture := newBrainBriefOutputFixture(t)
		path := filepath.Join(fixture.brainDir, filepath.FromSlash(sessionTombstonesPath))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{not-json"), 0o600); err != nil {
			t.Fatal(err)
		}
		stdout, stderr, err := executeSplit(t, NewRootCommand(fixture.opts), "brief", "metadata", "--json")
		if err == nil {
			t.Fatalf("malformed tombstones unexpectedly succeeded: %s", stdout)
		}
		if stdout != "" {
			t.Fatalf("malformed tombstones emitted stdout %q (stderr=%q)", stdout, stderr)
		}
	})
	t.Run("dirty privacy state blocks derived output", func(t *testing.T) {
		fixture := newBrainBriefOutputFixture(t)
		stones := loadSessionTombstones(fixture.brainDir)
		stones.Excluded["brief-evidence-1"] = sessionTombstone{At: fixture.opts.Now(), Reason: "brief regression"}
		if err := saveSessionTombstones(fixture.brainDir, stones); err != nil {
			t.Fatal(err)
		}
		stdout, stderr, err := executeSplit(t, NewRootCommand(fixture.opts), "brief", "ValidateToken", "--json")
		if err == nil || !strings.Contains(err.Error(), memoryErrPrivacyDirty) {
			t.Fatalf("dirty privacy state was not rejected: %v stdout=%s stderr=%s", err, stdout, stderr)
		}
		if stdout != "" {
			t.Fatalf("dirty privacy state emitted stdout %q", stdout)
		}
	})
	t.Run("malformed semantic store degrades with public warnings", func(t *testing.T) {
		fixture := newBrainBriefOutputFixture(t)
		manifest, err := loadBrainManifest(fixture.brainDir)
		if err != nil {
			t.Fatal(err)
		}
		store := filepath.Join(fixture.brainDir, filepath.FromSlash(manifest.Sources.Semantic.StorePath))
		if err := os.WriteFile(store, []byte("not sqlite"), 0o600); err != nil {
			t.Fatal(err)
		}
		profile := filepath.Join(t.TempDir(), "profile.json")
		out, err := execute(t, NewRootCommand(fixture.opts), "brief", "ValidateToken", "--json", "--limit", "3", "--profile-json", profile)
		if err != nil {
			t.Fatalf("corrupt semantic store should degrade: %v\n%s", err, out)
		}
		for _, warning := range []string{"semantic context unavailable", "runtime trace context unavailable", "test suggestions unavailable"} {
			if !strings.Contains(out, warning) {
				t.Fatalf("semantic warning %q absent: %s", warning, out)
			}
		}
	})
	t.Run("malformed fact store degrades with public warning", func(t *testing.T) {
		fixture := newBrainBriefProfileFixture(t)
		path := filepath.Join(fixture.brainDir, filepath.FromSlash(factsFileRelPath("feature")))
		if err := os.WriteFile(path, []byte("{not-json}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		profile := filepath.Join(t.TempDir(), "profile.json")
		out, err := execute(t, NewRootCommand(fixture.opts), "brief", "PRIVATE_FACT_PAYLOAD", "--json", "--limit", "3", "--profile-json", profile)
		if err != nil {
			t.Fatalf("corrupt fact store should degrade: %v\n%s", err, out)
		}
		if !strings.Contains(out, "facts unavailable") {
			t.Fatalf("fact warning absent: %s", out)
		}
	})
	t.Run("malformed history stores degrade with public warning", func(t *testing.T) {
		fixture := newBrainBriefOutputFixture(t)
		manifest, err := loadBrainManifest(fixture.brainDir)
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{filepath.Join(fixture.brainDir, filepath.FromSlash(manifest.Sources.History.IndexPath)), historyFTSDBPath(fixture.brainDir)} {
			if err := os.WriteFile(path, []byte("corrupt history"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		profile := filepath.Join(t.TempDir(), "profile.json")
		out, err := execute(t, NewRootCommand(fixture.opts), "brief", "ValidateToken", "--json", "--limit", "3", "--profile-json", profile)
		if err != nil {
			t.Fatalf("corrupt history should degrade: %v\n%s", err, out)
		}
		if !strings.Contains(out, "history context unavailable") {
			t.Fatalf("history warning absent: %s", out)
		}
	})
	t.Run("invalid budget is rejected", func(t *testing.T) {
		opts, _, _ := commandRegressionFixture(t)
		out, err := execute(t, NewRootCommand(opts), "brief", "metadata", "--limit", "0", "--json")
		if err == nil || !strings.Contains(err.Error(), "--limit must be greater than zero") {
			t.Fatalf("invalid budget: %v\n%s", err, out)
		}
	})
	t.Run("stale semantic input is explicit", func(t *testing.T) {
		fixture := newBrainBriefOutputFixture(t)
		fixture.opts.Runner.(*fakeCommandRunner).responses[fakeCommandKey("git", "rev-parse", "HEAD")] = fakeCommandResponse{stdout: "bbb222\n"}
		out, err := execute(t, NewRootCommand(fixture.opts), "brief", "ValidateToken", "--json", "--limit", "3")
		if err != nil {
			t.Fatalf("stale brief: %v\n%s", err, out)
		}
		if !strings.Contains(out, `"severity": "unsafe"`) || !strings.Contains(out, `"state": "stale"`) {
			t.Fatalf("stale input not explicit: %s", out)
		}
	})
}

func TestBriefOutputRawHistoryFailureIsPublicWarning(t *testing.T) {
	fixture := newBrainBriefOutputFixture(t)
	cmd := &cobra.Command{}
	var out strings.Builder
	cmd.SetOut(&out)
	matcher := func(string, string, []brainTextMatch, int, *brainBriefProfileRawHistory) ([]brainTextMatch, error) {
		return nil, errors.New("synthetic raw history failure")
	}
	err := runBrainBriefWithRawHistoryMatcher(context.Background(), cmd, fixture.opts, brainBriefOptions{limit: 3, json: true}, "ValidateToken", matcher)
	if err != nil {
		t.Fatalf("raw history fallback should degrade: %v", err)
	}
	if !strings.Contains(out.String(), "raw history fallback unavailable: synthetic raw history failure") {
		t.Fatalf("raw history warning absent: %s", out.String())
	}
}

func TestBriefOutputCurrentOverlaySupersedesLongTermHistory(t *testing.T) {
	fixture := newBrainBriefOutputFixture(t)
	manifest, err := loadBrainManifest(fixture.brainDir)
	if err != nil {
		t.Fatal(err)
	}
	const rel = "sessions/main/overlay-current.jsonl"
	overlay := shortTermIndex{
		ReconcilerVersion: historyShortTermReconcilerVersion,
		BaseGeneratedAt:   manifest.Sources.History.GeneratedAt,
		Files: map[string]shortTermFile{rel: {Records: []historyRecord{{
			ID: "overlay-current", Kind: "learning", Branch: "main", Path: rel, Line: 7,
			Summary: "OVERLAY_CURRENT_CANARY preserve current history", Terms: []string{"overlay", "current", "canary"},
		}}},
		},
	}
	if err := saveHistoryShortTerm(fixture.brainDir, overlay); err != nil {
		t.Fatal(err)
	}
	packet, err := execute(t, NewRootCommand(fixture.opts), "brief", "OVERLAY_CURRENT_CANARY", "--json", "--limit", "3")
	if err != nil {
		t.Fatalf("overlay brief: %v\n%s", err, packet)
	}
	var report brainBriefJSONReport
	if err := json.Unmarshal([]byte(packet), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.History.Matches) == 0 || report.History.Matches[0].Path != rel || report.History.Matches[0].Line != 7 || !strings.Contains(report.History.Matches[0].Excerpt, "OVERLAY_CURRENT_CANARY") {
		t.Fatalf("current overlay absent or lost provenance: %#v", report.History.Matches)
	}
}

// The root tests above prove these scanner results reach the public packet.
// This matrix pins fallback/primary selection and malformed-file behavior that
// cannot all coexist in one bounded likely-file list.
func TestBriefOutputActionScannerSelectionMatrix(t *testing.T) {
	repo := t.TempDir()
	files := map[string]string{
		"src/fallback.ts":                        "function fallback() {\n  const request = { metadata: { value: 7 } };\n}\n",
		"src/agentic-decider.ts":                 "function decide() {\n  const metadata = { agentic_decision: { step: 2 } };\n  return String(metadata);\n}\n",
		"src/secondary.ts":                       "function secondary() {\n  const request = { metadata: { value: 9 } };\n}\n",
		"packages/automation/agentic-decider.ts": "function executeAgenticPlan() {\n  let previousResponseId = null;\n  const perception: AgenticPerception = {\n    previousResponseId: null\n  };\n  previousResponseId = decision.previousResponseId;\n}\n",
	}
	for rel, body := range files {
		path := filepath.Join(repo, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	metadata := brainBriefMetadataStringActions(repo, []string{"README.md", "missing.ts", "src/fallback.ts", "src/agentic-decider.ts", "src/secondary.ts"})
	if len(metadata) == 0 {
		t.Fatal("metadata scanner returned no primary actions")
	}
	for _, action := range metadata {
		if action.File != "src/agentic-decider.ts" {
			t.Fatalf("fallback leaked beside primary action: %#v", metadata)
		}
	}
	if got := brainBriefMetadataStringActions(repo, []string{"src/fallback.ts", "src/secondary.ts"}); len(got) != 2 {
		t.Fatalf("metadata fallback actions=%d, want 2: %#v", len(got), got)
	}

	previous := brainBriefPreviousResponseActions(repo, []string{"missing.ts", "src/fallback.ts", "packages/automation/agentic-decider.ts"})
	if len(previous) != 3 {
		t.Fatalf("previous-response primary actions=%d, want 3: %#v", len(previous), previous)
	}
	if !strings.Contains(previous[1].Action, "Preserve self-contained") {
		t.Fatalf("literal-null preservation action absent: %#v", previous)
	}
	if got := brainBriefPreviousResponseActionsForFile("src/fallback.ts", "function x() {\n const perception: AgenticPerception = {\n previousResponseId\n };\n}\n"); len(got) != 1 || !strings.Contains(got[0].Action, "literal property") {
		t.Fatalf("shorthand action mismatch: %#v", got)
	}
	if err := os.WriteFile(filepath.Join(repo, "src", "previous-fallback.ts"), []byte("if (ready) {\n let previousResponseId = null;\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := brainBriefPreviousResponseActions(repo, []string{"README.md", "missing.ts", "src/previous-fallback.ts"}); len(got) != 1 || got[0].File != "src/previous-fallback.ts" {
		t.Fatalf("previous-response fallback mismatch: %#v", got)
	}
	if got := brainBriefMetadataStringActionsForFile("src/plain.ts", "function x() {\n const request = { metadata: { value: 1 } };\n const safe = String(metadata);\n}\n"); len(got) != 2 || !strings.Contains(got[1].Action, "Preserve string conversion") {
		t.Fatalf("generic metadata actions mismatch: %#v", got)
	}
	if got := brainBriefMetadataStringActionsForFile("src/provider.ts", "if (ready) {\n const unrelatedMetadata = true;\n}\nconst agentic_decision = { step: 1 };\n"); len(got) != 0 {
		t.Fatalf("provider-context filter accepted unrelated metadata: %#v", got)
	}

	var manyMetadata, manyPrevious strings.Builder
	for i := 0; i < 12; i++ {
		manyMetadata.WriteString("const request = { metadata: { value: 1 } };\n")
		manyPrevious.WriteString("let previousResponseId = null;\n")
	}
	if err := os.WriteFile(filepath.Join(repo, "src", "agentic-decider-many.ts"), []byte(manyMetadata.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "packages", "automation", "many.ts"), []byte("function executeAgenticPlan() {\nconst marker: AgenticPerception = {};\n"+manyPrevious.String()+"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := brainBriefMetadataStringActions(repo, []string{"src/agentic-decider-many.ts", "src/fallback.ts"}); len(got) != 12 {
		t.Fatalf("metadata primary scan cap path=%d, want 12", len(got))
	}
	if got := brainBriefPreviousResponseActions(repo, []string{"packages/automation/many.ts", "src/fallback.ts"}); len(got) != 12 {
		t.Fatalf("previous-response primary scan cap path=%d, want 12", len(got))
	}
}

func TestBriefOutputStatusAndProfileWriteFailures(t *testing.T) {
	fixture := newBrainBriefOutputFixture(t)
	profileDir := t.TempDir()
	out, err := execute(t, NewRootCommand(fixture.opts), "brief", "ValidateToken", "--json", "--profile-json", profileDir)
	if err == nil {
		t.Fatalf("directory profile path unexpectedly succeeded: %s", out)
	}
}

func TestBriefOutputWriterFailuresPropagate(t *testing.T) {
	for _, profiled := range []bool{false, true} {
		t.Run(map[bool]string{false: "packet", true: "profiled packet"}[profiled], func(t *testing.T) {
			fixture := newBrainBriefOutputFixture(t)
			cmd := &cobra.Command{}
			cmd.SetOut(briefOutputFailWriter{err: errors.New("synthetic brief writer failure")})
			opts := brainBriefOptions{limit: 3, json: true}
			if profiled {
				opts.profileJSON = filepath.Join(t.TempDir(), "profile.json")
			}
			err := runBrainBrief(context.Background(), cmd, fixture.opts, opts, "ValidateToken")
			if err == nil || !strings.Contains(err.Error(), "synthetic brief writer failure") {
				t.Fatalf("writer error not propagated: %v", err)
			}
		})
	}
}

func TestBriefOutputEmptyBranchUsesDefaultFactBranch(t *testing.T) {
	fixture := newBrainBriefOutputFixture(t)
	fixture.opts.Runner.(*fakeCommandRunner).responses[fakeCommandKey("git", "branch", "--show-current")] = fakeCommandResponse{}
	packet, err := execute(t, NewRootCommand(fixture.opts), "brief", "PRIVATE_FACT_PAYLOAD", "--json", "--limit", "3")
	if err != nil {
		t.Fatalf("empty-branch brief: %v\n%s", err, packet)
	}
	var report brainBriefJSONReport
	if err := json.Unmarshal([]byte(packet), &report); err != nil {
		t.Fatal(err)
	}
	if report.Status.Live.Branch != "" || len(report.Facts) != 0 {
		t.Fatalf("detached branch should use the empty default-main fact view: live=%#v facts=%#v", report.Status.Live, report.Facts)
	}
}

func TestBriefOutputRootPromotesGitIntentWhenNoActionsExist(t *testing.T) {
	fixture := newBrainBriefOutputFixture(t)
	const rel = "scripts/install-hooks.sh"
	path := filepath.Join(fixture.repoDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho install\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	task := "install hooks configuration"
	stream := &brainBriefGitIntentScriptedRunner{outputs: [][]byte{
		brainBriefGitIntentLogRecord(strings.Repeat("a", 40), task),
		[]byte(rel + "\x00"),
	}}
	fixture.opts.Runner = &briefOutputStreamingRunner{base: fixture.opts.Runner.(*fakeCommandRunner), stream: stream}
	packet, err := execute(t, NewRootCommand(fixture.opts), "brief", task, "--json", "--limit", "3", "--profile-json", filepath.Join(t.TempDir(), "profile.json"))
	if err != nil {
		t.Fatalf("git-intent brief: %v\n%s", err, packet)
	}
	var report brainBriefJSONReport
	if err := json.Unmarshal([]byte(packet), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.ActionChecklist) != 0 || !slices.Contains(report.LikelyEditFiles, rel) {
		t.Fatalf("git intent was not promoted: actions=%#v edits=%#v", report.ActionChecklist, report.LikelyEditFiles)
	}
}

func TestBriefOutputFusionEligibleHistoryFallsBackLexically(t *testing.T) {
	fixture := newBrainBriefOutputFixture(t)
	original := defaultEmbedderInst
	defaultEmbedderInst = &fakeFusionEmbedder{vecs: map[string][]float32{}}
	t.Cleanup(func() { defaultEmbedderInst = original })
	packet, err := execute(t, NewRootCommand(fixture.opts), "brief", "ValidateToken provenance", "--json", "--limit", "3", "--profile-json", filepath.Join(t.TempDir(), "profile.json"))
	if err != nil {
		t.Fatalf("fusion fallback brief: %v\n%s", err, packet)
	}
	if !strings.Contains(packet, "brief-evidence") {
		t.Fatalf("fusion fallback lost lexical history: %s", packet)
	}
}

type briefOutputStreamingRunner struct {
	base   *fakeCommandRunner
	stream *brainBriefGitIntentScriptedRunner
}

func (r *briefOutputStreamingRunner) Run(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
	return r.base.Run(ctx, dir, name, args...)
}

func (r *briefOutputStreamingRunner) Stream(ctx context.Context, dir, name string, args ...string) (CommandStream, error) {
	return r.stream.Stream(ctx, dir, name, args...)
}

type briefOutputFailWriter struct{ err error }

func (w briefOutputFailWriter) Write([]byte) (int, error) { return 0, w.err }

func TestBriefOutputRootFactReceiptAndConversationProvenance(t *testing.T) {
	fixture := newBrainBriefProfileFixture(t)
	t.Setenv("ENTIRE_BRAIN_BRIEF_CONVERSATION", "true")
	profile := filepath.Join(t.TempDir(), "profile.json")
	packet, err := execute(t, NewRootCommand(fixture.opts), "brief", "PRIVATE_FACT_PAYLOAD PRIVATE_HISTORY_PAYLOAD", "--json", "--limit", "4", "--profile-json", profile)
	if err != nil {
		t.Fatalf("fact/conversation brief: %v\n%s", err, packet)
	}
	var report brainBriefJSONReport
	if err := json.Unmarshal([]byte(packet), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Facts) == 0 || report.Facts[0].ID == "" || len(report.Facts[0].Paths) == 0 {
		t.Fatalf("fact lost public identity/provenance: %#v", report.Facts)
	}
	for _, fact := range report.Facts {
		publicFact := strings.Join([]string{fact.ID, strings.Join(fact.Paths, "\n"), fact.Kind, strings.Join(fact.Locus, "\n"), fact.Text, fact.Confidence}, "\n")
		if strings.Contains(publicFact, fixture.brainDir) || strings.Contains(publicFact, filepath.ToSlash(fixture.brainDir)) {
			t.Fatalf("fact leaked brain path: %#v", fact)
		}
	}
	if _, err := os.Stat(profile); err != nil {
		t.Fatalf("profile sidecar absent: %v", err)
	}
	for _, hit := range report.Conversation {
		if hit.ID == "" || hit.Path == "" || hit.Line <= 0 || hit.ContentRole != "historical_evidence" {
			t.Fatalf("conversation provenance incomplete: %#v", hit)
		}
	}
}

func TestBriefOutputLikelyFileGroupingUsesEveryPublicEvidenceLane(t *testing.T) {
	repo := t.TempDir()
	for _, rel := range []string{"src/context.go", "src/neighbor.go", "src/relation.go", "src/runtime.go", "src/evidence.go", "src/root.go", "src/suggested.go", "src/changed.go", "src/history.go", "src/named.go", "src/context_test.go"} {
		path := filepath.Join(repo, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("package fixture\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	report := brainBriefReport{
		Semantic: brainBriefSemantic{
			Context: semanticContextResult{
				Symbols:   []semanticRecord{{FilePath: "src/context.go", Path: "src/context_test.go"}, {FilePath: "../outside.go"}},
				Neighbors: []semanticRecord{{FilePath: "src/neighbor.go"}},
				Relations: []semanticRecord{{FilePath: "src/relation.go"}},
			},
			RuntimeTraces: []semanticRecord{{FilePath: "src/runtime.go", Evidence: []semanticEvidence{{FilePath: "src/evidence.go"}}}},
			Tests:         semanticTestsResult{Roots: []semanticRecord{{FilePath: "src/root.go"}}, Suggestions: []semanticTestSuggestion{{Symbol: semanticRecord{FilePath: "src/suggested.go"}, Reason: "direct"}}},
		},
		Status:  brainStatusReport{Live: brainLiveState{ChangedFiles: []string{"src/changed.go", "../hidden.go"}}},
		History: brainBriefHistory{Matches: []brainTextMatch{{Excerpt: "Edit src/history.go and ../secret.go"}}},
	}
	edits, tests, all := brainBriefLikelyFileGroupsForRepoAndFilenameCounts(repo, report, "context runtime", []string{"context", "runtime"}, map[string]int{"src/named.go": 50, "../../escape.go": 999})
	for _, want := range []string{"src/context.go", "src/neighbor.go", "src/relation.go", "src/runtime.go", "src/root.go", "src/suggested.go", "src/history.go", "src/named.go", "src/context_test.go"} {
		if !slices.Contains(edits, want) && !slices.Contains(all, want) {
			t.Fatalf("evidence lane omitted %q: edits=%#v tests=%#v all=%#v", want, edits, tests, all)
		}
	}
	if len(edits) > 8 || len(tests) > 6 || len(all) > 12 {
		t.Fatalf("group budgets exceeded: %d/%d/%d", len(edits), len(tests), len(all))
	}
	for _, hidden := range []string{"../outside.go", "../hidden.go", "../secret.go", "../../escape.go"} {
		if slices.Contains(all, hidden) {
			t.Fatalf("unsafe path leaked: %q in %#v", hidden, all)
		}
	}
	if gotEdits, gotTests, gotAll := brainBriefLikelyFileGroupsForRepoAndFilenameCounts(repo, brainBriefReport{}, "", nil, nil); len(gotEdits) != 0 || len(gotTests) != 0 || len(gotAll) != 0 {
		t.Fatalf("empty grouping=%#v/%#v/%#v", gotEdits, gotTests, gotAll)
	}
}

func newBrainBriefOutputFixture(t *testing.T) brainBriefProfileFixture {
	t.Helper()
	fixture := newBrainBriefProfileFixture(t)
	sources := map[string]string{
		"src/agentic-decider-metadata.ts":        "export function callProvider() {\n  const metadata = { agentic_decision: { step: 1 } };\n  return responses.create({ metadata, reasoningEffort, previousResponseId, images });\n}\n",
		"packages/automation/agentic-decider.ts": "export function executeAgenticPlan() {\n  let previousResponseId = null;\n  const perception: AgenticPerception = {\n    previousResponseId: previousResponseId\n  };\n  previousResponseId = decision.previousResponseId;\n}\n",
	}
	for rel, source := range sources {
		path := filepath.Join(fixture.repoDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	privatePath := filepath.Join(fixture.repoDir, ".git", "private-metadata.txt")
	if err := os.MkdirAll(filepath.Dir(privatePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(privatePath, []byte(briefOutputPrivateCanary), 0o600); err != nil {
		t.Fatal(err)
	}
	sessionDir := filepath.Join(fixture.brainDir, exportSessionsDirectory, "main")
	var exported []exportSession
	for i := 1; i <= 4; i++ {
		name := "brief-evidence-" + string(rune('0'+i)) + ".jsonl"
		rel := filepath.ToSlash(filepath.Join(exportSessionsDirectory, "main", name))
		body := `{"type":"agent_message","message":"Decision: ValidateToken public history provenance ordering budget evidence ` + string(rune('0'+i)) + ` in src/history.go"}` + "\n"
		if err := os.WriteFile(filepath.Join(sessionDir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		exported = append(exported, exportSession{SessionID: "brief-evidence-" + string(rune('0'+i)), Branch: "main", TranscriptPath: rel})
	}
	manifest, err := loadBrainManifest(fixture.brainDir)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Sources.Sessions = &sessionSourceManifest{TranscriptMode: "compact", Scope: exportScopeAll, Sessions: exported}
	if err := writeBrainManifestAndReadme(fixture.brainDir, *manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := writeBrainHistoryIndexAndSource(fixture.brainDir, fixture.opts.Now(), nil); err != nil {
		t.Fatalf("rebuild history fixture: %v", err)
	}
	runner := fixture.opts.Runner.(*fakeCommandRunner)
	runner.responses[fakeCommandKey("git", "status", "--porcelain", "--untracked-files=all")] = fakeCommandResponse{stdout: " M src/agentic-decider-metadata.ts\n M packages/automation/agentic-decider.ts\n"}
	runner.responses[fakeCommandKey("git", "diff", "--name-only", "HEAD")] = fakeCommandResponse{stdout: "src/agentic-decider-metadata.ts\npackages/automation/agentic-decider.ts\n"}
	runner.responses[fakeCommandKey("git", "diff", "--name-status", "-M", "-C", "HEAD")] = fakeCommandResponse{stdout: "M\tsrc/agentic-decider-metadata.ts\nM\tpackages/automation/agentic-decider.ts\n"}
	return fixture
}

func briefOutputHasAction(actions []brainBriefAction, file, action string) bool {
	for _, item := range actions {
		if item.File == file && strings.Contains(item.Action, action) && strings.Contains(item.Evidence, "current line") {
			return true
		}
	}
	return false
}

func assertBriefOutputUniqueActions(t *testing.T, actions []brainBriefAction) {
	t.Helper()
	seen := map[string]struct{}{}
	for _, action := range actions {
		key := strings.ToLower(action.File + "\x00" + action.Symbol + "\x00" + action.Action + "\x00" + action.Evidence)
		if _, ok := seen[key]; ok {
			t.Fatalf("duplicate public action: %#v", action)
		}
		seen[key] = struct{}{}
	}
}

func assertBriefOutputUniqueStrings(t *testing.T, name string, values []string) {
	t.Helper()
	seen := map[string]struct{}{}
	for _, value := range values {
		if _, ok := seen[value]; ok {
			t.Fatalf("%s contains duplicate %q: %#v", name, value, values)
		}
		seen[value] = struct{}{}
	}
}
