package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
)

// TestBriefAcceptsBranchAndReadsThatPartition closes a gap between what this
// product TELLS agents to do and what it accepts.
//
// Facts are partitioned per branch and a branch's file is a fork-time copy, so
// a brief run from a worktree reads a partition that is usually empty. brain
// already knows this and says so: briefBranchBlindSpot's own remedy text is
// "memories are stored per branch; retry with `--branch main`". recall has
// taken --branch since it shipped, and the shipped agent guide instructs agents
// to pass it to recall AND brief.
//
// brief had no such flag. cobra rejects an unknown flag before RunE, so an
// agent that followed the brain's own printed remedy -- or the guide -- got
// `unknown flag: --branch` and NO packet at all, on the command the guide makes
// step 1 of every substantive task. Measured on this machine: `brief` had
// served 2 memories in the store's entire history against recall's 267.
func TestBriefAcceptsBranchAndReadsThatPartition(t *testing.T) {
	const otherBranchFact = "retries are capped at three"

	fixture := newBrainBriefProfileFixture(t)
	// The fixture's live branch is "feature"; put the fact somewhere else.
	if err := factstore.Write(fixture.brainDir, "main", []factRecord{
		vitalityTestFact(otherBranchFact, "main", fixture.opts.Now()),
	}); err != nil {
		t.Fatalf("write main facts: %v", err)
	}
	if err := updateFactSourceManifest(fixture.brainDir, fixture.opts.Now()); err != nil {
		t.Fatalf("update manifest: %v", err)
	}

	// 1. The flag must exist at all. This is the whole bug: every assertion
	//    below is unreachable while cobra refuses the flag.
	withFlag, err := execute(t, NewRootCommand(fixture.opts), "brief", "retries capped", "--json", "--no-semantic", "--branch", "main")
	if err != nil {
		t.Fatalf("brief --branch must be accepted; the brain's own blind-spot remedy and the shipped agent guide both tell agents to pass it: %v", err)
	}
	if !factTextInPacket(t, withFlag, otherBranchFact) {
		t.Fatalf("--branch main was accepted but the packet did not read main's partition:\n%s", withFlag)
	}

	// 2. And it must be the flag doing the work, not the default. Without it
	//    the same query reads the checked-out branch, which does not hold the
	//    fact -- so a passing assertion above could otherwise mean nothing.
	withoutFlag, err := execute(t, NewRootCommand(fixture.opts), "brief", "retries capped", "--json", "--no-semantic")
	if err != nil {
		t.Fatalf("brief without --branch: %v", err)
	}
	if factTextInPacket(t, withoutFlag, otherBranchFact) {
		t.Fatalf("the default already reads main, so this test cannot tell whether --branch did anything:\n%s", withoutFlag)
	}
}

// factTextInPacket reports whether a brief packet surfaced a memory with this
// text, reading the facts section rather than the whole document so a match
// inside a warning (which NAMES other branches) cannot pass for a served fact.
func factTextInPacket(t *testing.T, out, text string) bool {
	t.Helper()
	var packet struct {
		Facts []struct {
			Text string `json:"text"`
		} `json:"facts"`
	}
	if err := json.Unmarshal([]byte(out), &packet); err != nil {
		t.Fatalf("brief --json did not emit a decodable packet: %v\n%s", err, out)
	}
	for _, fact := range packet.Facts {
		if strings.Contains(fact.Text, text) {
			return true
		}
	}
	return false
}
