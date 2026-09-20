package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestBuildHandoffPacket(t *testing.T) {
	_, manifest, index := historyEvalFixture(t) // s1 (older) + s2, with request/decision/validation records
	now := time.Date(2026, 6, 10, 13, 0, 0, 0, time.UTC)
	facts := []factRecord{
		{ID: "f:old", Kind: factKindConvention, Text: "older fact", Status: factStatusActive, UpdatedAt: now.Add(-3 * time.Hour)},
		{ID: "f:new", Kind: factKindClosedNegative, Text: "newer dead end", Status: factStatusActive, UpdatedAt: now.Add(-time.Hour)},
		{ID: "f:gone", Kind: factKindDecision, Text: "superseded", Status: factStatusSuperseded, UpdatedAt: now},
	}
	// Fixture sessions are created at 10:00 (s1) and 11:00 (s2) on the
	// fixture's own clock; a distill at 10:30 leaves exactly s2 undigested.
	manifest.Sources.Facts = &factSourceManifest{LastDistilledAt: time.Date(2026, 6, 10, 10, 30, 0, 0, time.UTC)}

	p := buildHandoffPacket(manifest, index, facts, "main", 5, now, sessionReadGuard{})

	if len(p.Sessions) != 2 {
		t.Fatalf("expected both sessions, got %d", len(p.Sessions))
	}
	// Most recent session first; its records attributed via transcript path.
	s2 := p.Sessions[0]
	if s2.SessionID != shortSessionID("session-bbbb-2222") {
		t.Fatalf("sessions must be recency-ordered, got %s first", s2.SessionID)
	}
	if len(s2.Decisions) != 1 || !strings.Contains(s2.Decisions[0], "export cursor") {
		t.Fatalf("s2's decision record missing: %+v", s2)
	}
	if len(s2.Validations) != 1 || !strings.Contains(s2.Validations[0], "cache hit rate") {
		t.Fatalf("s2's validation record missing: %+v", s2)
	}
	s1 := p.Sessions[1]
	if s1.Request != "add retry backoff to the fetcher" {
		t.Fatalf("s1's opening request (lowest-line request record) missing: %q", s1.Request)
	}

	// Recent facts: active only, newest first.
	if len(p.RecentFacts) != 2 || p.RecentFacts[0].ID != "f:new" {
		t.Fatalf("recent facts should be active-only, newest first: %+v", p.RecentFacts)
	}
	// Both fixture sessions predate... s2 created now-1h, distill at now-90m →
	// s2 (and only s2) is newer than the last distill.
	if p.SessionsSinceDistill != 1 {
		t.Fatalf("expected 1 session since distill, got %d", p.SessionsSinceDistill)
	}
	if len(p.Branches) != 1 || p.Branches[0] != "main" {
		t.Fatalf("active branches = %v, want [main]", p.Branches)
	}
}

func TestBuildHandoffPacketOmitsExcludedSessionCanary(t *testing.T) {
	_, manifest, index := historyEvalFixture(t)
	const canary = "PRIVATE-HANDOFF-CANARY"
	excluded := manifest.Sources.Sessions.Sessions[1]
	for i := range index.Records {
		if index.Records[i].Path == excluded.TranscriptPath {
			index.Records[i].Summary += " " + canary
		}
	}
	facts := []factRecord{{
		ID: "fact:private", Status: factStatusActive, Text: canary,
		Provenance: []factAnchor{{SessionID: excluded.SessionID, Transcript: excluded.TranscriptPath}},
	}}
	stones := sessionTombstones{Version: sessionTombstonesVersion, Excluded: map[string]sessionTombstone{
		excluded.SessionID: {At: time.Now().UTC()},
	}}
	guard := sessionReadGuard{ids: stones.Excluded, paths: excludedTranscriptPaths(manifest, stones)}
	packet := buildHandoffPacket(manifest, index, facts, "main", 5, time.Now().UTC(), guard)
	data, err := json.Marshal(packet)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), canary) || strings.Contains(string(data), excluded.SessionID) {
		t.Fatalf("handoff exposed excluded session: %s", data)
	}
	if len(packet.Sessions) != len(manifest.Sources.Sessions.Sessions)-1 || len(packet.RecentFacts) != 0 {
		t.Fatalf("handoff did not filter excluded session/facts: %+v", packet)
	}
}

func TestBriefRequiresTaskUnlessHandoff(t *testing.T) {
	opts := Options{Env: EntireEnv{RepoRoot: t.TempDir()}, Runner: ExecRunner{}, Now: time.Now}
	if _, err := execute(t, newBrainBriefCommand(opts)); err == nil || !strings.Contains(err.Error(), "--handoff") {
		t.Fatalf("brief without a task should point at --handoff, got %v", err)
	}
}
