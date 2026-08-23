package cli

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func distillApplicationTestIdentityV2() distillApplicationIdentityV2 {
	return distillApplicationIdentityV2{
		CandidateID:         "candidate-v2:one",
		ResultID:            distillCandidateCacheDigestV2("parsed extraction result"),
		SourceSessionID:     "session-one",
		SourceDigest:        distillCandidateCacheDigestV2("canonical source"),
		Branch:              "feature/receipts",
		TranscriptPath:      "sessions/feature/receipts.jsonl",
		CheckpointID:        "checkpoint-17",
		SourceStartLine:     12,
		SourceEndLine:       19,
		FactStoreGeneration: distillCandidateCacheDigestV2("facts-generation-3"),
	}
}

func TestDistillApplicationReceiptsV2DeterministicRoundTripAndPrivacyBounded(t *testing.T) {
	first := newDistillApplicationReceiptStoreV2()
	second := newDistillApplicationReceiptStoreV2()
	identityA := distillApplicationTestIdentityV2()
	identityB := identityA
	identityB.CandidateID = "candidate-v2:two"
	identityB.ResultID = distillCandidateCacheDigestV2("second result")

	receiptA := distillApplicationReceiptV2{
		Identity: identityA, AppliedFactIDs: []string{"fact:one"},
		AppliedProvenanceDigest: distillCandidateCacheDigestV2("provenance one"),
	}
	var err error
	receiptA.ReceiptID, err = distillApplicationReceiptIDV2(identityA)
	if err != nil {
		t.Fatal(err)
	}
	receiptA.SlotID, err = distillApplicationSlotIDV2(identityA)
	if err != nil {
		t.Fatal(err)
	}
	receiptB := distillApplicationReceiptV2{
		Identity: identityB, AppliedProvenanceDigest: distillCandidateCacheDigestV2("empty provenance"),
	}
	receiptB.ReceiptID, err = distillApplicationReceiptIDV2(identityB)
	if err != nil {
		t.Fatal(err)
	}
	receiptB.SlotID, err = distillApplicationSlotIDV2(identityB)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.PutSuccess(receiptB); err != nil {
		t.Fatal(err)
	}
	if err := first.PutSuccess(receiptA); err != nil {
		t.Fatal(err)
	}
	if err := second.PutSuccess(receiptA); err != nil {
		t.Fatal(err)
	}
	if err := second.PutSuccess(receiptB); err != nil {
		t.Fatal(err)
	}
	dataA, err := marshalDistillApplicationReceiptStoreV2(first)
	if err != nil {
		t.Fatal(err)
	}
	dataB, err := marshalDistillApplicationReceiptStoreV2(second)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(dataA, dataB) {
		t.Fatalf("receipt serialization is not deterministic:\n%s\n---\n%s", dataA, dataB)
	}
	for _, forbidden := range []string{"raw provider output", "candidate card plaintext", "transcript body", "model response"} {
		if strings.Contains(string(dataA), forbidden) {
			t.Fatalf("receipt retained forbidden provider/card text %q: %s", forbidden, dataA)
		}
	}

	brainDir := t.TempDir()
	if err := saveDistillApplicationReceiptStoreV2(brainDir, first); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadDistillApplicationReceiptStoreV2(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := loaded.LookupSuccess(identityA)
	if !ok || got.ReceiptID != receiptA.ReceiptID || len(got.AppliedFactIDs) != 1 || got.AppliedFactIDs[0] != "fact:one" {
		t.Fatalf("round-trip receipt = %+v, hit=%v", got, ok)
	}
	if empty, ok := loaded.LookupSuccess(identityB); !ok || len(empty.AppliedFactIDs) != 0 {
		t.Fatalf("successful empty application was not retained: %+v, hit=%v", empty, ok)
	}
}

func TestDistillApplicationReceiptsV2ReplacementIdempotenceAndIdentity(t *testing.T) {
	store := newDistillApplicationReceiptStoreV2()
	identity := distillApplicationTestIdentityV2()
	first, err := store.RecordSuccess(identity, []string{"fact:old"}, distillCandidateCacheDigestV2("old provenance"))
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := store.RecordSuccess(identity, []string{"fact:new"}, distillCandidateCacheDigestV2("new provenance"))
	if err != nil {
		t.Fatal(err)
	}
	if replacement.ReceiptID != first.ReceiptID || len(store.entries) != 1 {
		t.Fatalf("same identity was not replaced idempotently: first=%+v replacement=%+v entries=%d", first, replacement, len(store.entries))
	}
	got, ok := store.LookupSuccess(identity)
	if !ok || len(got.AppliedFactIDs) != 1 || got.AppliedFactIDs[0] != "fact:new" {
		t.Fatalf("replacement lookup = %+v, hit=%v", got, ok)
	}
	changedResult := identity
	changedResult.ResultID = distillCandidateCacheDigestV2("changed parsed result")
	if _, err := store.RecordSuccess(changedResult, []string{"fact:replacement"}, distillCandidateCacheDigestV2("replacement provenance")); err != nil {
		t.Fatal(err)
	}
	if len(store.entries) != 1 {
		t.Fatalf("changed result accumulated an obsolete receipt: %d", len(store.entries))
	}
	if _, ok := store.LookupSuccess(identity); ok {
		t.Fatal("stale identity still hit after stable-slot replacement")
	}
	if replaced, ok := store.LookupSuccess(changedResult); !ok || replaced.AppliedFactIDs[0] != "fact:replacement" {
		t.Fatalf("changed result was not current: %+v, hit=%v", replaced, ok)
	}

	changes := []func(*distillApplicationIdentityV2){
		func(v *distillApplicationIdentityV2) { v.Branch = "main" },
		func(v *distillApplicationIdentityV2) { v.TranscriptPath = "sessions/main/receipts.jsonl" },
		func(v *distillApplicationIdentityV2) { v.CheckpointID = "checkpoint-18" },
		func(v *distillApplicationIdentityV2) { v.SourceSessionID = "session-two" },
		func(v *distillApplicationIdentityV2) { v.ResultID = distillCandidateCacheDigestV2("changed result") },
		func(v *distillApplicationIdentityV2) {
			v.FactStoreGeneration = distillCandidateCacheDigestV2("facts-generation-4")
		},
	}
	for _, change := range changes {
		changed := identity
		change(&changed)
		if _, ok := store.LookupSuccess(changed); ok {
			t.Fatalf("changed application identity incorrectly hit: %+v", changed)
		}
	}
	other := identity
	other.CandidateID = "candidate-v2:other"
	if _, err := store.RecordSuccess(other, []string{"fact:other"}, distillCandidateCacheDigestV2("other provenance")); err != nil {
		t.Fatal(err)
	}
	removed, err := store.PruneUnseenSlots(identity.Branch, identity.SourceSessionID, map[string]struct{}{other.CandidateID: {}})
	if err != nil || removed != 1 {
		t.Fatalf("prune unseen slots removed=%d err=%v", removed, err)
	}
	if _, ok := store.LookupSuccess(other); !ok {
		t.Fatal("prune removed a current candidate slot")
	}
}

func TestDistillApplicationReceiptsV2CanonicalizesAppliedFactIDs(t *testing.T) {
	store := newDistillApplicationReceiptStoreV2()
	identity := distillApplicationTestIdentityV2()
	receipt, err := store.RecordSuccess(identity, []string{"fact:z", "fact:a"}, distillCandidateCacheDigestV2("provenance"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(receipt.AppliedFactIDs, ","); got != "fact:a,fact:z" {
		t.Fatalf("canonical fact IDs = %q", got)
	}
}

func TestDistillApplicationReceiptsV2StrictParsing(t *testing.T) {
	identity := distillApplicationTestIdentityV2()
	store := newDistillApplicationReceiptStoreV2()
	if _, err := store.RecordSuccess(identity, nil, distillCandidateCacheDigestV2("provenance")); err != nil {
		t.Fatal(err)
	}
	valid, err := marshalDistillApplicationReceiptStoreV2(store)
	if err != nil {
		t.Fatal(err)
	}
	newline := bytes.IndexByte(valid, '\n')
	if newline < 0 {
		t.Fatal("missing receipt header")
	}
	tests := map[string][]byte{
		"unknown header field": append([]byte(`{"type":"header","version":1,"unknown":true}`+"\n"), valid[newline+1:]...),
		"unknown record field": bytes.Replace(valid, []byte(`"receipt":{`), []byte(`"receipt":{"unknown":true,`), 1),
		"duplicate header key": append([]byte(`{"type":"header","version":1,"version":1}`+"\n"), valid[newline+1:]...),
		"noncanonical spacing": append([]byte(`{"type": "header","version":1}`+"\n"), valid[newline+1:]...),
		"trailing JSON":        append(append([]byte(nil), valid...), []byte(`{"type":"extra"}`+"\n")...),
		"missing header":       valid[newline+1:],
		"blank line":           append(append([]byte(nil), valid...), '\n'),
	}
	for name, data := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := parseDistillApplicationReceiptStoreV2(data); err == nil {
				t.Fatalf("invalid receipt document was accepted: %s", data)
			}
		})
	}

	bad := identity
	bad.TranscriptPath = "../outside.jsonl"
	badReceiptID, err := distillApplicationReceiptIDV2(identity)
	if err != nil {
		t.Fatal(err)
	}
	// Keep the valid receipt ID to ensure validation checks both identity and
	// path rather than merely trusting a caller-provided key.
	badReceipt := distillApplicationReceiptV2{ReceiptID: badReceiptID, Identity: bad, AppliedProvenanceDigest: distillCandidateCacheDigestV2("provenance")}
	if err := store.PutSuccess(badReceipt); err == nil {
		t.Fatal("unsafe transcript path was accepted")
	}
}

