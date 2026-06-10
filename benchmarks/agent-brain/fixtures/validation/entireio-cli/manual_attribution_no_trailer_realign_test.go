package strategy

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"
)

func TestPostCommitNoTrailerRealignsAttributionBaseHidden(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	repo, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatal(err)
	}

	s := &ManualCommitStrategy{}
	sessionID := "test-postcommit-no-trailer-realign-hidden"
	setupSessionWithCheckpoint(t, s, repo, dir, sessionID)

	state, err := s.loadSessionState(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	state.Phase = session.PhaseActive
	state.AttributionBaseCommit = state.BaseCommit
	state.DivergenceNoticeShown = true
	if err := s.saveSessionState(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	originalBase := state.BaseCommit

	testFile := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(testFile, []byte("no trailer commit"), 0o644); err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add("test.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Commit("commit without trailer", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	}); err != nil {
		t.Fatal(err)
	}

	head, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	newHead := head.Hash().String()
	if newHead == originalBase {
		t.Fatalf("HEAD should have changed from %s", originalBase)
	}

	if err := s.PostCommit(context.Background()); err != nil {
		t.Fatal(err)
	}

	reloaded, err := s.loadSessionState(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.BaseCommit != newHead {
		t.Fatalf("BaseCommit = %s, want %s", reloaded.BaseCommit, newHead)
	}
	if reloaded.AttributionBaseCommit != newHead {
		t.Fatalf("AttributionBaseCommit = %s, want %s", reloaded.AttributionBaseCommit, newHead)
	}
	if reloaded.DivergenceNoticeShown {
		t.Fatal("DivergenceNoticeShown should be cleared after realigning attribution base")
	}
	if reloaded.Phase != session.PhaseActive {
		t.Fatalf("Phase = %s, want %s", reloaded.Phase, session.PhaseActive)
	}
}
