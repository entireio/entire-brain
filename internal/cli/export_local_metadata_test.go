package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func localMetaSnapshot(t *testing.T, ids ...string) *checkpointSnapshot {
	t.Helper()
	snap := &checkpointSnapshot{
		Selected:              map[string]selectedSession{},
		UnreadableCheckpoints: map[string]struct{}{},
	}
	for i, id := range ids {
		session := selectedSession{
			CheckpointID: id,
			SessionID:    "session-" + id,
			SessionIndex: 0,
			Branch:       "main",
			SourceBranch: "main",
			Agent:        "Claude Code",
			CreatedAt:    time.Date(2026, 1, 1, 0, 0, i, 0, time.UTC),
			// The pointers a snapshot resolves and a routed detail never has.
			SourceTranscriptPath:  "checkpoints/" + id + "/0/full.jsonl",
			CompactTranscriptPath: "checkpoints/" + id + "/0/transcript.jsonl",
			SourceKey:             "source\x00" + id,
		}
		snap.Selected[selectedSessionKey("main", session.SessionID)] = session
		snap.SessionCheckpoints = append(snap.SessionCheckpoints, sessionCheckpointOf(session))
	}
	return snap
}

func localMetaCheckpoints(ids ...string) []checkpointListEntry {
	entries := make([]checkpointListEntry, 0, len(ids))
	for i, id := range ids {
		entries = append(entries, checkpointListEntry{
			CheckpointID: id,
			IsLogsOnly:   true,
			Date:         time.Date(2026, 1, 1, 0, 0, i, 0, time.UTC),
		})
	}
	return entries
}

// The saving itself: a checkpoint the local store accounts for costs no child
// process, and its sessions still reach the selection.
func TestLocalCheckpointMetadataSpendsNoChildOnACoveredCheckpoint(t *testing.T) {
	t.Parallel()
	snap := localMetaSnapshot(t, "aaa111aaa111", "bbb222bbb222")
	checkpoints := localMetaCheckpoints("aaa111aaa111", "bbb222bbb222")
	local, warnings := newLocalCheckpointMetadata(snap, checkpoints)
	if local == nil {
		t.Fatalf("shortcut declined a snapshot that covers the whole catalog: %v", warnings)
	}

	// No fixtures at all: any detail read would fail, so a pass that succeeds
	// is a pass that asked for none.
	runner := &fakeCommandRunner{}
	selected, selectWarnings, complete, err := selectRoutedCheckpointSessions(
		context.Background(), runner, "/repo", "entire-test", checkpoints, checkpointBranchDestinations{}, nil, local)
	if err != nil || !complete {
		t.Fatalf("covered pass = warnings:%v complete:%v err:%v", selectWarnings, complete, err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("a fully covered catalog still spawned %d children: %+v", len(runner.calls), runner.calls)
	}
	if len(selected) != 2 {
		t.Fatalf("expected both sessions from the local store, got %d", len(selected))
	}
	if !strings.Contains(strings.Join(selectWarnings, "\n"), "resolved metadata for 2 of 2 checkpoints from the local checkpoint store") {
		t.Fatalf("the pass did not report what came from where: %v", selectWarnings)
	}
}

// A seeded record must be what a routed detail would have produced, not a
// snapshot record: the snapshot's transcript pointers are resolved against a
// snapshot this pass has already released.
func TestLocalCheckpointMetadataDropsSnapshotOnlyTranscriptPointers(t *testing.T) {
	t.Parallel()
	snap := localMetaSnapshot(t, "aaa111aaa111")
	checkpoints := localMetaCheckpoints("aaa111aaa111")
	local, _ := newLocalCheckpointMetadata(snap, checkpoints)
	if local == nil {
		t.Fatal("shortcut declined a covering snapshot")
	}
	selected := map[string]selectedSession{}
	local.seed(selected)
	if len(selected) != 1 {
		t.Fatalf("seed produced %d sessions", len(selected))
	}
	for _, session := range selected {
		if session.SourceTranscriptPath != "" || session.CompactTranscriptPath != "" || session.SourceKey != "" {
			t.Fatalf("a seeded record carried snapshot-resolved pointers: %+v", session)
		}
		if session.Agent == "" || session.CreatedAt.IsZero() || session.SessionID == "" {
			t.Fatalf("seeding dropped metadata a routed detail would have carried: %+v", session)
		}
	}
}

// Only the covered ones are skipped. A checkpoint discovery listed that the
// local store knows nothing about must still be asked about, or the remote-only
// checkpoints — 147 of 499 on the repository this was measured against —
// would silently vanish.
func TestLocalCheckpointMetadataStillAsksAboutUncoveredCheckpoints(t *testing.T) {
	t.Parallel()
	snap := localMetaSnapshot(t, "aaa111aaa111")
	checkpoints := localMetaCheckpoints("aaa111aaa111", "ccc333ccc333")
	local, _ := newLocalCheckpointMetadata(snap, checkpoints)
	if local == nil {
		t.Fatal("shortcut declined a partially covering snapshot")
	}
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("entire-test", "checkpoint", "explain", "--json", "ccc333ccc333"): {
			stdout: `{"checkpoint_id":"ccc333ccc333","branch":"main","sessions":[{"index":0,"session_id":"remote-only","branch":"main"}]}`,
		},
	}}
	selected, warnings, complete, err := selectRoutedCheckpointSessions(
		context.Background(), runner, "/repo", "entire-test", checkpoints, checkpointBranchDestinations{}, nil, local)
	if err != nil || !complete {
		t.Fatalf("mixed pass = warnings:%v complete:%v err:%v", warnings, complete, err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("expected exactly one child, for the uncovered checkpoint: %+v", runner.calls)
	}
	if got := strings.Join(runner.calls[0].args, " "); !strings.Contains(got, "ccc333ccc333") {
		t.Fatalf("the child asked about the wrong checkpoint: %s", got)
	}
	if _, ok := selected[selectedSessionKey("main", "remote-only")]; !ok {
		t.Fatalf("the uncovered checkpoint's session is missing: %+v", selected)
	}
	if _, ok := selected[selectedSessionKey("main", "session-aaa111aaa111")]; !ok {
		t.Fatalf("the covered checkpoint's session is missing: %+v", selected)
	}
}

