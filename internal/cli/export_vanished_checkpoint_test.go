package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// vanishedFixture builds a snapshot that can serve one session's transcript
// and knows nothing about another — the shape a squash-merged repository has,
// where a manual-commit checkpoint names a commit its branch deletion
// destroyed.
// vanishedRunner answers the object-type probe the validator makes for a
// transcript chunk it can see, so "available" really means available.
func vanishedRunner() *fakeCommandRunner {
	return &fakeCommandRunner{fallback: func(name string, args []string) (fakeCommandResponse, bool) {
		for _, arg := range args {
			if arg == "cat-file" {
				return fakeCommandResponse{stdout: "blob\n"}, true
			}
		}
		return fakeCommandResponse{}, false
	}}
}

func vanishedFixture() (*checkpointSnapshot, []exportSession) {
	snap := &checkpointSnapshot{
		TranscriptMode:     "raw",
		TranscriptFileName: "full.jsonl",
		SessionCheckpoints: []sessionCheckpoint{{
			CheckpointID: "aaa111aaa111", SessionID: "alive", SessionIndex: 0,
			TranscriptPath: "checkpoints/aaa111aaa111/0/full.jsonl", SourceKey: "src",
		}},
		// The source must actually carry the transcript path, or
		// snapshotTranscriptChunks finds no chunks and EVERY session reads as
		// unavailable. A first version of this fixture left Sources empty and
		// the "nothing available" test below passed for that reason rather
		// than for the reason it claims.
		Sources: map[string]checkpointSnapshotSource{
			"src": {
				GitDir:    "/repo/.git",
				Ref:       "refs/heads/entire/checkpoints/v1",
				TreePaths: map[string]struct{}{"checkpoints/aaa111aaa111/0/full.jsonl": {}},
			},
		},
	}
	sessions := []exportSession{
		{SessionID: "alive", LatestCheckpoint: "aaa111aaa111", SessionIndex: 0, Branch: "main"},
		{SessionID: "6772544b-bdcb-4b07-be35-c46c1a37a1a4", LatestCheckpoint: "fd86c6e4bdb2", SessionIndex: 0, Branch: "main"},
	}
	return snap, sessions
}

// One session whose bytes are gone must cost its own session and nothing else.
// It used to refuse the entire export, which on a squash-merging repository is
// permanent: the commit a manual-commit checkpoint names is destroyed when its
// branch is deleted, so no fetch recovers it and refusing preserves nothing
// while discarding every available session.
func TestVanishedCheckpointIsPartitionedOutNotFatal(t *testing.T) {
	t.Parallel()
	snap, sessions := vanishedFixture()
	usable, unavailable, err := partitionLocalTranscriptSessions(context.Background(), vanishedRunner(), snap, sessions, "raw")
	if err != nil {
		t.Fatalf("partition returned a hard error for a merely-unavailable session: %v", err)
	}
	if len(unavailable) != 1 {
		t.Fatalf("unavailable = %d, want 1: %v", len(unavailable), unavailable)
	}
	if !strings.Contains(unavailable[0].Error(), "fd86c6e4bdb2") {
		t.Fatalf("the diagnostic does not name the checkpoint whose bytes are gone: %s", unavailable[0])
	}
	if len(usable) != 1 || usable[0].SessionID != "alive" {
		t.Fatalf("usable = %+v, want just the session whose transcript is present", usable)
	}
}

// The all-or-nothing contract is kept for the caller that wants it, so nothing
// that relied on the old behaviour changes shape.
func TestValidateLocalTranscriptSessionsStillFailsOnTheFirstUnavailable(t *testing.T) {
	t.Parallel()
	snap, sessions := vanishedFixture()
	err := validateLocalTranscriptSessions(context.Background(), vanishedRunner(), snap, sessions, "raw")
	if err == nil {
		t.Fatal("the strict wrapper stopped reporting an unavailable session")
	}
	if !strings.Contains(err.Error(), "fd86c6e4bdb2") {
		t.Fatalf("the strict wrapper lost the detail: %v", err)
	}
}

