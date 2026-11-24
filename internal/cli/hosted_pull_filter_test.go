package cli

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/entireio/entire-brain/factmerge"
	"github.com/entireio/entire-brain/internal/factsync"
)

type reachabilityFakeRunner struct {
	reachable map[string]bool
	calls     int
}

func (r *reachabilityFakeRunner) Run(_ context.Context, _, name string, args ...string) ([]byte, []byte, error) {
	r.calls++
	if name != "git" || len(args) != 4 || args[0] != "merge-base" || args[1] != "--is-ancestor" || args[3] != "HEAD" {
		return nil, nil, errors.New("unexpected command")
	}
	if r.reachable[args[2]] {
		return nil, nil, nil
	}
	return nil, []byte("fatal: Not a valid commit name"), errors.New("exit status 128")
}

func item(trailID, createdBy string, anchors ...factmerge.Anchor) factsync.FeedItem {
	return factsync.FeedItem{
		Record:    factmerge.Record{ID: "f", Text: "t", Status: "active", Provenance: anchors},
		TrailID:   trailID,
		CreatedBy: createdBy,
	}
}

func TestKeepHostedFact(t *testing.T) {
	reachable := func(c string) bool { return c == "aaa" }
	cases := []struct {
		name string
		item factsync.FeedItem
		keep bool
	}{
		{"merged trail backlink wins", item("trail-1", "other", factmerge.Anchor{SessionID: "s1", Commit: "zzz"}), true},
		{"reachable anchor commit", item("", "other", factmerge.Anchor{SessionID: "s1", Commit: "aaa"}), true},
		{"own fact follows the member", item("", "me", factmerge.Anchor{SessionID: "s1", Commit: "zzz"}), true},
		{"repo pool: no session or commit anchors", item("", "other", factmerge.Anchor{}), true},
		{"repo pool: no anchors at all", item("", "other"), true},
		{"foreign, unreachable, session-anchored: dropped", item("", "other", factmerge.Anchor{SessionID: "s1", Commit: "zzz"}), false},
		{"unknown commit treated as unreachable", item("", "other", factmerge.Anchor{Commit: "deadbeef"}), false},
		{"session-only anchor from a foreign member: dropped", item("", "other", factmerge.Anchor{SessionID: "s1"}), false},
	}
	for _, tc := range cases {
		if got := keepHostedFact(tc.item, "me", reachable); got != tc.keep {
			t.Errorf("%s: keep = %v, want %v", tc.name, got, tc.keep)
		}
	}
	if keepHostedFact(item("", ""), "", reachable) != true {
		t.Error("anchor-less fact stays repo pool even with no member id")
	}
	if keepHostedFact(item("", "", factmerge.Anchor{SessionID: "s"}), "", reachable) != false {
		t.Error("empty member id must not match an empty createdBy")
	}
}

func TestNewCommitReachabilityMemoizes(t *testing.T) {
	runner := &reachabilityFakeRunner{reachable: map[string]bool{"aaa": true}}
	reachable := newCommitReachability(context.Background(), runner, "/repo", "HEAD")
	if !reachable("aaa") || !reachable("aaa") {
		t.Fatal("aaa should be reachable")
	}
	if reachable("bbb") || reachable("bbb") {
		t.Fatal("bbb should be unreachable")
	}
	if runner.calls != 2 {
		t.Fatalf("git calls = %d, want 2 (memoized)", runner.calls)
	}
}

func TestJwtSubClaim(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"acct-42","iss":"core"}`))
	if got := jwtSubClaim("eyJhbGciOiJSUzI1NiJ9." + payload + ".sig"); got != "acct-42" {
		t.Fatalf("sub = %q, want acct-42", got)
	}
	for _, bad := range []string{"", "garbage", "a.b", "a.!!!.c", "a." + base64.RawURLEncoding.EncodeToString([]byte(`[]`)) + ".c"} {
		if got := jwtSubClaim(bad); got != "" {
			t.Fatalf("jwtSubClaim(%q) = %q, want empty", bad, got)
		}
	}
}

func TestKeepHostedFactMemberRuleCostsNoGitCalls(t *testing.T) {
	calls := 0
	reachable := func(string) bool { calls++; return false }
	it := item("", "me", factmerge.Anchor{SessionID: "s", Commit: "c1"}, factmerge.Anchor{Commit: "c2"})
	if !keepHostedFact(it, "me", reachable) {
		t.Fatal("member rule must keep the fact")
	}
	if calls != 0 {
		t.Fatalf("the member's own facts must not cost reachability checks, got %d", calls)
	}
}