// A checkpoint the snapshot itself could not read is not covered, however
// present it looks in the membership list.
func TestLocalCheckpointMetadataDoesNotCoverAnUnreadableCheckpoint(t *testing.T) {
	t.Parallel()
	snap := localMetaSnapshot(t, "aaa111aaa111", "bbb222bbb222")
	snap.UnreadableCheckpoints["bbb222bbb222"] = struct{}{}
	local, _ := newLocalCheckpointMetadata(snap, localMetaCheckpoints("aaa111aaa111", "bbb222bbb222"))
	if local == nil {
		t.Fatal("shortcut declined")
	}
	if !local.covers("aaa111aaa111") {
		t.Fatal("a readable checkpoint was not covered")
	}
	if local.covers("bbb222bbb222") {
		t.Fatal("a checkpoint the snapshot could not read was treated as covered")
	}
}

// The precondition. If a session's newest local checkpoint is one discovery
// never listed, the two disagree about membership: seeding would import a
// record from outside the discovered catalog, and dropping it would hide a
// session an older discovered checkpoint could have carried. Neither is
// defensible from the reduced selection, so the shortcut declines.
func TestLocalCheckpointMetadataDeclinesWhenTheLocalWinnerIsNotDiscovered(t *testing.T) {
	t.Parallel()
	snap := localMetaSnapshot(t, "aaa111aaa111", "zzz999zzz999")
	local, warnings := newLocalCheckpointMetadata(snap, localMetaCheckpoints("aaa111aaa111"))
	if local != nil {
		t.Fatalf("shortcut was taken although the local store and discovery disagree: covered=%d", local.count())
	}
	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, "disagree about membership") || !strings.Contains(joined, "1 of 2") {
		t.Fatalf("the refusal did not say what it found: %v", warnings)
	}

	// And declining means the ordinary pass runs: every checkpoint asked about.
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("entire-test", "checkpoint", "explain", "--json", "aaa111aaa111"): {
			stdout: `{"checkpoint_id":"aaa111aaa111","branch":"main","sessions":[{"index":0,"session_id":"asked","branch":"main"}]}`,
		},
	}}
	selected, _, complete, err := selectRoutedCheckpointSessions(
		context.Background(), runner, "/repo", "entire-test", localMetaCheckpoints("aaa111aaa111"), checkpointBranchDestinations{}, nil, nil)
	if err != nil || !complete || len(runner.calls) != 1 {
		t.Fatalf("declined pass did not fall back to asking: calls:%d complete:%v err:%v", len(runner.calls), complete, err)
	}
	if _, ok := selected[selectedSessionKey("main", "asked")]; !ok {
		t.Fatalf("fallback pass lost the session: %+v", selected)
	}
}