// Nothing available is the source-went-unreadable case, and there the right
// answer really is to stop: republishing an empty corpus would erase an
// indexed brain. Partitioning must therefore leave zero usable sessions so the
// caller can still refuse.
func TestNothingAvailableLeavesNoUsableSessions(t *testing.T) {
	t.Parallel()
	snap, sessions := vanishedFixture()
	snap.SessionCheckpoints = nil // the source knows about nothing at all
	usable, unavailable, err := partitionLocalTranscriptSessions(context.Background(), vanishedRunner(), snap, sessions, "raw")
	if err != nil {
		t.Fatalf("unexpected hard error: %v", err)
	}
	if len(usable) != 0 {
		t.Fatalf("usable = %d, want 0 so the caller can refuse", len(usable))
	}
	if len(unavailable) != 2 {
		t.Fatalf("unavailable = %d, want both sessions", len(unavailable))
	}
}

// Every session available must partition to exactly the input, with no
// diagnostics — the common case must not change at all.
func TestEverythingAvailablePartitionsUnchanged(t *testing.T) {
	t.Parallel()
	snap, sessions := vanishedFixture()
	only := sessions[:1]
	usable, unavailable, err := partitionLocalTranscriptSessions(context.Background(), vanishedRunner(), snap, only, "raw")
	if err != nil {
		t.Fatal(err)
	}
	if len(unavailable) != 0 {
		t.Fatalf("a fully available selection reported %d unavailable: %v", len(unavailable), unavailable)
	}
	if len(usable) != 1 {
		t.Fatalf("usable = %d, want 1", len(usable))
	}
}

// The threshold itself, pinned as a value. A weakening sweep showed that
// changing it back to "refuse if anything is unavailable" broke no test,
// because every other guard here exercises the partition function rather than
// the caller's decision. This is that decision.
func TestUnavailableSessionsAreFatalOnlyWhenNothingIsServable(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		usable, unavailable int
		fatal               bool
	}{
		"nothing servable at all":        {usable: 0, unavailable: 1, fatal: true},
		"nothing servable, many missing": {usable: 0, unavailable: 2561, fatal: true},
		"one vanished among many":        {usable: 2560, unavailable: 1, fatal: false},
		"half missing":                   {usable: 1, unavailable: 1, fatal: false},
		"everything available":           {usable: 2561, unavailable: 0, fatal: false},
		"empty selection":                {usable: 0, unavailable: 0, fatal: false},
	} {
		if got := unavailableSessionsAreFatal(c.usable, c.unavailable); got != c.fatal {
			t.Errorf("%s: usable=%d unavailable=%d -> fatal=%v, want %v", name, c.usable, c.unavailable, got, c.fatal)
		}
	}
}

// stubExitError models a process exit status without spawning a process, so a
// fixture can script git's exit 1 ("that object name is not present here")
// against the per-exec probe. *exec.ExitError carries the same ExitCode()
// capability from a real child.
type stubExitError struct{ code int }

func (e stubExitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }
func (e stubExitError) ExitCode() int { return e.code }

// probedFixture puts BOTH sessions in the snapshot with transcript paths the
// source lists, so the git object probe actually runs for both. vanishedFixture
// never reaches it: its second session is absent from the snapshot entirely, so
// it is classified from data alone and no git command is issued.
func probedFixture() (*checkpointSnapshot, []exportSession) {
	snap := &checkpointSnapshot{
		TranscriptMode:     "raw",
		TranscriptFileName: "full.jsonl",
		SessionCheckpoints: []sessionCheckpoint{
			{CheckpointID: "aaa111aaa111", SessionID: "alive", SessionIndex: 0,
				TranscriptPath: "checkpoints/aaa111aaa111/0/full.jsonl", SourceKey: "src"},
			{CheckpointID: "bbb222bbb222", SessionID: "probed", SessionIndex: 0,
				TranscriptPath: "checkpoints/bbb222bbb222/0/full.jsonl", SourceKey: "src"},
		},
		Sources: map[string]checkpointSnapshotSource{
			"src": {
				GitDir: "/repo/.git",
				Ref:    "refs/heads/entire/checkpoints/v1",
				TreePaths: map[string]struct{}{
					"checkpoints/aaa111aaa111/0/full.jsonl": {},
					"checkpoints/bbb222bbb222/0/full.jsonl": {},
				},
			},
		},
	}
	sessions := []exportSession{
		{SessionID: "alive", LatestCheckpoint: "aaa111aaa111", SessionIndex: 0, Branch: "main"},
		{SessionID: "probed", LatestCheckpoint: "bbb222bbb222", SessionIndex: 0, Branch: "main"},
	}
	return snap, sessions
}

