package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func graphContextFixtureResponse() string {
	return `{"schema_version":"1.1","commit":"aaa111","tree":"tree111",` +
		`"symbols":[{"id":"gc-root","name":"NormalizeQueryLimit","qualified_name":"b.NormalizeQueryLimit","kind":"function","file_path":"packages/store/query.ts","start_line":1,"end_line":8,"signature":"function NormalizeQueryLimit(limit: number)","language":"TypeScript","score":12.5},` +
		`{"id":"gc-helper","name":"clampLimit","qualified_name":"b.clampLimit","kind":"function","file_path":"packages/store/query.ts","start_line":10,"end_line":14,"score":4.2}],` +
		`"relations":[{"from_id":"gc-root","to_id":"gc-helper","type":"CALLS","confidence":1},` +
		`{"from_id":"gc-test","to_id":"gc-root","type":"TESTS","confidence":1}],` +
		`"tests":[{"id":"gc-test","name":"TestNormalizeQueryLimit","qualified_name":"d.TestNormalizeQueryLimit","kind":"test","file_path":"packages/store/query.test.ts","start_line":1,"end_line":8,"score":9.1}],` +
		`"latency_ms":42}`
}

func runGraphContextBriefJSON(t *testing.T, fixture brainBriefSemanticFreshnessFixtureEnv) brainBriefJSONReport {
	t.Helper()
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	err := runBrainBrief(context.Background(), cmd, fixture.opts, brainBriefOptions{
		json:        true,
		limit:       8,
		graphBinary: "entire",
	}, "normalize query limit")
	if err != nil {
		t.Fatalf("brief: %v\n%s", err, out.String())
	}
	var report brainBriefJSONReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("parse brief JSON: %v\n%s", err, out.String())
	}
	return report
}

func graphContextCommandKey(repoDir string) string {
	return fakeCommandKey("entire", "graph", "context", "--repo", repoDir,
		"--task", "normalize query limit", "--limit", "8", "--tests-limit", "8")
}

// TestBrainBriefDelegatesSemanticContextToGraphStore pins COR-2217's
// delegation contract: when `entire graph context` answers, the brief's
// semantic section is served from the graph's store (source=graph-context)
// and the internal semantic index is not consulted for context or tests.
func TestBrainBriefDelegatesSemanticContextToGraphStore(t *testing.T) {
	fixture := newBrainBriefSemanticFreshnessFixture(t)
	runner := fixture.opts.Runner.(*fakeCommandRunner)
	runner.responses[graphContextCommandKey(fixture.repoDir)] = fakeCommandResponse{stdout: graphContextFixtureResponse()}

	report := runGraphContextBriefJSON(t, fixture)
	if report.Semantic.Source != "graph-context" {
		t.Fatalf("semantic source = %q, want graph-context (warnings: %v)", report.Semantic.Source, report.Warnings)
	}
	if len(report.Semantic.Context.Symbols) != 2 || report.Semantic.Context.Symbols[0].Name != "NormalizeQueryLimit" {
		t.Fatalf("delegated symbols = %+v", report.Semantic.Context.Symbols)
	}
	// The CALLS edge joins two returned symbols and survives; the TESTS edge's
	// test endpoint is not a context symbol, so it is conveyed only by the
	// tests section (the packet's relation contract).
	if len(report.Semantic.Context.Relations) != 1 || report.Semantic.Context.Relations[0].Type != "CALLS" {
		t.Fatalf("delegated relations = %+v", report.Semantic.Context.Relations)
	}
	if len(report.Semantic.Tests.Suggestions) != 1 || report.Semantic.Tests.Suggestions[0].Symbol.Name != "TestNormalizeQueryLimit" {
		t.Fatalf("delegated test suggestions = %+v", report.Semantic.Tests.Suggestions)
	}
	// The internal index would have surfaced current-root from the fixture
	// snapshot; its absence proves the internal path was skipped.
	for _, symbol := range report.Semantic.Context.Symbols {
		if symbol.ID == "current-root" {
			t.Fatalf("internal semantic record leaked into delegated context: %+v", symbol)
		}
	}
}

