package entityindex

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// hangingRunner blocks until its context is canceled, standing in for a
// wedged `entire graph diff` process. Unlike fakeRunner (which ignores ctx
// entirely, per its signature `Run(_ context.Context, ...)`), this respects
// cancellation so a test can prove DiffCommit enforces its OWN deadline
// rather than merely inheriting whatever the caller happened to pass in.
type hangingRunner struct{}

func (hangingRunner) Run(ctx context.Context, _ string, _ string, _ ...string) ([]byte, []byte, error) {
	<-ctx.Done()
	return nil, nil, ctx.Err()
}

// TestDiffCommitTimesOutOnAHungProvider pins the defect this test was written
// to catch: DiffCommit shells out to the graph provider with no bound of its
// own, so a wedged provider process hangs the entire entity-index build
// forever. A caller passing context.Background() (as the real build loop's
// root context effectively does — nothing upstream sets a deadline; see
// build.go's indexCommit) must still get a bounded, labeled, distinguishable
// timeout error back — well before this test's own 2s patience limit, and
// well below graphDiffTimeout's real-world default.
func TestDiffCommitTimesOutOnAHungProvider(t *testing.T) {
	// Not t.Parallel(): this test mutates the package-level graphDiffTimeout,
	// which would race with other parallel tests in this package that also
	// call DiffCommit (e.g. TestDiffCommitNamesTheRepositoryExplicitly).
	orig := graphDiffTimeout
	graphDiffTimeout = 50 * time.Millisecond
	defer func() { graphDiffTimeout = orig }()

	done := make(chan error, 1)
	go func() {
		_, err := DiffCommit(context.Background(), hangingRunner{}, testRepoDir, "entire", "base", "head", time.Now())
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("DiffCommit returned nil error for a hung provider; want a bounded timeout error")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("DiffCommit error does not wrap context.DeadlineExceeded, so callers cannot distinguish a timeout from an ordinary provider failure: %v", err)
		}
		if !strings.Contains(err.Error(), "base") || !strings.Contains(err.Error(), "head") {
			t.Fatalf("DiffCommit timeout error does not name the commit range: %v", err)
		}
		if !strings.Contains(err.Error(), graphDiffTimeout.String()) {
			t.Fatalf("DiffCommit timeout error does not name the configured duration: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("DiffCommit did not return within 2s of its injected %s deadline; it hung indefinitely on a wedged provider", graphDiffTimeout)
	}
}
