package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
)

// vanishedFixture builds a snapshot that can serve one session's transcript
// and knows nothing about another — the shape a squash-merged repository has,
// where a manual-commit checkpoint names a commit its branch deletion
// destroyed.
// vanishedRunner answers the object-type probe the validator makes for a
// transcript chunk it can see, so "available" really means available -- AND
// answers the promisor-remote config read the way real git answers it in a
// FULL clone (exit 1, nothing matched).
//
// That second answer is not decoration. Every test built on this fixture
// asserts behaviour that is only true of a full clone, and leaving the probe
// unhandled makes the fake return a plain "unexpected command" error: an
// unasked question, which the guards deliberately read as "may still fetch".
// Commit 5f15accf had to state the same thing for probedFixture once the
// object-missing path grew its promisor gate; the snapshot-entry gate brings
// the identical requirement here. Tests that want the UNASKED question use
// vanishedRunnerWithUnansweredPromisorProbe below and say so in their name.
func vanishedRunner() *fakeCommandRunner {
	inner := vanishedRunnerWithUnansweredPromisorProbe()
	fallback := inner.fallback
	inner.fallback = func(name string, args []string) (fakeCommandResponse, bool) {
		if name == "git" && len(args) == 3 && args[0] == "config" && args[1] == "--get-regexp" {
			return fakeCommandResponse{err: stubExitError{code: 1}}, true
		}
		return fallback(name, args)
	}
	return inner
}

