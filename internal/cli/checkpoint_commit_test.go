package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/entities"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
)

const (
	ckptCommitMain   = "aaaa111111111111111111111111111111111111"
	ckptCommitCopy   = "bbbb222222222222222222222222222222222222"
	ckptCommitOther  = "cccc333333333333333333333333333333333333"
	ckptIDRebased    = "abcdef123456"
	ckptIDSingle     = "0123456789ab"
	ckptLogSeparator = gitLogFieldSeparator
)

func checkpointCommitLogFixture() []byte {
	records := []string{
		// The original commit on main.
		strings.Join([]string{ckptCommitMain, "2026-07-01T10:00:00Z", "first\n\nEntire-Checkpoint: " + ckptIDRebased + "\n"}, ckptLogSeparator),
		// A rebase copy on another branch, NEWER, carrying the same trailer.
		strings.Join([]string{ckptCommitCopy, "2026-07-02T10:00:00Z", "first (rebased)\n\nEntire-Checkpoint: " + ckptIDRebased + "\n"}, ckptLogSeparator),
		strings.Join([]string{ckptCommitOther, "2026-07-03T10:00:00Z", "second\n\nEntire-Checkpoint: " + ckptIDSingle + "\n"}, ckptLogSeparator),
		// No trailer: never indexed.
		strings.Join([]string{"dddd444444444444444444444444444444444444", "2026-07-04T10:00:00Z", "no checkpoint here\n"}, ckptLogSeparator),
	}
	return []byte(strings.Join(records, gitLogRecordSeparator))
}

func fixtureCheckpointResolver(reachable entities.BranchReachability) *checkpointCommitResolver {
	return &checkpointCommitResolver{
		byCheckpoint: checkpointCommitsFromGitLog(checkpointCommitLogFixture()),
		reachable:    reachable,
	}
}

func TestCheckpointCommitsFromGitLog(t *testing.T) {
	byCheckpoint := checkpointCommitsFromGitLog(checkpointCommitLogFixture())
	if len(byCheckpoint) != 2 {
		t.Fatalf("checkpoints indexed = %d, want 2 (%v)", len(byCheckpoint), byCheckpoint)
	}
	if got := len(byCheckpoint[ckptIDRebased]); got != 2 {
		t.Fatalf("candidates for rebased checkpoint = %d, want both copies", got)
	}
	if got := byCheckpoint[ckptIDSingle]; len(got) != 1 || got[0].commit != ckptCommitOther {
		t.Fatalf("single checkpoint candidates = %+v", got)
	}
}

// The newest commit carrying a checkpoint's trailer is routinely a rebase copy
// on ANOTHER branch; a known reachability set must refuse it and keep the
// on-branch original.
func TestCommitForPrefersOnBranchOverNewerCopy(t *testing.T) {
	onMain := map[string]struct{}{ckptCommitMain: {}}
	resolver := fixtureCheckpointResolver(func(branch string) map[string]struct{} {
		if branch == "main" {
			return onMain
		}
		return map[string]struct{}{}
	})
	if got := resolver.commitFor(ckptIDRebased, "main"); got != ckptCommitMain {
		t.Fatalf("commitFor on main = %q, want the on-branch original %q", got, ckptCommitMain)
	}
	// A branch where NO candidate is reachable resolves blank: absent is
	// honest, wrong is not.
	if got := resolver.commitFor(ckptIDRebased, "release"); got != "" {
		t.Fatalf("commitFor off-branch = %q, want blank", got)
	}
}

// Unknown reachability (nil set) is not a refusal: the newest candidate wins,
// matching entities_provenance's discipline.
func TestCommitForUnknownReachabilityPicksNewest(t *testing.T) {
	resolver := fixtureCheckpointResolver(func(string) map[string]struct{} { return nil })
	if got := resolver.commitFor(ckptIDRebased, "main"); got != ckptCommitCopy {
		t.Fatalf("commitFor = %q, want newest candidate %q", got, ckptCommitCopy)
	}
}

// TestDistillStampsAnchorCommit pins the wiring: a distilled fact's anchor
// carries the commit its checkpoint resolves to, and a nil resolver (index
// unavailable) leaves it blank — the pre-COR-2126 behavior.
func TestDistillStampsAnchorCommit(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	fakeRun := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		return "preferences.coding.style\tThe user prefers concise commits.\n", nil
	}
	for name, resolver := range map[string]*checkpointCommitResolver{
		"resolved": {byCheckpoint: map[string][]checkpointCommitCandidate{"cp1": {{commit: ckptCommitMain}}}},
		"absent":   nil,
	} {
		brainDir := writeDistillFixture(t, now)
		opts := distillCommandOptions{agent: "command", agentCommand: []string{"fake"}, run: fakeRun, maxChunkBytes: defaultDistillChunkSize, timeout: time.Minute, checkpointCommits: resolver}
		if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now); err != nil {
			t.Fatalf("%s: runDistillForBrain: %v", name, err)
		}
		facts, err := factstore.Load(brainDir, "main")
		if err != nil || len(facts) != 1 || len(facts[0].Provenance) != 1 {
			t.Fatalf("%s: facts = %+v (%v)", name, facts, err)
		}
		want := ""
		if resolver != nil {
			want = ckptCommitMain
		}
		if got := facts[0].Provenance[0].Commit; got != want {
			t.Fatalf("%s: anchor commit = %q, want %q", name, got, want)
		}
	}
}

func TestCommitForBlankCases(t *testing.T) {
	resolver := fixtureCheckpointResolver(nil)
	if got := resolver.commitFor("ffffffffffff", "main"); got != "" {
		t.Fatalf("unknown checkpoint = %q, want blank", got)
	}
	var nilResolver *checkpointCommitResolver
	if got := nilResolver.commitFor(ckptIDSingle, "main"); got != "" {
		t.Fatalf("nil resolver = %q, want blank", got)
	}
}
