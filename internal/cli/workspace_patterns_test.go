package cli

import "testing"

func TestBuildWorkspaceProcedures(t *testing.T) {
	byRepo := map[string][]procedureRecord{
		"gh/acme/a": {
			{Commands: []string{"go test", "git commit"}, Support: 5, Reinforcement: reinforcementCounts{Success: 5}, Examples: []episodeAnchor{{Path: "a.jsonl", Line: 1}}},
			{Commands: []string{"only", "here"}, Support: 3},
		},
		"gh/acme/b": {
			{Commands: []string{"go test", "git commit"}, Support: 4, Reinforcement: reinforcementCounts{Success: 4}},
		},
	}
	order := []string{"gh/acme/a", "gh/acme/b"}

	procs := buildWorkspaceProcedures(byRepo, order, 2, "platform")
	if len(procs) != 1 {
		t.Fatalf("got %d workspace procedures, want 1 (only the cross-repo shape): %+v", len(procs), procs)
	}
	p := procs[0]
	if p.Scope != "workspace" || p.Workspace != "platform" || p.RepoKey != "" {
		t.Errorf("scope fields wrong: %+v", p)
	}
	if p.Repos != 2 || p.Support != 9 || p.Reinforcement.Success != 9 {
		t.Errorf("merged stats wrong: repos=%d support=%d success=%d", p.Repos, p.Support, p.Reinforcement.Success)
	}
	if len(p.RepoBreakdown) != 2 || p.RepoBreakdown[0].RepoKey != "gh/acme/a" || p.RepoBreakdown[0].Support != 5 {
		t.Errorf("breakdown wrong: %+v", p.RepoBreakdown)
	}
	if p.ID != procedureID("workspace", "platform", []string{"go test", "git commit"}) {
		t.Errorf("id wrong: %s", p.ID)
	}
}

func TestBuildWorkspacePractices(t *testing.T) {
	byRepo := map[string][]practiceRecord{
		"gh/acme/a": {
			{ID: "pattern:practice:fact:x", Kind: factKindInvariant, Statement: "Token must never be logged.", Support: 2},
		},
		"gh/acme/b": {
			{ID: "pattern:practice:fact:x", Kind: factKindInvariant, Statement: "Token must never be logged.", Support: 1},
			{ID: "pattern:practice:fact:y", Kind: factKindDecision, Statement: "Repo-only fact.", Support: 1},
		},
	}
	order := []string{"gh/acme/a", "gh/acme/b"}

	pracs := buildWorkspacePractices(byRepo, order, 2, "platform")
	if len(pracs) != 1 {
		t.Fatalf("got %d workspace practices, want 1 (cross-repo only): %+v", len(pracs), pracs)
	}
	p := pracs[0]
	if p.ID != "pattern:practice:fact:x" || p.Repos != 2 || p.Support != 3 {
		t.Errorf("merge wrong: id=%s repos=%d support=%d", p.ID, p.Repos, p.Support)
	}
	if p.Scope != "workspace" || p.Workspace != "platform" {
		t.Errorf("scope fields wrong: %+v", p)
	}
}

func TestRepoBreadthScore(t *testing.T) {
	if s := repoBreadthScore(3, 3); s != 1.0 {
		t.Errorf("all members = %v, want 1.0", s)
	}
	if s := repoBreadthScore(2, 5); s >= 1.0 || s <= 0 {
		t.Errorf("2 of 5 = %v, want in (0,1)", s)
	}
	if s := repoBreadthScore(2, 1); s != 1.0 {
		t.Errorf("degenerate single-member denom = %v, want 1.0", s)
	}
}
