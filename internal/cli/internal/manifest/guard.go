package manifest

import (
	"strings"

	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
)

// ExcludedTranscriptPaths resolves the brain-relative transcript paths that
// belong to excluded sessions, from the trusted manifest only.
func ExcludedTranscriptPaths(manifest *Manifest, stones brainstore.SessionTombstones) map[string]string {
	out := map[string]string{}
	if len(stones.Excluded) == 0 || manifest == nil || manifest.Sources == nil || manifest.Sources.Sessions == nil {
		return out
	}
	for _, session := range manifest.Sources.Sessions.Sessions {
		id := strings.TrimSpace(session.SessionID)
		if id == "" {
			continue
		}
		if _, excluded := stones.Excluded[id]; !excluded {
			continue
		}
		rel := brainstore.NormalizePrivacyTranscriptPath(session.TranscriptPath)
		if rel != "" {
			out[rel] = id
		}
	}
	return out
}

// LoadSessionReadGuard builds the guard, resolving the manifest's excluded
// transcript paths alongside brainstore's durable transaction scope.
func LoadSessionReadGuard(brainDir string, manifest *Manifest) (brainstore.SessionReadGuard, error) {
	return brainstore.LoadSessionReadGuard(brainDir, func(stones brainstore.SessionTombstones) map[string]string {
		return ExcludedTranscriptPaths(manifest, stones)
	})
}

func GuardBlocksSession(g brainstore.SessionReadGuard, session Session) bool {
	return g.BlocksSession(session.SessionID) || g.BlocksPath(session.TranscriptPath)
}

func GuardSessions(guard brainstore.SessionReadGuard, sessions []Session) []Session {
	if guard.Empty() || len(sessions) == 0 {
		return sessions
	}
	kept := make([]Session, 0, len(sessions))
	for _, session := range sessions {
		if !GuardBlocksSession(guard, session) {
			kept = append(kept, session)
		}
	}
	return kept
}
