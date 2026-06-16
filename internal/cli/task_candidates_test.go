package cli

import (
	"context"
	"strings"
	"testing"
	"time"
)

func taskEp(id, sig, intent, reinf string, cmds ...string) episodeRecord {
	return episodeRecord{
		ID: id, RepoKey: "gh/acme/cli", IntentSignature: sig, Intent: intent,
		Reinforcement: reinf, CommandSequence: cmds,
		Source: episodeAnchor{Path: "sessions/main/x.jsonl", Line: 1},
	}
}

func findTask(tasks []taskCandidate, sig string) *taskCandidate {
	for i := range tasks {
		if tasks[i].IntentSignature == sig {
			return &tasks[i]
		}
	}
	return nil
}

func TestBuildTaskCandidates(t *testing.T) {
	var eps []episodeRecord
	// A real recurring task with command activity.
	for i := 0; i < 4; i++ {
		eps = append(eps, taskEp("cp"+string(rune('a'+i)), "commit:push", "commit and push the change", reinforcementSuccess, "git add", "git commit", "git push"))
	}
	// A conversational continuation cluster — should be dropped by nonTaskSignature.
	for i := 0; i < 4; i++ {
		eps = append(eps, taskEp("y"+string(rune('a'+i)), "yes", "Yes", reinforcementNeutral, "cd"))
	}
	// A cluster with enough episodes but no command activity — dropped.
	for i := 0; i < 4; i++ {
		eps = append(eps, taskEp("q"+string(rune('a'+i)), "two:questions", "Two questions about X", reinforcementNeutral))
	}

	tasks := buildTaskCandidates(eps)

	cp := findTask(tasks, "commit:push")
	if cp == nil {
		t.Fatalf("expected commit:push task; got %d: %+v", len(tasks), tasks)
	}
	if cp.Support != 4 || cp.WithCommands != 4 || cp.Reinforcement.Success != 4 {
		t.Errorf("commit:push stats wrong: %+v", cp)
	}
	if len(cp.Commands) == 0 || cp.Commands[0] == "" {
		t.Errorf("commit:push commands empty")
	}
	if findTask(tasks, "yes") != nil {
		t.Error("conversational 'yes' cluster should be dropped")
	}
	if findTask(tasks, "two:questions") != nil {
		t.Error("no-command cluster should be dropped")
	}
}

func TestParseSynthesisOutput(t *testing.T) {
	skill := "---\nname: do-the-thing\ndescription: Use when ...\n---\n# Do the thing\nsteps"
	r := parseSynthesisOutput(skill)
	if !r.IsSkill || r.Name != "do-the-thing" {
		t.Errorf("skill parse wrong: %+v", r)
	}
	// fenced output is unwrapped.
	r = parseSynthesisOutput("```markdown\n" + skill + "\n```")
	if !r.IsSkill || !strings.HasPrefix(r.SkillText, "---") || r.Name != "do-the-thing" {
		t.Errorf("fenced parse wrong: %+v", r)
	}
	// rejection is recognized with its reason.
	r = parseSynthesisOutput("NOT_A_SKILL: generic git usage, nothing repo-specific")
	if r.IsSkill || r.Reason != "generic git usage, nothing repo-specific" {
		t.Errorf("rejection parse wrong: %+v", r)
	}
}

func TestSynthesizeSkillUsesEvidenceAndRunner(t *testing.T) {
	brainDir := t.TempDir()
	cand := taskCandidate{
		ID: "task:x", RepoKey: "gh/acme/cli", IntentSignature: "commit:push",
		Label: "commit and push", Support: 5, WithCommands: 5,
		Reinforcement: reinforcementCounts{Success: 5},
		Commands:      []string{"git add", "git commit", "git push"},
		SampleIntents: []string{"commit and push the change"},
	}
	var gotInput string
	stub := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		gotInput = string(input)
		return "---\nname: ship-change\ndescription: x\n---\nbody", nil
	}
	res, err := synthesizeSkill(context.Background(), "/repo", brainDir, cand, "codex", "", "", stub)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsSkill || res.Name != "ship-change" {
		t.Errorf("result wrong: %+v", res)
	}
	// The evidence bundle must carry the recurring intent and the real commands.
	for _, want := range []string{"commit:push", "commit and push the change", "git commit", "git push"} {
		if !strings.Contains(gotInput, want) {
			t.Errorf("evidence missing %q\n%s", want, gotInput)
		}
	}
}