// probeRunner serves "blob" for every chunk except the probed session's, whose
// `cat-file -t` and `cat-file -e` answers the caller scripts.
func probeRunner(objectType, exists fakeCommandResponse) *fakeCommandRunner {
	const probed = "bbb222bbb222"
	return &fakeCommandRunner{fallback: func(name string, args []string) (fakeCommandResponse, bool) {
		if name != "git" || len(args) != 3 || args[0] != "cat-file" {
			return fakeCommandResponse{}, false
		}
		if !strings.Contains(args[2], probed) {
			return fakeCommandResponse{stdout: "blob\n"}, true
		}
		switch args[1] {
		case "-t":
			return objectType, true
		case "-e":
			return exists, true
		}
		return fakeCommandResponse{}, false
	}}
}

// git's own "that object is not present here" answer -- confirmed by the exit
// code of a second, narrower probe -- is the ONE probe failure that costs its
// session instead of the export.
func TestConfirmedMissingObjectCostsOnlyItsOwnSession(t *testing.T) {
	t.Parallel()
	snap, sessions := probedFixture()
	runner := probeRunner(
		fakeCommandResponse{err: stubExitError{code: 128}, stderr: "fatal: git cat-file: could not get object info\n"},
		fakeCommandResponse{err: stubExitError{code: 1}},
	)
	usable, unavailable, err := partitionLocalTranscriptSessions(context.Background(), runner, snap, sessions, "raw")
	if err != nil {
		t.Fatalf("a confirmed-missing object must not fail the export: %v", err)
	}
	if len(unavailable) != 1 || !strings.Contains(unavailable[0].Error(), "bbb222bbb222") {
		t.Fatalf("unavailable = %v, want just the probed session", unavailable)
	}
	if len(usable) != 1 || usable[0].SessionID != "alive" {
		t.Fatalf("usable = %+v, want just the available session", usable)
	}
}

// The defect this guard exists for. A git invocation that could not run says
// NOTHING about whether the bytes are there, and the caller is fatal only when
// nothing at all is servable -- so recording it as unavailability drops the
// session and republishes a shrunken corpus, silently. These must be errors.
func TestProbeFailureThatIsNotAMissingObjectFailsTheExport(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		objectType, exists fakeCommandResponse
	}{
		// git never ran: no exit status to read at all.
		"spawn failure": {
			objectType: fakeCommandResponse{err: errors.New("fork/exec git: resource temporarily unavailable")},
			exists:     fakeCommandResponse{err: errors.New("fork/exec git: resource temporarily unavailable")},
		},
		// The runner refuses because it cannot enforce the local-only policy.
		"runner cannot enforce policy": {
			objectType: fakeCommandResponse{err: errors.New("no_egress: command runner cannot enforce GIT_NO_LAZY_FETCH=1")},
			exists:     fakeCommandResponse{err: errors.New("no_egress: command runner cannot enforce GIT_NO_LAZY_FETCH=1")},
		},
		// The child was killed, so it has no exit code of its own.
		"child killed": {
			objectType: fakeCommandResponse{err: stubExitError{code: -1}},
			exists:     fakeCommandResponse{err: stubExitError{code: -1}},
		},
		// 128 is git's generic fatal: shared between "this spec does not
		// resolve", "this is not a repository" and a bad config value. Not
		// evidence about one object.
		"generic git fatal": {
			objectType: fakeCommandResponse{err: stubExitError{code: 128}, stderr: "fatal: not a git repository\n"},
			exists:     fakeCommandResponse{err: stubExitError{code: 128}, stderr: "fatal: not a git repository\n"},
		},
		// The confirming probe says the object IS there, so the type probe
		// failed for some other, transient reason.
		"object is present after all": {
			objectType: fakeCommandResponse{err: stubExitError{code: 128}},
			exists:     fakeCommandResponse{},
		},
		// git answered, and the answer is that the snapshot's own path is not
		// a transcript blob. A fault in this brain, not in one session.
		"snapshot path is not a blob": {
			objectType: fakeCommandResponse{stdout: "tree\n"},
			exists:     fakeCommandResponse{},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			snap, sessions := probedFixture()
			usable, unavailable, err := partitionLocalTranscriptSessions(context.Background(), probeRunner(c.objectType, c.exists), snap, sessions, "raw")
			if err == nil {
				t.Fatalf("partition reported data instead of an error: usable=%+v unavailable=%v", usable, unavailable)
			}
			if len(usable) != 0 || len(unavailable) != 0 {
				t.Fatalf("an error must carry no partition: usable=%+v unavailable=%v", usable, unavailable)
			}
		})
	}
}

