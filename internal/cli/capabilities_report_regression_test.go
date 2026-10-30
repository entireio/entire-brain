package cli

import (
	"fmt"
	"strings"
	"testing"

	"github.com/entireio/entire-brain/internal/cli/command/capabilities"
	"github.com/entireio/entire-brain/internal/cli/internal/sqlitedriver"
)

func TestCapabilitiesHumanReportNamesBuildGatesWithoutRepositoryAccess(t *testing.T) {
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{}}
	opts := Options{Version: "regression-capability-version", Runner: runner, Env: EntireEnv{RepoRoot: "not-a-repository"}}
	out, err := execute(t, NewRootCommand(opts), "capabilities")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Entire Brain regression-capability-version",
		fmt.Sprintf("Build: %s (brain_cgo=%t)", sqlitedriver.Name, sqlitedriver.BrainCGO),
		"Retrieval: BM25 keyword (semantic retrieval removed)",
		"fact: keyword; semantic compiled=false (default)",
		"conversation: keyword; semantic compiled=false (experimental, explicit opt-in)",
		"Semantic requires: semantic retrieval removed (trail #278); BM25 lexical is the only ranking",
		"Repository readiness: entire brain status --details --json",
		"Languages and relations: entire graph capabilities --json",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("capabilities omitted %q: %s", want, out)
		}
	}
	if len(runner.calls) != 0 {
		t.Fatalf("capabilities invoked repository/provider commands: %+v", runner.calls)
	}
	for _, args := range [][]string{nil, {"--json"}} {
		cmd := capabilities.New(opts)
		cmd.SetOut(evalCommandFailWriter{})
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "eval output failure") {
			t.Fatalf("writer failure ignored: %v", err)
		}
	}
}
