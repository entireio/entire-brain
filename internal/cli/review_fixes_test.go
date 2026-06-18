package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// ---------- Priority 3: only one skill-creation path ----------

func TestPatternsCommandHasNoFormSubcommand(t *testing.T) {
	cmd := newPatternsCommand(Options{})
	names := map[string]bool{}
	for _, c := range cmd.Commands() {
		names[c.Name()] = true
	}
	if names["form"] {
		t.Error("`patterns form` must be removed — skills come only from `patterns skills form`")
	}
	if !names["skills"] {
		t.Error("`patterns skills` must exist")
	}
	// The skill-creation path lives under `patterns skills form`.
	var skills *cobra.Command
	for _, c := range cmd.Commands() {
		if c.Name() == "skills" {
			skills = c
		}
	}
	if skills == nil {
		t.Fatal("skills command missing")
	}
	hasForm := false
	for _, c := range skills.Commands() {
		if c.Name() == "form" {
			hasForm = true
		}
	}
	if !hasForm {
		t.Error("`patterns skills form` must exist as the sole skill-creation path")
	}
}

// ---------- Priority 4: redaction ----------

func TestRedactText(t *testing.T) {
	cases := []struct{ in, mustNotContain, mustContain string }{
		{"export GITHUB_TOKEN=ghp_abcdefghijklmnopqrstuvwxyz0123456789", "ghp_abcdefghijklmnopqrstuvwxyz0123456789", "[REDACTED"},
		{"Authorization: Bearer abcdef1234567890XYZ", "abcdef1234567890XYZ", "[REDACTED]"},
		{"token eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.abcDEF123_-x", "eyJhbGciOiJIUzI1NiJ9", "[REDACTED JWT]"},
		{"API_KEY=supersecretvalue123", "supersecretvalue123", "[REDACTED]"},
		{"ran in /Users/alice/Projects/secret/app", "/Users/alice", "/Users/[redacted]"},
		// macOS is case-insensitive by default (and may be case-sensitive); a
		// lowercase /users/ home path is the same dir and must redact too.
		{"ls /users/peytonmontei/Documents/entire", "peytonmontei", "/users/[redacted]"},
		// Linux home layout.
		{"cat /home/bob/.ssh/config", "/home/bob", "/home/[redacted]"},
		// Quoted home path keeps the surrounding quote (and the subpath shape).
		{`"/Users/carol"`, "carol", `"/Users/[redacted]"`},
		// Path shape is preserved (only the username segment goes).
		{"/Users/dave/go/src", "dave", "/Users/[redacted]/go/src"},
		// Real cases from a live brain: canonical /Users (capital) is a home dir
		// wherever it appears — inside file:// URLs, gitbash /c/Users, sed args.
		{"git clone file:///Users/dvydra/src/cli x", "dvydra", "file:///Users/[redacted]"},
		{"grep go-git /c/Users/Victor/cli/go.mod", "Victor", "/c/Users/[redacted]"},
		{`sed 's|x|/Users/peytonmontei/Documents/cli|'`, "peytonmontei", "/Users/[redacted]"},
		{`C:\Users\Victor\cli`, "Victor", `C:\Users\[redacted]`},
		// API/repo paths use lowercase /users/ embedded after a segment -> kept.
		{"gh api users/octocat --jq .name", "", "octocat"},
		{"gh api repos/x/contents/platform/users/components/Grid.tsx", "", "platform/users/components"},
		{"rg users/me/checkpoints api/src", "", "users/me/checkpoints"},
		{"-----BEGIN RSA PRIVATE KEY-----\nMIIabc\n-----END RSA PRIVATE KEY-----", "MIIabc", "[REDACTED PRIVATE KEY]"},
	}
	for _, c := range cases {
		got := redactText(c.in)
		if c.mustNotContain != "" && strings.Contains(got, c.mustNotContain) {
			t.Errorf("redactText(%q) still contains secret %q -> %q", c.in, c.mustNotContain, got)
		}
		if !strings.Contains(got, c.mustContain) {
			t.Errorf("redactText(%q) = %q, want it to contain %q", c.in, got, c.mustContain)
		}
	}
}