// A cancelled context is never evidence about an object, and it must not even
// be investigated: the confirming probe would be killed on arrival.
func TestCancelledContextFailsTheExportRatherThanShrinkingIt(t *testing.T) {
	t.Parallel()
	snap, sessions := probedFixture()
	runner := probeRunner(
		fakeCommandResponse{err: context.Canceled},
		// Scripted as a missing answer on purpose: if cancellation were
		// classified by asking git again, this would wrongly read as missing.
		fakeCommandResponse{err: stubExitError{code: 1}},
	)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, unavailable, err := partitionLocalTranscriptSessions(ctx, runner, snap, sessions, "raw")
	if err == nil {
		t.Fatalf("a cancelled probe was reported as unavailability: %v", unavailable)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("the cancellation was lost: %v", err)
	}
}

// A skipped session's diagnostic must keep the IDENTITY of what git said, not
// only its wording, and must keep it through the strict all-or-nothing wrapper
// too. Flattening it to a formatted string leaves the message intact and makes
// a cancellation indistinguishable from a vanished object upstream -- the same
// conflation this partition exists to remove, one layer higher.
func TestUnavailableDiagnosticsKeepTheirErrorIdentity(t *testing.T) {
	t.Parallel()
	snap, sessions := probedFixture()
	missing := func() *fakeCommandRunner {
		return probeRunner(
			fakeCommandResponse{err: stubExitError{code: 128}},
			fakeCommandResponse{err: stubExitError{code: 1}},
		)
	}
	_, unavailable, err := partitionLocalTranscriptSessions(context.Background(), missing(), snap, sessions, "raw")
	if err != nil {
		t.Fatal(err)
	}
	if len(unavailable) != 1 {
		t.Fatalf("unavailable = %v, want 1", unavailable)
	}
	if !errors.Is(unavailable[0], errGitObjectMissing) {
		t.Fatalf("the diagnostic kept its message and lost its cause: %v", unavailable[0])
	}
	if wrapped := validateLocalTranscriptSessions(context.Background(), missing(), snap, sessions, "raw"); !errors.Is(wrapped, errGitObjectMissing) {
		t.Fatalf("the strict wrapper flattened the cause: %v", wrapped)
	}
}

// promisorProbeRunner is probeRunner plus an answer to the promisor-remote
// config read, so the partial-clone branch can be exercised. The remote is
// named by URL on purpose: that is the real shape in the cli checkpoint
// repository, and a detector written against remote.origin.promisor would
// pass every other test and miss the only repository that matters.
func promisorProbeRunner(objectType, exists fakeCommandResponse, promisor bool) *fakeCommandRunner {
	inner := probeRunner(objectType, exists)
	fallback := inner.fallback
	inner.fallback = func(name string, args []string) (fakeCommandResponse, bool) {
		if name == "git" && len(args) == 3 && args[0] == "config" && args[1] == "--get-regexp" {
			if !promisor {
				// git exits 1 with no output when nothing matches.
				return fakeCommandResponse{err: stubExitError{code: 1}}, true
			}
			return fakeCommandResponse{stdout: "remote.https://github.com/entireio/cli-checkpoints.git.promisor true\n"}, true
		}
		return fallback(name, args)
	}
	return inner
}