// TestBrainBriefUsesGraphProjectionWhenContextUnavailable pins the degradation
// contract: store_unavailable (and equally a missing or too-old plugin) must
// use the Graph-owned read-only projection when native context is unavailable.
func TestBrainBriefUsesGraphProjectionWhenContextUnavailable(t *testing.T) {
	fixture := newBrainBriefSemanticFreshnessFixture(t)
	runner := fixture.opts.Runner.(*fakeCommandRunner)
	runner.responses[graphContextCommandKey(fixture.repoDir)] = fakeCommandResponse{
		stdout: `{"schema_version":"1.1","symbols":[],"relations":[],"tests":[],"store_unavailable":true,"detail":"search store not built for this commit"}`,
	}

	report := runGraphContextBriefJSON(t, fixture)
	if report.Semantic.Source != "" {
		t.Fatalf("semantic source = %q, want internal (empty)", report.Semantic.Source)
	}
	assertInternalSemanticServed(t, report)
}

// TestBrainBriefFallsBackWhenGraphStoreEmpty pins that a successful but
// empty provider response does not displace the internal answer: zero
// symbols means the store had nothing for the task, not that the task has no
// context.
func TestBrainBriefFallsBackWhenGraphStoreEmpty(t *testing.T) {
	fixture := newBrainBriefSemanticFreshnessFixture(t)
	runner := fixture.opts.Runner.(*fakeCommandRunner)
	runner.responses[graphContextCommandKey(fixture.repoDir)] = fakeCommandResponse{
		stdout: `{"schema_version":"1.1","commit":"aaa111","tree":"tree111","symbols":[],"relations":[],"tests":[],"latency_ms":3}`,
	}

	report := runGraphContextBriefJSON(t, fixture)
	if report.Semantic.Source != "" {
		t.Fatalf("semantic source = %q, want internal (empty)", report.Semantic.Source)
	}
	assertInternalSemanticServed(t, report)
}

// TestBrainBriefFallsBackWhenGraphPluginAbsent covers the user-required
// no-graph degradation: the unscripted command errors like a machine without
// the plugin, and the brief continues without code context.
func TestBrainBriefFallsBackWhenGraphPluginAbsent(t *testing.T) {
	fixture := newBrainBriefSemanticFreshnessFixture(t)
	fixture.opts.Runner.(*fakeCommandRunner).graphStoreFixtures = nil

	report := runGraphContextBriefJSON(t, fixture)
	if report.Semantic.Source != "" {
		t.Fatalf("semantic source = %q, want internal (empty)", report.Semantic.Source)
	}
	if len(report.Semantic.Context.Symbols) != 0 || len(report.Semantic.Tests.Suggestions) != 0 {
		t.Fatal("code context survived absent Graph")
	}
	for _, warning := range report.Warnings {
		if strings.Contains(warning, "graph context") {
			t.Fatalf("plugin absence must degrade silently, got warning %q", warning)
		}
	}
}

func assertInternalSemanticServed(t *testing.T, report brainBriefJSONReport) {
	t.Helper()
	found := false
	for _, symbol := range report.Semantic.Context.Symbols {
		if symbol.Name == "NormalizeQueryLimit" {
			found = true
		}
	}
	if !found {
		t.Fatalf("internal semantic context missing expected symbol: %+v (warnings: %v)", report.Semantic.Context.Symbols, report.Warnings)
	}
}

// TestBrainBriefStoreUnavailableWarningCarriesGraphRemedy pins COR-2257: when
// the graph plugin answers but has no store for the current commit, and no
// internal semantic source exists either, the brief's warning must carry
// graph's own remedy (build the graph-owned store) instead of advising a full
// `brain refresh`, which rebuilds the brain-side snapshot the architecture
// retired with COR-2217. Since COR-2314 the refusal that reaches the warning
// is the status build's store probe (`graph store`), the one graph call the
// brief still makes in this state.
func TestBrainBriefStoreUnavailableWarningCarriesGraphRemedy(t *testing.T) {
	detail := "search store not built for this commit: run `entire graph index --head` with ENTIRE_GRAPH_SEARCH_STORE=1"
	for name, c := range map[string]struct{ detail, want string }{
		"graph detail carried":               {detail: detail, want: detail},
		"empty detail falls back to brain's": {detail: "", want: "refresh index"},
	} {
		t.Run(name, func(t *testing.T) {
			opts, repoDir, _ := commandRegressionFixture(t)
			runner := opts.Runner.(*fakeCommandRunner)
			response, err := json.Marshal(map[string]any{"contract_version": 1, "store_unavailable": true, "detail": c.detail})
			if err != nil {
				t.Fatal(err)
			}
			runner.responses[fakeCommandKey("entire", "graph", "store", "--repo", repoDir,
				"--profile", "full")] = fakeCommandResponse{stdout: string(response)}

			out, err := execute(t, NewRootCommand(opts), "brief", "metadata", "--json", "--limit", "3")
			if err != nil {
				t.Fatalf("brief: %v\n%s", err, out)
			}
			var report brainBriefJSONReport
			if err := json.Unmarshal([]byte(out), &report); err != nil {
				t.Fatal(err)
			}
			carried := false
			for _, warning := range report.Warnings {
				if strings.Contains(warning, "semantic index missing") {
					t.Fatalf("stale brain-side advice survived: %q", warning)
				}
				// The probe's wrapped error must not leak: it re-prefixes the
				// detail and appends a second remedy, and with an empty detail
				// it renders "Graph store unavailable: ; run ..." instead of
				// brainBriefCodeContextWarning's designed fallback.
				if strings.Contains(warning, "Graph store unavailable") {
					t.Fatalf("store-probe error text leaked into the warning: %q", warning)
				}
				if strings.Contains(warning, "code context unavailable") && strings.Contains(warning, c.want) {
					carried = true
				}
			}
			if !carried {
				t.Fatalf("graph remedy not carried into warnings: %v", report.Warnings)
			}
		})
	}
}