func TestSynthesisEvidenceRedactsSecrets(t *testing.T) {
	brainDir := t.TempDir()
	// A transcript excerpt with a secret in a command + its output.
	tp := "sessions/main/s.jsonl"
	full := filepath.Join(brainDir, filepath.FromSlash(tp))
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatal(err)
	}
	transcript := `{"type":"event_msg","payload":{"type":"user_message","message":"deploy it"}}
{"type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"curl -H 'Authorization: Bearer ghp_SECRETTOKEN0123456789abcdef'\"}"}}`
	if err := os.WriteFile(full, []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}
	cand := taskCandidate{
		ID: "task:x", RepoKey: "gh/acme/cli", IntentSignature: "deploy:it", Label: "deploy it",
		Support: 3, WithCommands: 3,
		MatchingFacts: []string{"The deploy token is stored at AWS_SECRET_ACCESS_KEY=AKIAEXAMPLEdeadbeef00"},
		Examples:      []episodeAnchor{{Path: tp, Line: 1}},
	}
	evidence := buildSkillEvidence(brainDir, cand)
	for _, secret := range []string{"ghp_SECRETTOKEN0123456789abcdef", "AKIAEXAMPLEdeadbeef00"} {
		if strings.Contains(evidence, secret) {
			t.Errorf("synthesis evidence leaked secret %q:\n%s", secret, evidence)
		}
	}
}

// ---------- Priority 6 + 7: preview/write contract via stubbed agent ----------

const stubSkillText = "---\nname: ship-it\ndescription: Use when shipping.\n---\n# Ship it\nstep"

func stubRunner(out string) distillAgentRunner {
	return func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		return out, nil
	}
}

func formCmd() (*cobra.Command, *bytes.Buffer) {
	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)
	cmd.SetErr(&bytes.Buffer{})
	return cmd, &buf
}

func sampleCand() taskCandidate {
	return taskCandidate{
		ID: "task:x", RepoKey: "gh/acme/cli", IntentSignature: "ship:it", Label: "ship the change",
		Support: 5, WithCommands: 5, Reinforcement: reinforcementCounts{Success: 5},
		Commands:   []string{"git commit", "git push"},
		Procedures: []taskProcedure{{Commands: []string{"git commit", "git push"}, Count: 5, Specificity: 0.4}},
	}
}