// In a PARTIAL clone, git's "not here" does not mean "does not exist": an
// unfetched blob and a destroyed one both exit 1 silently under the local-only
// policy. Partitioning on that answer would republish a corpus smaller than
// the one already published, over bytes a sync would recover. This is the one
// case where a confirmed-missing object must still fail the export.
func TestAMissingObjectInAPartialCloneFailsTheExportRatherThanShrinkingIt(t *testing.T) {
	promisorRemoteAnswers.Delete("/repo/.git")
	t.Cleanup(func() { promisorRemoteAnswers.Delete("/repo/.git") })

	snap, sessions := probedFixture()
	runner := promisorProbeRunner(
		fakeCommandResponse{err: stubExitError{code: 128}, stderr: "fatal: git cat-file: could not get object info\n"},
		fakeCommandResponse{err: stubExitError{code: 1}},
		true,
	)
	usable, unavailable, err := partitionLocalTranscriptSessions(context.Background(), runner, snap, sessions, "raw")
	if err == nil {
		t.Fatalf("a partial clone's absent object shrank the corpus instead of failing: usable=%d unavailable=%v", len(usable), unavailable)
	}
	if !errors.Is(err, errGitObjectMissing) {
		t.Fatalf("the cause must survive for errors.Is: %v", err)
	}
	if !strings.Contains(err.Error(), "partial clone") || !strings.Contains(err.Error(), "bbb222bbb222") {
		t.Fatalf("the error must name the reason and the chunk: %v", err)
	}
}

// The same absent object in a FULL clone is absent for good, so it must still
// cost only its own session -- otherwise this guard would have undone #489.
func TestAMissingObjectWithoutAPromisorRemoteStillCostsOnlyItsSession(t *testing.T) {
	promisorRemoteAnswers.Delete("/repo/.git")
	t.Cleanup(func() { promisorRemoteAnswers.Delete("/repo/.git") })

	snap, sessions := probedFixture()
	runner := promisorProbeRunner(
		fakeCommandResponse{err: stubExitError{code: 128}, stderr: "fatal: git cat-file: could not get object info\n"},
		fakeCommandResponse{err: stubExitError{code: 1}},
		false,
	)
	usable, unavailable, err := partitionLocalTranscriptSessions(context.Background(), runner, snap, sessions, "raw")
	if err != nil {
		t.Fatalf("a full clone's confirmed-missing object must not fail the export: %v", err)
	}
	if len(usable) != 1 || len(unavailable) != 1 {
		t.Fatalf("usable=%d unavailable=%d, want 1 and 1", len(usable), len(unavailable))
	}
}

// The fake runner above answers any `--get-regexp`, so it cannot prove the
// PATTERN is right -- and the pattern is the whole point. cli's checkpoint
// repository names its promisor remote by URL
// (remote.https://github.com/entireio/cli-checkpoints.git.promisor) and does
// not set extensions.partialclone at all, so a detector written against
// remote.origin.promisor would satisfy every other test here and miss the one
// repository this guard exists for. Real git, real config, no fake.
func TestPromisorDetectionFindsAURLNamedRemoteWithRealGit(t *testing.T) {
	for _, probe := range []struct {
		name   string
		key    string
		expect bool
	}{
		{"url-named promisor remote, as cli has", `remote.https://github.com/entireio/cli-checkpoints.git.promisor`, true},
		{"conventionally named promisor remote", "remote.origin.promisor", true},
		{"no promisor remote at all", "", false},
	} {
		t.Run(probe.name, func(t *testing.T) {
			dir := t.TempDir()
			runner := ExecRunner{}
			ctx := context.Background()
			if _, _, err := runner.Run(ctx, dir, "git", "init", "-q", "."); err != nil {
				t.Skipf("git unavailable: %v", err)
			}
			if probe.key != "" {
				if _, _, err := runner.Run(ctx, dir, "git", "config", probe.key, "true"); err != nil {
					t.Fatalf("seed %s: %v", probe.key, err)
				}
			}
			promisorRemoteAnswers.Delete(dir)
			t.Cleanup(func() { promisorRemoteAnswers.Delete(dir) })
			if got := gitDirHasPromisorRemote(ctx, runner, dir); got != probe.expect {
				t.Fatalf("gitDirHasPromisorRemote = %v, want %v (key %q)", got, probe.expect, probe.key)
			}
		})
	}
}
