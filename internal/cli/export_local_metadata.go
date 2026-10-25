package cli

import "fmt"

// localCheckpointMetadata lets discovery go on deciding membership while the
// metadata for every checkpoint whose bytes are already local comes from one
// snapshot read instead of one child process each.
//
// This is the division export.go states but did not act on: "Discovery
// controls membership; it does not establish where bytes live." A configured
// checkpoint remote refuses the direct snapshot, because the remote may hold
// checkpoints the local refs do not — and it genuinely does. But that is an
// argument about membership, and discovery already settles it by merging the
// routed list with a local ref enumeration. Re-deriving the metadata with one
// `checkpoint explain` child per checkpoint then pays, over and over, for a
// question that was already answered: on a repository with 5,768 discovered
// checkpoints, 5,587 of them were in a snapshot that took 3.6s to read whole.
type localCheckpointMetadata struct {
	// selected is the snapshot's own reduced selection, copied verbatim: the
	// local path builds a superset of what a routed detail carries (the same
	// fields plus the transcript paths), and it has already resolved
	// collisions under the same total order, so re-running the reduction over
	// it would only re-expand branch destinations that are already applied.
	selected map[string]selectedSession
	// covered holds the discovered checkpoint IDs whose sessions that
	// selection accounts for. A checkpoint the snapshot could not read is
	// deliberately absent, so it still gets asked about.
	covered map[string]struct{}
}

// newLocalCheckpointMetadata decides whether the shortcut applies to this
// pairing of a discovered catalog and a local snapshot, and refuses it rather
// than guessing when the two disagree about membership.
//
// The disagreement that matters is a session whose newest LOCAL checkpoint is
// one discovery never listed. Its winning record would then enter the export
// from outside the membership discovery established, and dropping it instead
// would hide a session that a discovered, older checkpoint of the same session
// could have carried. Neither is defensible from the reduced selection alone,
// so the shortcut steps aside and the ordinary routed pass runs.
func newLocalCheckpointMetadata(snapshot *checkpointSnapshot, discovered []checkpointListEntry) (*localCheckpointMetadata, []string) {
	if snapshot == nil || len(snapshot.Selected) == 0 {
		return nil, nil
	}

	discoveredIDs := make(map[string]struct{}, len(discovered))
	for _, entry := range discovered {
		if entry.CheckpointID != "" {
			discoveredIDs[entry.CheckpointID] = struct{}{}
		}
	}

	outside := 0
	for _, session := range snapshot.Selected {
		if _, ok := discoveredIDs[session.CheckpointID]; !ok {
			outside++
		}
	}
	if outside > 0 {
		return nil, []string{fmt.Sprintf(
			"local checkpoint metadata not used: %d of %d locally selected sessions are newest on a checkpoint discovery did not list, so discovery and the local store disagree about membership; asking Entire for every checkpoint instead",
			outside, len(snapshot.Selected))}
	}

	// What the local store read, per checkpoint.
	localSessions := make(map[string]map[string]struct{}, len(snapshot.SessionCheckpoints))
	for _, member := range snapshot.SessionCheckpoints {
		if member.CheckpointID == "" || member.SessionID == "" {
			continue
		}
		if localSessions[member.CheckpointID] == nil {
			localSessions[member.CheckpointID] = map[string]struct{}{}
		}
		localSessions[member.CheckpointID][member.SessionID] = struct{}{}
	}

	covered := make(map[string]struct{}, len(localSessions))
	for _, entry := range discovered {
		sessions, read := localSessions[entry.CheckpointID]
		if !read {
			continue
		}
		if _, unreadable := snapshot.UnreadableCheckpoints[entry.CheckpointID]; unreadable {
			continue
		}
		if entry.RoutedOrigin {
			// Entire named this checkpoint, so Entire is authoritative about
			// which sessions it has. Its list arrives with the membership
			// list, which costs nothing extra, and every session on it has to
			// be one the local store actually read. A session Entire declares
			// and the local refs cannot resolve MUST reach the detail path,
			// because that is what refuses the export instead of silently
			// shipping a smaller one (TestFilelessRoutedCatalogUsesLocal-
			// Identities pins exactly that).
			//
			// An entry with no declared sessions cannot be cross-checked —
			// older hosts omit the field, and the host also omits it for the
			// remote checkpoints whose metadata it could not load, which are
			// precisely the ones that need asking. Fail closed and ask.
			if len(entry.SessionIDs) == 0 {
				continue
			}
			missing := false
			for _, sessionID := range entry.SessionIDs {
				if _, ok := sessions[sessionID]; !ok {
					missing = true
					break
				}
			}
			if missing {
				continue
			}
		}
		covered[entry.CheckpointID] = struct{}{}
	}
	if len(covered) == 0 {
		return nil, nil
	}
	return &localCheckpointMetadata{selected: snapshot.Selected, covered: covered}, nil
}

// seed copies the snapshot's selection into the pass's accumulator.
//
// The values are taken as they are, not re-added: addSelectedSession reads
// candidate.Branch as the SOURCE branch and fans it back out over the branch
// destinations, which the snapshot path has already done, so re-adding would
// map destinations onto destinations.
//
// What is dropped matters as much as what is kept. A snapshot record also
// carries where that snapshot found the transcript — SourceTranscriptPath,
// CompactTranscriptPath, SourceKey — and those are resolved against the
// snapshot that produced them. This shortcut's snapshot is released as soon
// as the metadata is taken, and the write phase later loads its own, so
// carrying the pointers across would couple two snapshots by paths one of
// them never produced. Cleared, each record is exactly what a routed detail
// would have yielded, and the write phase computes the transcript path from
// the checkpoint and session index as it already does for every routed
// session.
func (m *localCheckpointMetadata) seed(selected map[string]selectedSession) {
	for key, session := range m.selected {
		session.SourceTranscriptPath = ""
		session.CompactTranscriptPath = ""
		session.SourceKey = ""
		selected[key] = session
	}
}

func (m *localCheckpointMetadata) covers(checkpointID string) bool {
	if m == nil {
		return false
	}
	_, ok := m.covered[checkpointID]
	return ok
}

func (m *localCheckpointMetadata) count() int {
	if m == nil {
		return 0
	}
	return len(m.covered)
}