func TestFormPreviewWritesNothing(t *testing.T) {
	store := t.TempDir()
	repo := t.TempDir()
	cmd, out := formCmd()
	s := skillFormOptions{target: "standard", scope: "repo"} // no --yes
	if err := synthesizeAndForm(context.Background(), cmd, sampleCand(), nil, nil, store, repo, "codex", stubRunner(stubSkillText), s, time.Now()); err != nil {
		t.Fatal(err)
	}
	// Preview shows evidence + draft + would-write, but writes nothing.
	if !strings.Contains(out.String(), "Candidate:") || !strings.Contains(out.String(), "would write to:") {
		t.Errorf("preview missing evidence/would-write:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(repo, ".agents", "skills", "ship-it", "SKILL.md")); !os.IsNotExist(err) {
		t.Error("preview must not write skill files")
	}
	if recs, _ := loadBrainSkillMemory(store); len(recs) != 0 {
		t.Error("preview must not record skill memory")
	}
}

func TestFormDraftOnlyPrintsOnlyDraft(t *testing.T) {
	cmd, out := formCmd()
	s := skillFormOptions{target: "standard", scope: "repo", draftOnly: true}
	if err := synthesizeAndForm(context.Background(), cmd, sampleCand(), nil, nil, t.TempDir(), t.TempDir(), "codex", stubRunner(stubSkillText), s, time.Now()); err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(out.String())
	if got != stubSkillText {
		t.Errorf("draft-only should print only the draft, got:\n%s", got)
	}
}

func TestFormYesWritesAndRecords(t *testing.T) {
	store := t.TempDir()
	repo := t.TempDir()
	cmd, _ := formCmd()
	s := skillFormOptions{target: "standard", scope: "repo", yes: true}
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	if err := synthesizeAndForm(context.Background(), cmd, sampleCand(), nil, nil, store, repo, "codex", stubRunner(stubSkillText), s, now); err != nil {
		t.Fatal(err)
	}
	skillPath := filepath.Join(repo, ".agents", "skills", "ship-it", "SKILL.md")
	if _, err := os.Stat(skillPath); err != nil {
		t.Fatalf("--yes should write the skill file: %v", err)
	}
	recs, _ := loadBrainSkillMemory(store)
	if len(recs) != 1 || recs[0].PatternID != "task:x" || recs[0].Status != skillStatusActive {
		t.Fatalf("--yes should record skill memory: %+v", recs)
	}
	// Existing file requires --force.
	cmd2, _ := formCmd()
	if err := synthesizeAndForm(context.Background(), cmd2, sampleCand(), nil, nil, store, repo, "codex", stubRunner(stubSkillText), s, now); err == nil {
		t.Error("re-form without --force should refuse to overwrite")
	}
	cmd3, _ := formCmd()
	s.force = true
	if err := synthesizeAndForm(context.Background(), cmd3, sampleCand(), nil, nil, store, repo, "codex", stubRunner(stubSkillText), s, now); err != nil {
		t.Errorf("--force should overwrite: %v", err)
	}
}

func TestFormRejectsNotASkill(t *testing.T) {
	store := t.TempDir()
	repo := t.TempDir()
	cmd, out := formCmd()
	s := skillFormOptions{target: "standard", scope: "repo", yes: true}
	if err := synthesizeAndForm(context.Background(), cmd, sampleCand(), nil, nil, store, repo, "codex", stubRunner("NOT_A_SKILL: generic git usage"), s, time.Now()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "not a skill") {
		t.Errorf("expected not-a-skill message, got: %s", out.String())
	}
	if recs, _ := loadBrainSkillMemory(store); len(recs) != 0 {
		t.Error("NOT_A_SKILL must not record skill memory or write files")
	}
}

func TestFormPreviewRedactsEvidence(t *testing.T) {
	cand := sampleCand()
	cand.MatchingFacts = []string{"deploy with TOKEN=ghp_SECRETTOKEN0123456789abcdef"}
	cmd, out := formCmd()
	s := skillFormOptions{target: "standard", scope: "repo"}
	if err := synthesizeAndForm(context.Background(), cmd, cand, nil, nil, t.TempDir(), t.TempDir(), "codex", stubRunner(stubSkillText), s, time.Now()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "ghp_SECRETTOKEN0123456789abcdef") {
		t.Errorf("preview leaked a secret from evidence:\n%s", out.String())
	}
}

// ---------- Priority 8: compact candidate eval ----------

func evalEp(id, sig, intent string, cmds ...string) episodeRecord {
	return episodeRecord{ID: id, RepoKey: "gh/acme/cli", IntentSignature: sig, Intent: intent,
		Reinforcement: reinforcementNeutral, CommandSequence: cmds, Source: episodeAnchor{Path: "s.jsonl", Line: 1}}
}

func TestCandidateEval(t *testing.T) {
	var eps []episodeRecord
	n := 0
	add := func(sig, intent string, cmds ...string) {
		n++
		eps = append(eps, evalEp("e"+string(rune('A'+n%26))+string(rune('a'+n/26)), sig, intent, cmds...))
	}
	// Make git ubiquitous across many unrelated episodes so commit/push has low idf.
	for i := 0; i < 16; i++ {
		add("misc:work", "do misc work", "git add", "git commit", "git push", "sed")
	}
	// (1) Generic commit/push loop — ubiquitous git, no facts -> reject.
	for i := 0; i < 4; i++ {
		add("commit:push", "commit and push", "git add", "git commit", "git push")
	}
	// (2) Repo-specific release/check workflow — rare specific commands -> keep.
	for i := 0; i < 4; i++ {
		add("release:readiness", "verify release readiness", "mise verify-release", "mise tag-release")
	}
	// (3) Conversational cluster -> reject (non-task signature).
	for i := 0; i < 4; i++ {
		add("yes:please", "yes please", "cd")
	}
	// (5) Incoherent same-signature cluster — each episode unrelated one-offs -> reject.
	add("do:thing", "do the thing", "alpha-tool")
	add("do:thing", "do the thing", "beta-tool")
	add("do:thing", "do the thing", "gamma-tool")
	add("do:thing", "do the thing", "delta-tool")

	// (4) Read-only diagnosis practice — generic commands but a matching fact -> keep.
	for i := 0; i < 4; i++ {
		add("inspect:logs", "inspect the session logs", "cat", "grep")
	}
	facts := []factRecord{{ID: "f1", Status: "active", Text: "Session logs for inspect live under sessions/<branch> and are JSONL."}}

	tasks := buildTaskCandidates(eps, facts)
	got := map[string]bool{}
	for _, tk := range tasks {
		got[tk.IntentSignature] = true
	}

	wantKeep := []string{"release:readiness", "inspect:logs"}
	wantReject := []string{"commit:push", "yes:please", "do:thing"}
	for _, s := range wantKeep {
		if !got[s] {
			t.Errorf("expected candidate %q to be kept; tasks=%v", s, got)
		}
	}
	for _, s := range wantReject {
		if got[s] {
			t.Errorf("expected candidate %q to be rejected", s)
		}
	}
}

func TestWorkspaceSkillsListJSONRedacts(t *testing.T) {
	env := EntireEnv{PluginDataDir: t.TempDir()}
	wsDir, err := workspaceDir(env, "platform")
	if err != nil {
		t.Fatal(err)
	}
	cand := taskCandidate{
		ID: "task:ws:x", Workspace: "platform", IntentSignature: "deploy:it", Label: "deploy it",
		Support: 3, Repos: 2,
		MatchingFacts: []string{"token is ghp_SECRETTOKEN0123456789abcdef"},
	}
	if err := writeBrainTasksFile(wsDir, []taskCandidate{cand}); err != nil {
		t.Fatal(err)
	}
	cmd, out := formCmd()
	if err := runWorkspaceSkillsList(context.Background(), cmd, Options{Env: env}, "platform", true, 10); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "ghp_SECRETTOKEN0123456789abcdef") {
		t.Errorf("workspace skills --json leaked a secret:\n%s", out.String())
	}
}

