package cli

import (
	"fmt"
	"testing"
)

func procEpisode(id, author, branch, reinf string, commands ...string) episodeRecord {
	return episodeRecord{
		ID:              id,
		RepoKey:         "gh/acme/cli",
		Author:          author,
		Branch:          branch,
		Reinforcement:   reinf,
		CommandSequence: commands,
		Source:          episodeAnchor{Path: "sessions/main/x.jsonl", Line: 1},
	}
}

func findProcedure(procs []procedureRecord, commands ...string) *procedureRecord {
	key := fmt.Sprint(commands)
	for i := range procs {
		if fmt.Sprint(procs[i].Commands) == key {
			return &procs[i]
		}
	}
	return nil
}

func TestBuildBrainProcedures(t *testing.T) {
	var episodes []episodeRecord
	// 5 episodes run a meaningful, specific workflow with a ubiquitous "sed" prefix.
	for i := 0; i < 5; i++ {
		episodes = append(episodes, procEpisode(fmt.Sprintf("a%d", i), "Ada", "main", reinforcementSuccess, "sed", "go test", "git commit"))
	}
	// 5 episodes pair the ubiquitous "sed" with a common "ls".
	for i := 0; i < 5; i++ {
		episodes = append(episodes, procEpisode(fmt.Sprintf("b%d", i), "Ada", "main", reinforcementNeutral, "sed", "ls"))
	}

	procs := buildBrainProcedures(episodes)

	// The specific workflow is detected with full support and reinforcement.
	gw := findProcedure(procs, "go test", "git commit")
	if gw == nil {
		t.Fatalf("expected 'go test → git commit' procedure; got %d procedures: %+v", len(procs), procs)
	}
	if gw.Support != 5 || gw.Reinforcement.Success != 5 {
		t.Errorf("workflow support=%d success=%d, want 5/5", gw.Support, gw.Reinforcement.Success)
	}
	if gw.Type != "procedure" || gw.Scope != "repo" || gw.RepoKey != "gh/acme/cli" {
		t.Errorf("identity wrong: %+v", gw)
	}

	// "sed" is ubiquitous (every episode) so idf ~ 0: any n-gram dominated by it
	// must be suppressed below the specificity floor and not surface.
	if p := findProcedure(procs, "sed", "go test"); p != nil {
		t.Errorf("ubiquitous-prefixed shape 'sed → go test' should be suppressed, got strength %.3f", p.Strength)
	}
	if p := findProcedure(procs, "sed", "ls"); p != nil {
		t.Errorf("ubiquitous shape 'sed → ls' should be suppressed, got strength %.3f", p.Strength)
	}
}

func TestProcedureMinSupport(t *testing.T) {
	// A shape in only 2 episodes is below procedureMinSupport (3).
	episodes := []episodeRecord{
		procEpisode("a", "Ada", "main", reinforcementSuccess, "terraform plan", "terraform apply"),
		procEpisode("b", "Ada", "main", reinforcementSuccess, "terraform plan", "terraform apply"),
	}
	if procs := buildBrainProcedures(episodes); len(procs) != 0 {
		t.Errorf("expected no procedures below min support, got %+v", procs)
	}
}

func TestProcedureReinforcementAndSorting(t *testing.T) {
	var episodes []episodeRecord
	// A corrected-heavy specific shape vs a success-heavy specific shape, equal support.
	for i := 0; i < 4; i++ {
		episodes = append(episodes, procEpisode(fmt.Sprintf("good%d", i), "Ada", "main", reinforcementSuccess, "go build", "go test"))
		episodes = append(episodes, procEpisode(fmt.Sprintf("bad%d", i), "Ada", "main", reinforcementCorrected, "cargo build", "cargo test"))
	}
	procs := buildBrainProcedures(episodes)
	good := findProcedure(procs, "go build", "go test")
	bad := findProcedure(procs, "cargo build", "cargo test")
	if good == nil || bad == nil {
		t.Fatalf("expected both procedures; got %+v", procs)
	}
	// Reinforcement is the only differing term, so success-heavy must score higher
	// and sort first.
	if good.Strength <= bad.Strength {
		t.Errorf("success-heavy strength %.3f should exceed corrected-heavy %.3f", good.Strength, bad.Strength)
	}
	if procs[0].Strength < procs[len(procs)-1].Strength {
		t.Error("procedures not sorted by descending strength")
	}
}

func TestProcedureIDStable(t *testing.T) {
	a := procedureID("repo", "gh/acme/cli", []string{"go test", "git commit"})
	b := procedureID("repo", "gh/acme/cli", []string{"go test", "git commit"})
	c := procedureID("repo", "gh/acme/cli", []string{"git commit", "go test"})
	if a != b {
		t.Error("same inputs produced different ids")
	}
	if a == c {
		t.Error("different command order should produce different ids")
	}
}
