package cli

import (
	"context"
	"testing"
	"time"
)

func TestWorkspaceFamilyWithdrawnAfterMemberRemoval(t *testing.T) {
	env, m := twoRepoWorkspace(t, "gh/acme/a", "release:build", []string{"mise build", "mise deploy"}, "gh/acme/b", "release:ship", []string{"make build", "make release"})
	if _, err := proposeWorkspaceFamilies(context.Background(), env, m, t.TempDir(), "codex", "", "", releaseGroupingAgent(t), time.Now()); err != nil {
		t.Fatal(err)
	}
	m.Repos = m.Repos[:1]
	if _, err := buildWorkspacePatternCorpus(env, m, time.Now()); err != nil {
		t.Fatal(err)
	}
	stats, err := proposeWorkspaceFamilies(context.Background(), env, m, t.TempDir(), "codex", "", "", releaseGroupingAgent(t), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	d, _ := workspaceDir(env, m.Name)
	f := loadAcceptedWorkspaceFamilies(d)
	if len(f) != 0 {
		t.Fatalf("stale two-member family still accepted: %+v", f)
	}
	t.Logf("current members=%d accepted families=%d verify=%+v", len(m.Repos), len(f), stats)
}

func TestWorkspaceFamilyConsumptionChecksCurrentMembership(t *testing.T) {
	env, m := twoRepoWorkspace(t, "gh/acme/a", "release:build", []string{"mise build", "mise deploy"}, "gh/acme/b", "release:ship", []string{"make build", "make release"})
	if _, err := proposeWorkspaceFamilies(context.Background(), env, m, t.TempDir(), "codex", "", "", releaseGroupingAgent(t), time.Now()); err != nil {
		t.Fatal(err)
	}
	dir, _ := workspaceDir(env, m.Name)
	families, err := loadAcceptedWorkspaceFamiliesChecked(dir)
	if err != nil || len(families) != 1 {
		t.Fatalf("valid family: %v %v", families, err)
	}
	m.Repos = m.Repos[:1]
	if err := writeWorkspaceManifest(env, m); err != nil {
		t.Fatal(err)
	}
	families, err = loadAcceptedWorkspaceFamiliesChecked(dir)
	if err != nil || len(families) != 0 {
		t.Fatalf("stale family consumed without refresh: %v %v", families, err)
	}
}