func TestWorkspaceTaskCandidatesCrossRepo(t *testing.T) {
	mk := func(sig string) taskCandidate {
		return taskCandidate{IntentSignature: sig, Label: sig, Support: 3, Reinforcement: reinforcementCounts{Success: 3},
			Commands: []string{"mise verify"}, Procedures: []taskProcedure{{Commands: []string{"mise verify"}, Count: 3, Specificity: 0.5}}}
	}
	byRepo := map[string][]taskCandidate{
		"gh/acme/a": {mk("release:readiness"), mk("only:a")},
		"gh/acme/b": {mk("release:readiness")},
	}
	order := []string{"gh/acme/a", "gh/acme/b"}
	ws := buildWorkspaceTaskCandidates(byRepo, order, 2, "platform")
	if len(ws) != 1 {
		t.Fatalf("want 1 cross-repo task candidate, got %d: %+v", len(ws), ws)
	}
	if ws[0].IntentSignature != "release:readiness" || ws[0].Repos != 2 || ws[0].Workspace != "platform" {
		t.Errorf("workspace candidate wrong: %+v", ws[0])
	}
	if len(ws[0].RepoBreakdown) != 2 {
		t.Errorf("want per-repo breakdown, got %+v", ws[0].RepoBreakdown)
	}
}