func TestDistillApplicationReceiptsV2Bounds(t *testing.T) {
	store := newDistillApplicationReceiptStoreV2()
	for i := 0; i < distillApplicationReceiptsV2MaxEntries+1; i++ {
		identity := distillApplicationTestIdentityV2()
		identity.CandidateID = fmt.Sprintf("candidate-v2:%d", i)
		identity.ResultID = distillCandidateCacheDigestV2(fmt.Sprintf("result-%d", i))
		id, err := distillApplicationReceiptIDV2(identity)
		if err != nil {
			t.Fatal(err)
		}
		receipt := distillApplicationReceiptV2{ReceiptID: id, Identity: identity, AppliedProvenanceDigest: distillCandidateCacheDigestV2("provenance")}
		receipt.SlotID, err = distillApplicationSlotIDV2(identity)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.PutSuccess(receipt); i < distillApplicationReceiptsV2MaxEntries && err != nil {
			t.Fatalf("entry %d rejected before bound: %v", i, err)
		} else if i == distillApplicationReceiptsV2MaxEntries && err == nil {
			t.Fatal("entry beyond maximum was accepted")
		}
	}
	if _, err := marshalDistillApplicationReceiptStoreV2(store); err != nil {
		t.Fatalf("bounded store failed to marshal: %v", err)
	}
	if _, err := parseDistillApplicationReceiptStoreV2([]byte(strings.Repeat("x", distillApplicationReceiptsV2MaxBytes+1))); err == nil {
		t.Fatal("oversized receipt document was accepted")
	}

	invalid := distillApplicationTestIdentityV2()
	invalid.SourceEndLine = distillApplicationReceiptsV2MaxLineNo + 1
	if _, err := distillApplicationReceiptIDV2(invalid); err == nil {
		t.Fatal("out-of-bounds source line was accepted")
	}
}
