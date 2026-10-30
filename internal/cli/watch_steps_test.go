package cli

import (
	"context"
	"testing"

	"github.com/entireio/entire-brain/internal/cli/command/watch"
)

func TestWatchDistillOptionsDefaultAndExplicitJobs(t *testing.T) {
	// concurrency, not a separate `jobs` field: --jobs is now a spelling of
	// --concurrency, so there is one value for a caller to get wrong instead of
	// two to keep in step.
	defaultOpts := watchDistillOptions(watch.DefaultFlags())
	if defaultOpts.concurrency != 1 {
		t.Fatalf("default watch distill concurrency = %d, want 1", defaultOpts.concurrency)
	}
	explicit := watch.DefaultFlags()
	explicit.DistillJobs = 4
	explicit.DistillAgent = "ollama"
	explicit.Model = "local-model"
	got := watchDistillOptions(explicit)
	if got.concurrency != 4 || got.agent != "ollama" || got.model != "local-model" {
		t.Fatalf("watch distill options not propagated: %+v", got)
	}
}

func TestWatchFingerprintUsesRefs(t *testing.T) {
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "rev-parse", "--verify", "--quiet", v1MainRef):   {stdout: "ckSHA\n"},
		fakeCommandKey("git", "rev-parse", "--verify", "--quiet", v1OriginRef): {stdout: "originSHA\n"},
		fakeCommandKey("git", "rev-parse", "--verify", "--quiet", "HEAD"):      {stdout: "headSHA\n"},
	}}
	if got := watchFingerprint(context.Background(), runner, "/repo"); got != "ckSHA:originSHA:headSHA" {
		t.Fatalf("fingerprint = %q, want ckSHA:originSHA:headSHA (incl. fetched origin checkpoint ref)", got)
	}
}
