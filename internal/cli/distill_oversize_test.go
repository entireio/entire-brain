package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
)

// One transcript over the store's read cap used to refuse the WHOLE distill
// pass. Measured on the real store before this fix: `distill --dry-run --branch
// main` died instantly on a single 256MiB transcript and planned 0 sessions,
// and the workspace watch daemon burned CPU for days with the distilled count
// frozen, because every run died on the same file. After: 207 sessions planned,
// 206 to distill, with the one unreadable session named.
//
// The history readers reached this conclusion first for the identical sentinel
// (#483, and shortTermCanonicalInputRefusal after it). This is distill's copy.
func TestDistillOversizeTranscriptExcusesOnlyItself(t *testing.T) {
	t.Parallel()

	oversize := errHistoryTranscriptTooLarge
	coded := fmt.Errorf("read transcript x: %s: stored transcript exceeds maximum size", brainstore.InputTooLargeCode)

	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"the sentinel":              {oversize, true},
		"wrapped sentinel":          {fmt.Errorf("read canonical transcript x: %w", oversize), true},
		"the store's coded message": {coded, true},
		"nil":                       {nil, false},
		"an ordinary read failure":  {errors.New("permission denied"), false},
		"a containment breach":      {fmt.Errorf("%s: escapes root", brainstore.StateCorruptCode), false},
		"a stale source":            {fmt.Errorf("%s: membership changed", brainstore.SourceStaleCode), false},
		"cancellation":              {context.Canceled, false},
		"a deadline":                {context.DeadlineExceeded, false},
		"a degraded inventory":      {errHistorySessionInventoryDegraded, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := distillOversizeTranscript(tc.err); got != tc.want {
				t.Errorf("distillOversizeTranscript(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// THE BUG THIS PINS, found while writing the test rather than in review: one
// error value can carry several reasons, because readers return
// errors.Join(readErr, finishErr, closeErr). A predicate that asks about the
// oversize sentinel FIRST answers "skip" for a join that also carries a
// cancelled context or a stale source -- which publishes a partial pass on a
// refusal, exactly the hazard "complete-or-refused" existed to prevent.
//
// The sentinel excuses only itself, so every join below must REFUSE.
func TestDistillOversizeJoinedWithARefusalStillRefuses(t *testing.T) {
	t.Parallel()

	oversize := errHistoryTranscriptTooLarge
	for name, other := range map[string]error{
		"cancellation":       context.Canceled,
		"deadline":           context.DeadlineExceeded,
		"degraded inventory": errHistorySessionInventoryDegraded,
		"stale source":       fmt.Errorf("%s: membership changed", brainstore.SourceStaleCode),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// Both orderings: a join is unordered as far as the caller knows.
			for _, joined := range []error{errors.Join(oversize, other), errors.Join(other, oversize)} {
				if distillOversizeTranscript(joined) {
					t.Errorf("an oversize transcript joined with %s must REFUSE, not skip: %v", name, joined)
				}
			}
		})
	}
}

// A skipped session must carry its previous cache entry forward. The final
// flush persists the NEW cache only, so dropping the entry would make the next
// run re-distill every OTHER session from scratch -- turning a tolerated skip
// into a corpus-wide cost.
func TestSkippedSessionCarriesItsCacheEntryForward(t *testing.T) {
	t.Parallel()

	session := exportSession{SessionID: "s1", TranscriptPath: "sessions/main/s1.jsonl"}
	key := distillSessionCacheKeyFor("main", session, nil)

	t.Run("an existing entry is carried under its current key", func(t *testing.T) {
		t.Parallel()
		prev := distillCache{Sessions: map[string]string{key: "fingerprint-1"}}
		next := distillCache{Sessions: map[string]string{}}
		carryPreviousSessionCache(&next, prev, "main", session, nil)
		if next.Sessions[key] != "fingerprint-1" {
			t.Errorf("entry not carried: %v", next.Sessions)
		}
	})

	t.Run("a legacy entry stays under its legacy key", func(t *testing.T) {
		t.Parallel()
		// Copying it under the new key would make it permanently unmatchable.
		prev := distillCache{Sessions: map[string]string{"s1": "legacy-fp"}}
		next := distillCache{Sessions: map[string]string{}}
		carryPreviousSessionCache(&next, prev, "main", session, nil)
		if next.Sessions["s1"] != "legacy-fp" {
			t.Errorf("legacy entry not carried: %v", next.Sessions)
		}
		if _, moved := next.Sessions[key]; moved && key != "s1" {
			t.Errorf("legacy entry was copied under the new key, making it unmatchable: %v", next.Sessions)
		}
	})

	t.Run("a session with no previous entry carries nothing", func(t *testing.T) {
		t.Parallel()
		// It must stay UNCACHED so a later run retries it once the transcript
		// is readable. Inventing an entry would mark it done for ever.
		next := distillCache{Sessions: map[string]string{}}
		carryPreviousSessionCache(&next, distillCache{Sessions: map[string]string{}}, "main", session, nil)
		if len(next.Sessions) != 0 {
			t.Errorf("a never-distilled session must stay uncached, got %v", next.Sessions)
		}
	})
}

// THE CALL SITE, not the predicate. Deleting the tolerance from
// validateDistillCanonicalInputs left every test above passing, because they
// only exercise the pure function -- the same "logic in the call site" gap that
// has hidden defects in this repo before. This reaches the site.
//
// validateDistillCanonicalInputs is the gate that actually aborted the pass:
// it runs before any provider work, so one over-cap transcript meant
// `distill --dry-run` planned nothing and the daemon never progressed.
func TestDistillPreflightSkipsAnOversizeTranscriptAndRefusesTheRest(t *testing.T) {
	sessions := []exportSession{
		{SessionID: "ok-1", TranscriptPath: "sessions/main/ok1.jsonl", Branch: "main"},
		{SessionID: "too-big", TranscriptPath: "sessions/main/big.jsonl", Branch: "main"},
		{SessionID: "ok-2", TranscriptPath: "sessions/main/ok2.jsonl", Branch: "main"},
	}
	branchOf := func(s exportSession) string { return "main" }

	restore := distillReadCanonicalTranscript
	t.Cleanup(func() { distillReadCanonicalTranscript = restore })

	t.Run("an over-cap transcript is skipped, and the others are still read", func(t *testing.T) {
		var read []string
		distillReadCanonicalTranscript = func(_ context.Context, _, rel string) ([]byte, error) {
			read = append(read, rel)
			if rel == "sessions/main/big.jsonl" {
				return nil, fmt.Errorf("read transcript %s: %w", rel, errHistoryTranscriptTooLarge)
			}
			return []byte("{}"), nil
		}
		var notices []string
		opts := distillCommandOptions{notice: func(m string) { notices = append(notices, m) }}

		if err := validateDistillCanonicalInputs(context.Background(), t.TempDir(), sessions, opts, branchOf); err != nil {
			t.Fatalf("one over-cap transcript must not refuse the pass: %v", err)
		}
		if len(read) != 3 {
			t.Errorf("the preflight stopped early: read %v", read)
		}
		if len(notices) != 1 || !strings.Contains(notices[0], "too-big") {
			t.Errorf("the skip must be disclosed, naming the session; got %v", notices)
		}
	})

	t.Run("every other input failure still refuses everything", func(t *testing.T) {
		for name, failure := range map[string]error{
			"a containment breach": fmt.Errorf("%s: escapes root", brainstore.StateCorruptCode),
			"cancellation":         context.Canceled,
			"an oversize joined with cancellation": errors.Join(
				errHistoryTranscriptTooLarge, context.Canceled),
		} {
			t.Run(name, func(t *testing.T) {
				distillReadCanonicalTranscript = func(_ context.Context, _, rel string) ([]byte, error) {
					if rel == "sessions/main/big.jsonl" {
						return nil, failure
					}
					return []byte("{}"), nil
				}
				opts := distillCommandOptions{notice: func(string) {}}
				if err := validateDistillCanonicalInputs(context.Background(), t.TempDir(), sessions, opts, branchOf); err == nil {
					t.Fatalf("%s must refuse the whole pass, not be skipped", name)
				}
			})
		}
	})
}

// PARTIAL progress is the fix; TOTAL failure must still refuse, and that
// boundary is the one the existing safety test
// (TestUnsafeTranscriptNeverInvokesDistillProvider) defends: a refused distill
// must leave NO derived state -- no facts manifest, no cache. Tolerating the
// skip unconditionally turned "refuse and leave nothing" into "succeed having
// done nothing", which would publish a manifest claiming coverage for a corpus
// that produced no facts. Caught by the full suite, not by this file.
func TestDistillRefusesWhenEveryCandidateIsUnreadable(t *testing.T) {
	branchOf := func(s exportSession) string { return "main" }
	restore := distillReadCanonicalTranscript
	t.Cleanup(func() { distillReadCanonicalTranscript = restore })
	distillReadCanonicalTranscript = func(_ context.Context, _, rel string) ([]byte, error) {
		return nil, fmt.Errorf("read transcript %s: %w", rel, errHistoryTranscriptTooLarge)
	}
	opts := distillCommandOptions{notice: func(string) {}}

	t.Run("every candidate unreadable refuses", func(t *testing.T) {
		sessions := []exportSession{
			{SessionID: "big-1", TranscriptPath: "sessions/main/b1.jsonl", Branch: "main"},
			{SessionID: "big-2", TranscriptPath: "sessions/main/b2.jsonl", Branch: "main"},
		}
		err := validateDistillCanonicalInputs(context.Background(), t.TempDir(), sessions, opts, branchOf)
		if err == nil {
			t.Fatal("nothing could be distilled, so the pass must refuse rather than publish derived state")
		}
		if !strings.Contains(err.Error(), brainstore.InputTooLargeCode) {
			t.Errorf("the refusal must stay diagnosable and name the cause: %v", err)
		}
	})

	t.Run("one readable candidate is enough to proceed", func(t *testing.T) {
		distillReadCanonicalTranscript = func(_ context.Context, _, rel string) ([]byte, error) {
			if rel == "sessions/main/ok.jsonl" {
				return []byte("{}"), nil
			}
			return nil, fmt.Errorf("read transcript %s: %w", rel, errHistoryTranscriptTooLarge)
		}
		sessions := []exportSession{
			{SessionID: "big-1", TranscriptPath: "sessions/main/b1.jsonl", Branch: "main"},
			{SessionID: "ok", TranscriptPath: "sessions/main/ok.jsonl", Branch: "main"},
		}
		if err := validateDistillCanonicalInputs(context.Background(), t.TempDir(), sessions, opts, branchOf); err != nil {
			t.Fatalf("one readable session must let the pass proceed: %v", err)
		}
	})

	t.Run("no candidates at all is not a refusal", func(t *testing.T) {
		// A branch filter that matches nothing has no unreadable input; it
		// must not be turned into an error by the total-failure rule.
		opts := distillCommandOptions{branch: "other", notice: func(string) {}}
		sessions := []exportSession{{SessionID: "big-1", TranscriptPath: "sessions/main/b1.jsonl", Branch: "main"}}
		if err := validateDistillCanonicalInputs(context.Background(), t.TempDir(), sessions, opts, branchOf); err != nil {
			t.Fatalf("a filter that selects nothing must not refuse: %v", err)
		}
	})
}
