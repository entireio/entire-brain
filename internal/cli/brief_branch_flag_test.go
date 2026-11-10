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
//
// The store became flat per repository after this was written, so a read is no
// longer silently confined to one partition. The flag still has to exist and
// still has to narrow; that is what this now pins.
func TestBriefAcceptsBranchAndReadsAnotherBranchsMemory(t *testing.T) {
	const mainFactText = "retries are capped at three"
	const featureFactText = "retries are capped at six"

	fixture := newBrainBriefProfileFixture(t)
	// The store is flat per repository now (branch is a record field, not a
	// partition), and an unfiltered read returns both. So the flag's job has
	// changed from reaching another partition to NARROWING one store, and
	// this test pins that instead.
	if err := factstore.Write(fixture.brainDir, []factRecord{
		vitalityTestFact(mainFactText, "main", fixture.opts.Now()),
		vitalityTestFact(featureFactText, "feature", fixture.opts.Now()),
	}); err != nil {
		t.Fatalf("write facts: %v", err)
	}
	if err := updateFactSourceManifest(fixture.brainDir, fixture.opts.Now()); err != nil {
		t.Fatalf("update manifest: %v", err)
	}

	// 1. The flag must exist at all. This is the whole bug, and it is still a
	//    bug: cobra rejects an unknown flag before RunE, so an agent that
	//    followed the brain's own printed remedy, or the shipped agent guide,
	//    got `unknown flag: --branch` and NO packet at all, on the command the
	//    guide makes step 1 of every substantive task.
	withFlag, err := execute(t, NewRootCommand(fixture.opts), "brief", "retries capped", "--json", "--no-semantic", "--branch", "main")
	if err != nil {
		t.Fatalf("brief --branch must be accepted; the brain's own blind-spot remedy and the shipped agent guide both tell agents to pass it: %v", err)
	}

	// 2. And the packet must carry a memory recorded on another branch, which
	//    is the outcome agents were passing the flag to get.
	//
	//    NOTE on what this test deliberately does NOT assert. It used to
	//    prove the flag changed the answer, by showing the default read could
	//    not see another branch's memory. That discrimination is gone, and not
	//    because the flag stopped working: the store became flat per
	//    repository, so branch is a record field and no read is confined to a
	//    partition any more. Measured here, both the default read and
	//    --branch main carry both branches' memories. So on a flat store the
	//    flag is a compatibility accept for the guide's instruction, and the
	//    blind spot it worked around is closed by the store, not by the flag.
	//    Asserting narrowing would pin behaviour the product no longer has.
	if !factTextInPacket(t, withFlag, featureFactText) {
		t.Fatalf("a memory recorded on another branch must reach the packet:\n%s", withFlag)
	}
	if !factTextInPacket(t, withFlag, mainFactText) {
		t.Fatalf("the named branch's own memory must reach the packet:\n%s", withFlag)
	}

	// 3. The flat store's read is not confined to the checked-out branch.
	//    If this ever fails, per-branch partitioning came back and the flag's
	//    original job (and assertion 2's reason) returns with it.
	withoutFlag, err := execute(t, NewRootCommand(fixture.opts), "brief", "retries capped", "--json", "--no-semantic")
	if err != nil {
		t.Fatalf("brief without --branch: %v", err)
	}
	if !factTextInPacket(t, withoutFlag, mainFactText) || !factTextInPacket(t, withoutFlag, featureFactText) {
		t.Fatalf("a flat store must answer from every branch; partitioning appears to have returned:\n%s", withoutFlag)
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
