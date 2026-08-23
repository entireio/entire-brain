package cli

import (
	"reflect"
	"strings"
	"testing"
)

func TestPackDistillCandidateMembersV2SequentialFirstFit(t *testing.T) {
	members := []distillCandidatePackMemberV2{
		packMemberV2("candidate-a", strings.Repeat("a", 16<<10)),
		packMemberV2("candidate-b", strings.Repeat("b", 16<<10)),
		packMemberV2("candidate-c", "c"),
	}
	packs, err := packDistillCandidateMembersV2(members)
	if err != nil {
		t.Fatal(err)
	}
	if len(packs) != 2 {
		t.Fatalf("packs = %d, want 2", len(packs))
	}
	assertPackV2(t, packs[0], []string{"candidate-a", "candidate-b"}, members[0].RenderedCard+members[1].RenderedCard)
	assertPackV2(t, packs[1], []string{"candidate-c"}, members[2].RenderedCard)
}

func TestPackDistillCandidateMembersV2MemberLimit(t *testing.T) {
	members := make([]distillCandidatePackMemberV2, distillCandidatePackMaxMembersV2+1)
	for index := range members {
		members[index] = packMemberV2("candidate-"+benchmarkCardIDV2(index), "x")
	}
	packs, err := packDistillCandidateMembersV2(members)
	if err != nil {
		t.Fatal(err)
	}
	if len(packs) != 2 || len(packs[0].Members) != distillCandidatePackMaxMembersV2 || len(packs[1].Members) != 1 {
		t.Fatalf("member limit packs = %+v", packs)
	}
}

func TestPackDistillCandidateMembersV2RejectsInvalidMembers(t *testing.T) {
	valid := packMemberV2("candidate-a", "card")
	tooLarge := packMemberV2("candidate-b", strings.Repeat("x", distillCandidatePackMaxRenderedBytesV2+1))
	cases := []struct {
		name    string
		members []distillCandidatePackMemberV2
		want    string
	}{
		{"blank ID", []distillCandidatePackMemberV2{{RenderedCard: "card", Anchor: distillCandidateAnchorV1{}}}, "invalid candidate ID"},
		{"spaced ID", []distillCandidatePackMemberV2{packMemberV2(" candidate-a", "card")}, "invalid candidate ID"},
		{"tab ID", []distillCandidatePackMemberV2{packMemberV2("candidate\ta", "card")}, "invalid candidate ID"},
		{"duplicate ID", []distillCandidatePackMemberV2{valid, valid}, "duplicate candidate ID"},
		{"empty card", []distillCandidatePackMemberV2{packMemberV2("candidate-a", " \n\t ")}, "empty rendered card"},
		{"oversized card", []distillCandidatePackMemberV2{tooLarge}, "exceeds"},
		{"mismatched anchor", []distillCandidatePackMemberV2{{CandidateID: "candidate-a", RenderedCard: "card", Anchor: distillCandidateAnchorV1{CandidateID: "candidate-b"}}}, "anchor candidate ID"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := packDistillCandidateMembersV2(tc.members); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestPackDistillCandidateMembersV2DeterministicAndInputOrdered(t *testing.T) {
	ordered := []distillCandidatePackMemberV2{
		packMemberV2("candidate-z", strings.Repeat("z", 20<<10)),
		packMemberV2("candidate-a", strings.Repeat("a", 14<<10)),
		packMemberV2("candidate-m", "m"),
	}
	first, err := packDistillCandidateMembersV2(ordered)
	if err != nil {
		t.Fatal(err)
	}
	second, err := packDistillCandidateMembersV2(ordered)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("identical inputs packed differently\nfirst:  %#v\nsecond: %#v", first, second)
	}
	if got := packIDsV2(first); !reflect.DeepEqual(got, []string{"candidate-z", "candidate-a", "candidate-m"}) {
		t.Fatalf("member order = %v, want supplied order", got)
	}

	reversed := []distillCandidatePackMemberV2{ordered[2], ordered[1], ordered[0]}
	got, err := packDistillCandidateMembersV2(reversed)
	if err != nil {
		t.Fatal(err)
	}
	if ids := packIDsV2(got); !reflect.DeepEqual(ids, []string{"candidate-m", "candidate-a", "candidate-z"}) {
		t.Fatalf("reordered input was sorted: %v", ids)
	}
}

// This production-sized synthetic case protects the packer from accidental
// per-card call regressions. 368 cards at 3,800 bytes are about 1.4 MB and
// should fit eight complete cards per 32 KiB pack, yielding 46 calls.
func TestPackDistillCandidateMembersV2Synthetic368Cards(t *testing.T) {
	const cards = 368
	const cardBytes = 3800
	members := make([]distillCandidatePackMemberV2, 0, cards)
	for index := 0; index < cards; index++ {
		id := "benchmark-candidate-" + benchmarkCardIDV2(index)
		members = append(members, packMemberV2(id, strings.Repeat(string(rune('a'+index%26)), cardBytes)))
	}
	packs, err := packDistillCandidateMembersV2(members)
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, pack := range packs {
		total += len(pack.ProviderInput)
		if len(pack.ProviderInput) > distillCandidatePackMaxRenderedBytesV2 || len(pack.Members) > distillCandidatePackMaxMembersV2 {
			t.Fatalf("out-of-bounds pack: bytes=%d members=%d", len(pack.ProviderInput), len(pack.Members))
		}
	}
	if total < 1_300_000 || total > 1_500_000 {
		t.Fatalf("synthetic input = %d bytes, want roughly 1.4 MB", total)
	}
	if len(packs) < 30 || len(packs) > 60 {
		t.Fatalf("packs = %d, want 30..60 for %d cards / %d bytes", len(packs), cards, total)
	}
	if len(packs) != 46 {
		t.Fatalf("packs = %d, want deterministic 46", len(packs))
	}
	t.Logf("packed %d cards / %d bytes into %d bounded provider calls", cards, total, len(packs))
}

func packMemberV2(id, card string) distillCandidatePackMemberV2 {
	return distillCandidatePackMemberV2{
		CandidateID:  id,
		RenderedCard: card,
		Anchor: distillCandidateAnchorV1{
			CandidateID: id,
			SessionID:   "session-" + id,
			Transcript:  "sessions/" + id + ".jsonl",
			StartLine:   1,
			EndLine:     1,
		},
	}
}

func benchmarkCardIDV2(index int) string {
	return string([]byte{
		byte('a' + index/(26*26)),
		byte('a' + (index/26)%26),
		byte('a' + index%26),
	})
}

func assertPackV2(t *testing.T, pack distillCandidatePackV2, wantIDs []string, wantInput string) {
	t.Helper()
	if got := packIDsV2([]distillCandidatePackV2{pack}); !reflect.DeepEqual(got, wantIDs) {
		t.Errorf("member IDs = %v, want %v", got, wantIDs)
	}
	if pack.ProviderInput != wantInput {
		t.Errorf("provider input = %q, want %q", pack.ProviderInput, wantInput)
	}
}

func packIDsV2(packs []distillCandidatePackV2) []string {
	var ids []string
	for _, pack := range packs {
		for _, member := range pack.Members {
			ids = append(ids, member.CandidateID)
		}
	}
	return ids
}