// A nil shortcut must behave exactly as before it existed, including the
// terminal refusal when nothing was readable.
func TestSelectRoutedCheckpointSessionsUnchangedWithoutTheShortcut(t *testing.T) {
	t.Parallel()
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("entire-test", "checkpoint", "explain", "--json", "aaa111aaa111"): {
			err: errors.New("unreadable"),
		},
	}}
	selected, warnings, complete, err := selectRoutedCheckpointSessions(
		context.Background(), runner, "/repo", "entire-test", localMetaCheckpoints("aaa111aaa111"), checkpointBranchDestinations{}, nil, nil)
	if err == nil || complete || len(selected) != 0 {
		t.Fatalf("all-unreadable pass = selected:%d warnings:%v complete:%v err:%v", len(selected), warnings, complete, err)
	}
	if !strings.Contains(err.Error(), checkpointScopeIncompleteCode) {
		t.Fatalf("unexpected terminal error: %v", err)
	}
}

// The fan-out and the shortcut have to agree. Prefetching is speculative, so a
// fan-out that still reads every covered checkpoint would spend exactly the
// child processes the shortcut exists to avoid — and spend them on answers the
// selection pass never looks at.
func TestRoutedDetailFanoutDoesNotPrefetchCoveredCheckpoints(t *testing.T) {
	t.Parallel()
	if checkpointDetailFanoutLimit() < 2 {
		t.Skipf("fan-out is off on this host: limit is %d", checkpointDetailFanoutLimit())
	}
	ids := []string{"aaa111aaa111", "bbb222bbb222", "ccc333ccc333", "ddd444ddd444"}
	snap := localMetaSnapshot(t, ids[:3]...)
	checkpoints := localMetaCheckpoints(ids...)
	local, warnings := newLocalCheckpointMetadata(snap, checkpoints)
	if local == nil || local.count() != 3 {
		t.Fatalf("expected three covered checkpoints, got %v (%v)", local.count(), warnings)
	}

	details := map[string]string{
		"ddd444ddd444": `{"checkpoint_id":"ddd444ddd444","branch":"main","sessions":[{"index":0,"session_id":"only-remote","branch":"main"}]}`,
	}
	runner := newExplainFanoutRunner(details)
	got := prefetchCheckpointDetails(context.Background(), runner, "/repo", "entire-test", checkpoints, local, nil)
	if got != nil {
		t.Fatalf("one uncovered checkpoint is not enough work to fan out, yet it did: %d results", len(got))
	}
	if calls, _ := runner.observed(); calls != 0 {
		t.Fatalf("the prefetch read %d checkpoints when only one was uncovered", calls)
	}

	// With two uncovered it does fan out — and still only over those two.
	snap2 := localMetaSnapshot(t, ids[:2]...)
	local2, _ := newLocalCheckpointMetadata(snap2, checkpoints)
	details["ccc333ccc333"] = `{"checkpoint_id":"ccc333ccc333","branch":"main","sessions":[{"index":0,"session_id":"also-remote","branch":"main"}]}`
	runner2 := newExplainFanoutRunner(details)
	if got := prefetchCheckpointDetails(context.Background(), runner2, "/repo", "entire-test", checkpoints, local2, nil); got == nil {
		t.Fatal("two uncovered checkpoints should fan out")
	}
	calls, _ := runner2.observed()
	if calls != 2 {
		t.Fatalf("the prefetch read %d checkpoints; only the 2 uncovered ones should be read", calls)
	}
}