// vanishedRunnerWithUnansweredPromisorProbe serves transcript chunks but
// leaves the promisor config read unhandled, which is how a probe that could
// not be answered reaches the code under test.
func vanishedRunnerWithUnansweredPromisorProbe() *fakeCommandRunner {
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
				GitDir:    "/vanished-repo/.git",
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
	snap, sessions := probedFixtureInGitDir(t, "/probed-confirmed-missing/.git")
	// promisorProbeRunner, not probeRunner: "costs only its own session" is
	// true of a FULL clone, and the fixture has to say so. probeRunner leaves
	// the promisor config probe unhandled, which the fake answers with a plain
	// "unexpected command" error -- an unasked question, not a full clone.
	runner := promisorProbeRunner(
		fakeCommandResponse{err: stubExitError{code: 128}, stderr: "fatal: git cat-file: could not get object info\n"},
		fakeCommandResponse{err: stubExitError{code: 1}},
		false,
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
			snap, sessions := probedFixtureInGitDir(t, "/probed-probe-failure-"+name+"/.git")
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
	snap, sessions := probedFixtureInGitDir(t, "/probed-cancelled/.git")
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
	snap, sessions := probedFixtureInGitDir(t, "/probed-identity/.git")
	// Full clone, stated in the fixture: see the note in
	// TestConfirmedMissingObjectCostsOnlyItsOwnSession.
	missing := func() *fakeCommandRunner {
		return promisorProbeRunner(
			fakeCommandResponse{err: stubExitError{code: 128}},
			fakeCommandResponse{err: stubExitError{code: 1}},
			false,
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
	snap, sessions := probedFixtureInGitDir(t, "/probed-partial/.git")
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
	snap, sessions := probedFixtureInGitDir(t, "/probed-full-clone/.git")
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

// A probe that could not ASK must not be remembered as a no.
//
// gitDirHasPromisorRemote's false answer licenses the caller to treat an absent
// object as destroyed and drop the session, and the answer is cached per git
// directory for the life of the process. So a single transient fault -- a
// cancelled context, a deadline, a failed spawn -- previously pinned a real
// partial clone to "no promisor remote" and every later absent object there
// read as gone. That is the shrink this file's other tests exist to prevent,
// reached through the cache instead of through the probe.
//
// Real git, because the distinction being asserted is `--get-regexp`'s exit 1
// for "nothing matched" (a real answer) against a failure to run at all.
func TestAnIndeterminatePromisorProbeIsNeitherCachedNorReadAsNo(t *testing.T) {
	dir := t.TempDir()
	runner := ExecRunner{}
	if _, _, err := runner.Run(context.Background(), dir, "git", "init", "-q", "."); err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	if _, _, err := runner.Run(context.Background(), dir, "git", "config", "remote.origin.promisor", "true"); err != nil {
		t.Fatalf("seed promisor remote: %v", err)
	}
	promisorRemoteAnswers.Delete(dir)
	t.Cleanup(func() { promisorRemoteAnswers.Delete(dir) })

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if got := gitDirHasPromisorRemote(cancelled, runner, dir); !got {
		t.Fatalf("an unasked question was reported as proof there is no promisor remote (got %v); absent objects here would read as destroyed", got)
	}
	if _, cached := promisorRemoteAnswers.Load(dir); cached {
		t.Fatalf("an indeterminate probe was cached; every later call for %s would reuse it", dir)
	}
	if got := gitDirHasPromisorRemote(context.Background(), runner, dir); !got {
		t.Fatalf("after the failed probe the real answer was lost: got %v, want true", got)
	}
	if answer, cached := promisorRemoteAnswers.Load(dir); !cached || answer != true {
		t.Fatalf("a definitive answer must be cached: cached=%v answer=%v", cached, answer)
	}
}

// The other half of the same contract: exit 1 from `--get-regexp` IS an answer,
// so a full clone must still be cached as such -- otherwise this fix would turn
// every export into a refusal and undo #489 by the other side.
func TestNoPromisorRemoteIsADefinitiveCachedAnswer(t *testing.T) {
	dir := t.TempDir()
	runner := ExecRunner{}
	if _, _, err := runner.Run(context.Background(), dir, "git", "init", "-q", "."); err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	promisorRemoteAnswers.Delete(dir)
	t.Cleanup(func() { promisorRemoteAnswers.Delete(dir) })

	if got := gitDirHasPromisorRemote(context.Background(), runner, dir); got {
		t.Fatalf("a full clone must report no promisor remote, got %v", got)
	}
	answer, cached := promisorRemoteAnswers.Load(dir)
	if !cached || answer != false {
		t.Fatalf("exit 1 is a real answer and must be cached: cached=%v answer=%v", cached, answer)
	}
}

// The difference those two fixtures were silently relying on, pinned.
//
// probeRunner leaves the promisor config probe unhandled, and the fake answers
// an unhandled command with a plain errors.New -- no exit code. That is an
// unasked question, and an unasked question must not license treating an absent
// object as destroyed. Two tests above asserted full-clone behaviour on exactly
// that fixture and passed only because the probe used to read any error as "no
// promisor remote"; both now state the full clone explicitly.
func TestAnUnansweredPromisorProbeRefusesRatherThanSkipping(t *testing.T) {
	t.Parallel()
	snap, sessions := probedFixtureInGitDir(t, "/probed-unanswered/.git")
	runner := probeRunner(
		fakeCommandResponse{err: stubExitError{code: 128}},
		fakeCommandResponse{err: stubExitError{code: 1}},
	)

	_, unavailable, err := partitionLocalTranscriptSessions(context.Background(), runner, snap, sessions, "raw")
	if err == nil {
		t.Fatalf("an unanswered promisor probe shrank the corpus instead of refusing: unavailable=%v", unavailable)
	}
	if !errors.Is(err, errGitObjectMissing) {
		t.Fatalf("the cause must survive for errors.Is: %v", err)
	}
}

// The transcript of the session that is about to vanish, kept distinct from
// the servable one so the two have different blob ids and destroying one
// cannot destroy the other.
const vanishedCacheTranscript = conversationClaudeFixture + `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"bytes only this cache still holds"}]}}
`

// vanishedCacheFixture builds a repository whose checkpoint catalog names two
// sessions, both initially servable, and returns the git helper the test uses
// to destroy one of their transcript objects afterwards. The helper returns
// git's trimmed stdout so the test can resolve a blob id.
func vanishedCacheFixture(t *testing.T) (Options, string, func(args ...string) string) {
	t.Helper()
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, stderr.String())
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	alive, gone := checkpointPath("aaa111aaa111"), checkpointPath("bbb222bbb222")
	files := map[string]string{
		".entire/settings.json":    `{"enabled":true}`,
		alive + "/metadata.json":   `{"branch":"main","sessions":[{"metadata":"/` + alive + `/0/metadata.json","transcript":"/` + alive + `/0/full.jsonl"}]}`,
		alive + "/0/metadata.json": `{"checkpoint_id":"aaa111aaa111","session_id":"session-alive","branch":"main","created_at":"2026-01-01T00:00:00Z"}`,
		alive + "/0/full.jsonl":    conversationClaudeFixture,
		gone + "/metadata.json":    `{"branch":"main","sessions":[{"metadata":"/` + gone + `/0/metadata.json","transcript":"/` + gone + `/0/full.jsonl"}]}`,
		gone + "/0/metadata.json":  `{"checkpoint_id":"bbb222bbb222","session_id":"session-gone","branch":"main","created_at":"2026-01-02T00:00:00Z"}`,
		gone + "/0/full.jsonl":     vanishedCacheTranscript,
	}
	for rel, data := range files {
		full := filepath.Join(repo, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	git("add", ".")
	git("-c", "commit.gpgsign=false", "commit", "-qm", "checkpoint fixture")
	git("update-ref", v1MainRef, "HEAD")
	opts := Options{Runner: ExecRunner{}, Env: EntireEnv{RepoRoot: repo, PluginDataDir: t.TempDir(), PluginCacheDir: t.TempDir(), PluginStateDir: t.TempDir(), PluginConfigDir: t.TempDir()}, Now: func() time.Time { return time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC) }}
	return opts, repo, git
}

// A session whose transcript object is gone is skipped and dropped from the
// manifest -- and the manifest is exactly what the transcript cache sweep
// consults for liveness. The cached copy left behind by the legacy disk-mode
// exports is then the ONLY surviving copy of those bytes, so dropping the
// session must not hand its cache to the sweep.
//
// Liveness is read from the manifest ON DISK, so the dropped session is still
// listed during the export that drops it and only becomes unlisted on the
// NEXT one. That later export is where the file was deleted, which is why
// this runs two consecutive persistent exports and checks the file after each.
func TestVanishedSessionKeepsItsLastCachedTranscript(t *testing.T) {
	opts, repo, git := vanishedCacheFixture(t)
	storage, err := repoStoragePaths(context.Background(), opts.Runner, opts.Env, repo)
	if err != nil {
		t.Fatal(err)
	}
	brain := storage.BrainDir

	// A legacy disk-mode export: both cached transcripts are materialized and
	// both sessions are recorded.
	runFilelessExport(t, opts, brain, false)
	first, err := bmanifest.Load(brain)
	if err != nil {
		t.Fatal(err)
	}
	cached := map[string]string{}
	for _, session := range first.Sources.Sessions.Sessions {
		cached[session.SessionID] = session.TranscriptPath
	}
	if len(cached) != 2 || cached["session-gone"] == "" || cached["session-alive"] == "" {
		t.Fatalf("fixture must export both sessions, got %v", cached)
	}
	gonePath := filepath.Join(brain, filepath.FromSlash(cached["session-gone"]))
	if data, err := os.ReadFile(gonePath); err != nil || string(data) != vanishedCacheTranscript {
		t.Fatalf("cached transcript for the soon-vanished session: %q %v", data, err)
	}

	// Let the cached copy run ahead of its checkpoint, which is what carries a
	// legacy cached transcript through the migration to checkpoint storage.
	// Without this the brain would not be checkpoint-backed when the object
	// vanishes, and the first export would refuse instead of dropping the
	// session -- a refusal, not a deletion, and not the case under test.
	preserved := vanishedCacheTranscript + `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"tail that exists nowhere else"}]}}` + "\n"
	if err := os.WriteFile(gonePath, []byte(preserved), 0600); err != nil {
		t.Fatal(err)
	}
	runFilelessExport(t, opts, "", false)
	migrated, err := bmanifest.Load(brain)
	if err != nil {
		t.Fatal(err)
	}
	if !checkpointBackedSessions(migrated) || len(migrated.Sources.Sessions.Sessions) != 2 {
		t.Fatalf("migration must publish checkpoint storage for both sessions: %+v", migrated.Sources.Sessions)
	}
	if data, err := os.ReadFile(gonePath); err != nil || string(data) != preserved {
		t.Fatalf("migration lost the divergent cache: %q %v", data, err)
	}
	// Narrow rescue, not blanket retention: the servable session's cache is
	// recoverable from its checkpoint and is swept by that same migration.
	if _, err := os.Stat(filepath.Join(brain, filepath.FromSlash(cached["session-alive"]))); !os.IsNotExist(err) {
		t.Fatalf("recoverable cache was not swept: %v", err)
	}

	// The transcript BLOB leaves the object store while the catalog keeps
	// naming it: the shape a destroyed manual-commit checkpoint has on a
	// squash-merging repository, and the only one the partition classifies as
	// one session's unavailability rather than a fault.
	blob := git("rev-parse", v1MainRef+":"+checkpointPath("bbb222bbb222")+"/0/full.jsonl")
	if len(blob) < 3 {
		t.Fatalf("blob id = %q", blob)
	}
	if err := os.Remove(filepath.Join(repo, ".git", "objects", blob[:2], blob[2:])); err != nil {
		t.Fatal("destroy transcript object:", err)
	}

	for round := 1; round <= 2; round++ {
		runFilelessExport(t, opts, "", false)
		manifest, err := bmanifest.Load(brain)
		if err != nil {
			t.Fatal(err)
		}
		var recorded []string
		for _, session := range manifest.Sources.Sessions.Sessions {
			recorded = append(recorded, session.SessionID)
		}
		if len(recorded) != 1 || recorded[0] != "session-alive" {
			t.Fatalf("round %d recorded %v; the brain must not record a session it cannot serve", round, recorded)
		}
		got, err := os.ReadFile(gonePath)
		if err != nil || string(got) != preserved {
			t.Fatalf("round %d destroyed the only surviving copy of the skipped session's transcript: %q %v", round, got, err)
		}
	}
}

// vanishedFixtureInGitDir and probedFixtureInGitDir relocate a fixture's
// source to a git directory of the calling test's own, and own that
// directory's entry in promisorRemoteAnswers.
//
// That cache is process-global and these tests disagree about what it should
// hold: several state a full clone (cached false), one states a probe nobody
// answered (which must not be cached at all), one states a promisor remote.
// Sharing "/repo/.git" between them made the answer depend on execution order,
// and widening the promisor gate to the snapshot-entry path turned that latent
// race into a reproducible failure of
// TestAnUnansweredPromisorProbeRefusesRatherThanSkipping. One directory per
// test removes the shared state instead of ordering around it.
func vanishedFixtureInGitDir(t *testing.T, gitDir string) (*checkpointSnapshot, []exportSession) {
	t.Helper()
	snap, sessions := vanishedFixture()
	return relocateSnapshotSource(t, snap, sessions, gitDir)
}

func probedFixtureInGitDir(t *testing.T, gitDir string) (*checkpointSnapshot, []exportSession) {
	t.Helper()
	snap, sessions := probedFixture()
	return relocateSnapshotSource(t, snap, sessions, gitDir)
}

func relocateSnapshotSource(t *testing.T, snap *checkpointSnapshot, sessions []exportSession, gitDir string) (*checkpointSnapshot, []exportSession) {
	t.Helper()
	source := snap.Sources["src"]
	source.GitDir = gitDir
	snap.Sources["src"] = source
	promisorRemoteAnswers.Delete(gitDir)
	t.Cleanup(func() { promisorRemoteAnswers.Delete(gitDir) })
	return snap, sessions
}

// "This snapshot names no transcript for that session" is NOT proof that the
// bytes are gone. In a partial clone, or whenever the promisor probe could not
// be answered, it is equally what an unfetched checkpoint ref looks like --
// and a sync recovers those bytes. Skipping the session there republishes a
// corpus smaller than the published one, which is exactly the loss the
// object-missing path already refuses. This case was not gated at all.
func TestANoSnapshotEntryInAPartialCloneFailsTheExportRatherThanShrinkingIt(t *testing.T) {
	for name, c := range map[string]struct {
		gitDir string
		runner func() *fakeCommandRunner
	}{
		// git answered, and the answer names a promisor remote.
		"promisor remote": {
			gitDir: "/no-entry-partial/.git",
			runner: func() *fakeCommandRunner {
				inner := vanishedRunnerWithUnansweredPromisorProbe()
				fallback := inner.fallback
				inner.fallback = func(name string, args []string) (fakeCommandResponse, bool) {
					if name == "git" && len(args) == 3 && args[0] == "config" && args[1] == "--get-regexp" {
						return fakeCommandResponse{stdout: "remote.https://github.com/entireio/cli-checkpoints.git.promisor true\n"}, true
					}
					return fallback(name, args)
				}
				return inner
			},
		},
		// Nobody answered: an unasked question must never license treating an
		// absence as definitive, which is the same asymmetry
		// probeGitDirPromisorRemote exists to preserve.
		"unanswered probe": {
			gitDir: "/no-entry-unanswered/.git",
			runner: vanishedRunnerWithUnansweredPromisorProbe,
		},
	} {
		t.Run(name, func(t *testing.T) {
			snap, sessions := vanishedFixtureInGitDir(t, c.gitDir)
			usable, unavailable, err := partitionLocalTranscriptSessions(context.Background(), c.runner(), snap, sessions, "raw")
			if err == nil {
				t.Fatalf("an unproven absence shrank the corpus instead of failing: usable=%d unavailable=%v", len(usable), unavailable)
			}
			if len(usable) != 0 || len(unavailable) != 0 {
				t.Fatalf("an error must carry no partition: usable=%+v unavailable=%v", usable, unavailable)
			}
			if !errors.Is(err, errTranscriptNotInSnapshot) {
				t.Fatalf("the cause must survive for errors.Is: %v", err)
			}
			if !strings.Contains(err.Error(), "sync checkpoints and retry") || !strings.Contains(err.Error(), "fd86c6e4bdb2") {
				t.Fatalf("the error must keep the sync-and-retry shape and name the session's checkpoint: %v", err)
			}
			// The strict wrapper must not flatten the cause either.
			if wrapped := validateLocalTranscriptSessions(context.Background(), c.runner(), snap, sessions, "raw"); !errors.Is(wrapped, errTranscriptNotInSnapshot) {
				t.Fatalf("the strict wrapper lost the cause: %v", wrapped)
			}
		})
	}
}

// The other half of the same contract, and the half that must not regress:
// in a FULL clone a checkpoint with no snapshot entry is a destroyed
// manual-commit checkpoint (measured on devenv/cli: 0 of 39 sampled
// commit-style ids still resolved, and no fetch recovers them). It must cost
// only its own session, or this gate would undo #489 from the other side.
func TestANoSnapshotEntryWithoutAPromisorRemoteStillCostsOnlyItsSession(t *testing.T) {
	snap, sessions := vanishedFixtureInGitDir(t, "/no-entry-full/.git")
	usable, unavailable, err := partitionLocalTranscriptSessions(context.Background(), vanishedRunner(), snap, sessions, "raw")
	if err != nil {
		t.Fatalf("a full clone's absent snapshot entry must not fail the export: %v", err)
	}
	if len(usable) != 1 || usable[0].SessionID != "alive" {
		t.Fatalf("usable = %+v, want just the available session", usable)
	}
	if len(unavailable) != 1 {
		t.Fatalf("unavailable = %v, want just the vanished session", unavailable)
	}
	if !errors.Is(unavailable[0], errTranscriptNotInSnapshot) {
		t.Fatalf("the diagnostic kept its message and lost its cause: %v", unavailable[0])
	}
	if !strings.Contains(unavailable[0].Error(), "sync checkpoints and retry") {
		t.Fatalf("the diagnostic must keep its remedy: %v", unavailable[0])
	}
}

// The gate reads EVERY source the snapshot was built from, not just the first
// one it happens to visit: a session can be missing from a snapshot whose
// fetchable source is a different checkpoint ref entirely. The two config
// answers are scripted in sequence, and the probe visits git directories in
// sorted order, so "/multi-definitive/.git" is asked first and answers git's
// definitive exit 1 while "/multi-fetchable/.git" names a promisor remote. A
// gate that stopped at the first definitive "no" would pass the two tests
// above and lose bytes here.
func TestAnyFetchableSnapshotSourceGatesTheSkip(t *testing.T) {
	snap, sessions := vanishedFixtureInGitDir(t, "/multi-definitive/.git")
	promisorRemoteAnswers.Delete("/multi-fetchable/.git")
	t.Cleanup(func() { promisorRemoteAnswers.Delete("/multi-fetchable/.git") })
	snap.Sources["second"] = checkpointSnapshotSource{
		GitDir:    "/multi-fetchable/.git",
		Ref:       "refs/heads/entire/checkpoints/v1",
		TreePaths: map[string]struct{}{},
	}
	runner := vanishedRunnerWithUnansweredPromisorProbe()
	runner.sequences = map[string][]fakeCommandResponse{
		fakeCommandKey("git", "config", "--get-regexp", `\.promisor$`): {
			{err: stubExitError{code: 1}},
			{stdout: "remote.https://github.com/entireio/cli-checkpoints.git.promisor true\n"},
		},
	}
	_, _, err := partitionLocalTranscriptSessions(context.Background(), runner, snap, sessions, "raw")
	if err == nil {
		t.Fatal("a fetchable second source did not gate the skip")
	}
	if !errors.Is(err, errTranscriptNotInSnapshot) {
		t.Fatalf("the cause must survive for errors.Is: %v", err)
	}
	// Both sources were really asked: a gate that short-circuited on the first
	// answer would leave the second response unconsumed.
	if left := len(runner.sequences[fakeCommandKey("git", "config", "--get-regexp", `\.promisor$`)]); left != 0 {
		t.Fatalf("%d scripted config answers were never consumed; the gate did not ask every source", left)
	}
}
