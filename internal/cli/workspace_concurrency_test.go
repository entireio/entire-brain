package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// workspace_concurrency_test.go covers the other race `setup` creates: running
// it in two repos at once is the ordinary case on a developer machine, and an
// unlocked read-modify-write of one shared manifest silently drops one of the
// two registrations.

// TestConcurrentWorkspaceAddsAllPersist is the lost-registration guard: N repos
// registering into one workspace at the same time must all end up members.
func TestConcurrentWorkspaceAddsAllPersist(t *testing.T) {
	f := newSetupTestFixture(t)
	const repos = 8

	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, repos)
	for i := 0; i < repos; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := addWorkspaceRepoLocked(f.env, setupDefaultWorkspace, workspaceRepo{
				RepoKey:       fmt.Sprintf("local/repo-%02d", i),
				LocalPathHint: fmt.Sprintf("/tmp/repo-%02d", i),
			})
			errs <- err
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent add: %v", err)
		}
	}

	manifest, err := loadWorkspaceManifest(f.env, setupDefaultWorkspace)
	if err != nil {
		t.Fatalf("load workspace: %v", err)
	}
	if len(manifest.Repos) != repos {
		t.Fatalf("every concurrent registration must persist: got %d of %d\n%+v",
			len(manifest.Repos), repos, manifest.Repos)
	}
	seen := map[string]bool{}
	for _, repo := range manifest.Repos {
		if seen[repo.RepoKey] {
			t.Fatalf("duplicate member %s", repo.RepoKey)
		}
		seen[repo.RepoKey] = true
	}
}

// TestConcurrentSetupRegistrationsAllPersist is the same property through the
// path setup actually takes, with two different repos racing.
func TestConcurrentSetupRegistrationsAllPersist(t *testing.T) {
	f := newSetupTestFixture(t)
	const repos = 6

	start := make(chan struct{})
	var wg sync.WaitGroup
	states := make(chan setupWorkspaceState, repos)
	errs := make(chan error, repos)
	for i := 0; i < repos; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			state, err := registerRepoInWorkspace(context.Background(), nil, f.opts,
				setupDefaultWorkspace, fmt.Sprintf("/tmp/repo-%02d", i), fmt.Sprintf("local/repo-%02d", i))
			states <- state
			errs <- err
		}(i)
	}
	close(start)
	wg.Wait()
	close(states)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent registration: %v", err)
		}
	}
	registered := 0
	for state := range states {
		if state.Registered {
			registered++
		}
	}
	if registered != repos {
		t.Fatalf("every setup must report its repo registered, got %d", registered)
	}

	manifest, err := loadWorkspaceManifest(f.env, setupDefaultWorkspace)
	if err != nil {
		t.Fatalf("load workspace: %v", err)
	}
	if len(manifest.Repos) != repos {
		t.Fatalf("concurrent setups must not lose registrations: got %d of %d\n%+v",
			len(manifest.Repos), repos, manifest.Repos)
	}
}

// TestWorkspaceAddReportsCreatedAndAlready keeps the reporting honest now that
// the decision is made inside the lock rather than by a racy pre-read.
func TestWorkspaceAddReportsCreatedAndAlready(t *testing.T) {
	f := newSetupTestFixture(t)
	repo := workspaceRepo{RepoKey: "local/demo", LocalPathHint: "/tmp/demo"}

	first, err := addWorkspaceRepoLocked(f.env, "fresh", repo)
	if err != nil {
		t.Fatalf("first add: %v", err)
	}
	if !first.Created || first.Already {
		t.Fatalf("the first add creates the workspace: %+v", first)
	}
	second, err := addWorkspaceRepoLocked(f.env, "fresh", repo)
	if err != nil {
		t.Fatalf("second add: %v", err)
	}
	if second.Created || !second.Already {
		t.Fatalf("re-adding the same repo is a no-op membership-wise: %+v", second)
	}
	manifest, err := loadWorkspaceManifest(f.env, "fresh")
	if err != nil {
		t.Fatalf("load workspace: %v", err)
	}
	if len(manifest.Repos) != 1 {
		t.Fatalf("re-adding must not duplicate the member: %+v", manifest.Repos)
	}
	// The lock file lives beside the manifest and must not leak into the store's
	// meaningful contents.
	dir, err := workspaceDir(f.env, "fresh")
	if err != nil {
		t.Fatalf("workspace dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, brainLockDirName, workspaceManifestLockName)); err != nil {
		t.Fatalf("the manifest lock must live beside the manifest: %v", err)
	}
}