// TestBrainBriefSkipsGraphContextWhenStoreProbeAlreadyRefused pins COR-2314's
// no-repeat rule: the status build's store probe already heard "no store for
// this commit" from the provider. Without ancestor support a `graph context`
// subprocess can only repeat that refusal, so the brief must not spawn it —
// and the warning must still carry the provider's remedy.
func TestBrainBriefSkipsGraphContextWhenStoreProbeAlreadyRefused(t *testing.T) {
	t.Cleanup(func() { graphAncestorSupport.Delete("entire") })
	opts, repoDir, _ := commandRegressionFixture(t)
	runner := opts.Runner.(*fakeCommandRunner)
	// No graphStoreFixtures entry: the fake provider answers `graph store` with
	// store_unavailable, and the unscripted capabilities probe fails closed
	// (no ancestor answers), so delegation has nothing left to ask.

	out, err := execute(t, NewRootCommand(opts), "brief", "metadata", "--json", "--limit", "3")
	if err != nil {
		t.Fatalf("brief: %v\n%s", err, out)
	}
	for _, call := range runner.calls {
		if len(call.args) > 1 && call.args[0] == "graph" && call.args[1] == "context" {
			t.Fatalf("brief spawned graph context for %s after the store probe already refused", repoDir)
		}
	}
	var report brainBriefJSONReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	carried := false
	for _, warning := range report.Warnings {
		if strings.Contains(warning, "code context unavailable") && strings.Contains(warning, "refresh index") {
			carried = true
		}
	}
	if !carried {
		t.Fatalf("store-probe remedy not carried into warnings: %v", report.Warnings)
	}
}

// TestBrainBriefCustomGraphBinaryStillAskedAfterDefaultProbeRefusal pins the
// skip's binary scope: the status build's store probe always runs the default
// `entire` binary, so its refusal says nothing about a CLI named with
// --graph-binary. Delegation must still ask that binary.
func TestBrainBriefCustomGraphBinaryStillAskedAfterDefaultProbeRefusal(t *testing.T) {
	t.Cleanup(func() { graphAncestorSupport.Delete("mycli") })
	opts, repoDir, _ := commandRegressionFixture(t)
	runner := opts.Runner.(*fakeCommandRunner)
	// The default-binary store probe stays unscripted, so it answers
	// store_unavailable; the custom binary answers context with real symbols.
	runner.responses[fakeCommandKey("mycli", "graph", "context", "--repo", repoDir,
		"--task", "metadata", "--limit", "3", "--tests-limit", "3")] = fakeCommandResponse{stdout: graphContextFixtureResponse()}

	out, err := execute(t, NewRootCommand(opts), "brief", "metadata", "--json", "--limit", "3", "--graph-binary", "mycli")
	if err != nil {
		t.Fatalf("brief: %v\n%s", err, out)
	}
	var report brainBriefJSONReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	if report.Semantic.Source != "graph-context" {
		t.Fatalf("semantic source = %q, want graph-context: the default probe's refusal suppressed the custom binary (warnings: %v)", report.Semantic.Source, report.Warnings)
	}
}
