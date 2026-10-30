package cli

import (
	"os"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
)

func TestDashHomeViewSourcesAndNotBuilt(t *testing.T) {
	// An empty manifest (no sources, zero generated_at) flags "not built".
	report := brainStatusReport{Sources: brainStatusSources{}}
	empty := &exportManifest{}
	home := dashHomeView(report, empty, "main")
	if len(home.Sources) != 5 {
		t.Fatalf("want 5 source rows, got %d", len(home.Sources))
	}
	foundNotBuilt := false
	for _, w := range home.Warnings {
		if w == "brain not built yet — run `"+base.BrainInvocation(os.LookupEnv, "refresh")+"`" {
			foundNotBuilt = true
		}
	}
	if !foundNotBuilt {
		t.Errorf("empty brain should warn it is not built; warnings=%v", home.Warnings)
	}

	// A built brain with sources present should not warn "not built" and should
	// carry per-source detail.
	built := brainStatusReport{
		Sources: brainStatusSources{Sessions: true, Semantic: true},
		Brain:   brainStatusBrain{GeneratedAt: "2026-06-10T09:00:00Z"},
	}
	manifest := &exportManifest{
		GeneratedAt: time.Now(),
		Sources: &brainSources{
			Sessions: &sessionSourceManifest{Sessions: make([]exportSession, 14), CheckpointsScanned: 30},
			Semantic: &semanticSourceManifest{Symbols: 55, Relations: 165, Files: 8},
		},
	}
	home = dashHomeView(built, manifest, "main")
	for _, w := range home.Warnings {
		if w == "brain not built yet — run `"+base.BrainInvocation(os.LookupEnv, "refresh")+"`" {
			t.Errorf("built brain should not warn not-built")
		}
	}
	var semDetail string
	for _, s := range home.Sources {
		if s.Name == "Code" {
			semDetail = s.Detail
		}
	}
	if semDetail == "" || semDetail[:2] != "55" {
		t.Errorf("semantic source detail = %q, want it to start with the symbol count", semDetail)
	}
}

func TestDashLiveSummary(t *testing.T) {
	if got := dashLiveSummary(brainLiveState{Dirty: false}); got != "clean" {
		t.Errorf("clean tree summary = %q", got)
	}
	got := dashLiveSummary(brainLiveState{Dirty: true, Staged: []string{"a"}, Untracked: []string{"b", "c"}})
	if got != "1 staged · 2 untracked" {
		t.Errorf("dirty summary = %q, want \"1 staged · 2 untracked\"", got)
	}
}