// routedEntry is a membership entry as Entire's list produces it: marked as
// routed, and declaring which sessions the checkpoint has.
func routedEntry(id string, sessionIDs ...string) checkpointListEntry {
	return checkpointListEntry{
		CheckpointID: id,
		IsLogsOnly:   true,
		RoutedOrigin: true,
		SessionIDs:   sessionIDs,
		SessionCount: len(sessionIDs),
	}
}

// When Entire names a checkpoint, Entire is authoritative about its sessions.
// Its declared list arrives with the membership list at no extra cost, so it
// can be honoured without a child — but only when the local store actually
// read every session on it.
func TestLocalCheckpointMetadataHonoursEntiresDeclaredSessionList(t *testing.T) {
	t.Parallel()
	snap := localMetaSnapshot(t, "aaa111aaa111")
	// localMetaSnapshot names each session "session-<id>".
	present := routedEntry("aaa111aaa111", "session-aaa111aaa111")
	local, warnings := newLocalCheckpointMetadata(snap, []checkpointListEntry{present})
	if local == nil || !local.covers("aaa111aaa111") {
		t.Fatalf("a routed checkpoint whose declared sessions are all local was not covered: %v", warnings)
	}

	// One session Entire declares that the local store never read: the whole
	// point of the refusal path, so it must not be covered.
	missing := routedEntry("aaa111aaa111", "session-aaa111aaa111", "remote-only-session")
	local, _ = newLocalCheckpointMetadata(snap, []checkpointListEntry{missing})
	if local.covers("aaa111aaa111") {
		t.Fatal("a checkpoint Entire says has a session the local store never read was covered, which is how that session gets silently dropped")
	}

	// No declared list at all cannot be cross-checked. Older hosts omit the
	// field, and so does the host for the remote checkpoints whose metadata it
	// could not load — exactly the ones that need asking about.
	silent := routedEntry("aaa111aaa111")
	local, _ = newLocalCheckpointMetadata(snap, []checkpointListEntry{silent})
	if local.covers("aaa111aaa111") {
		t.Fatal("a routed entry declaring no sessions was covered instead of asked about")
	}
}

// A checkpoint only the local ref enumeration knows about carries no routed
// claim, so there is no second authority to honour and the local store's own
// reading stands.
func TestLocalCheckpointMetadataCoversACheckpointOnlyLocalRefsKnow(t *testing.T) {
	t.Parallel()
	snap := localMetaSnapshot(t, "aaa111aaa111")
	entries := localMetaCheckpoints("aaa111aaa111")
	if entries[0].RoutedOrigin {
		t.Fatal("fixture is wrong: a local ref entry must not be marked routed")
	}
	local, _ := newLocalCheckpointMetadata(snap, entries)
	if local == nil || !local.covers("aaa111aaa111") {
		t.Fatal("a locally enumerated checkpoint was not covered")
	}
}

// Branch scope returns the routed list without ever merging it, so the mark
// has to be set where the list is produced. Unmarked, every branch-scoped
// entry would look like one only the local refs know about.
func TestRoutedCheckpointListMarksEveryEntryAsRouted(t *testing.T) {
	t.Parallel()
	for _, searchAll := range []bool{true, false} {
		runner := &fakeCommandRunner{fallback: func(name string, args []string) (fakeCommandResponse, bool) {
			return fakeCommandResponse{stdout: `[{"checkpoint_id":"aaa111aaa111","is_logs_only":true},{"checkpoint_id":"bbb222bbb222","is_logs_only":true}]`}, true
		}}
		entries, _, err := listRoutedCheckpoints(context.Background(), runner, "/repo", "entire-test", 0, searchAll)
		if err != nil || len(entries) != 2 {
			t.Fatalf("searchAll=%v: %d entries err=%v", searchAll, len(entries), err)
		}
		for _, entry := range entries {
			if !entry.RoutedOrigin {
				t.Fatalf("searchAll=%v: %s came from Entire's list but is not marked routed", searchAll, entry.CheckpointID)
			}
		}
	}
}
