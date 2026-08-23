package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writePrivacyApplicationReceipt(t *testing.T, brainDir, sessionID string) {
	t.Helper()
	store := newDistillApplicationReceiptStoreV2()
	identity := distillApplicationTestIdentityV2()
	identity.SourceSessionID = sessionID
	if _, err := store.RecordSuccess(identity, []string{"fact:privacy"}, distillCandidateCacheDigestV2("privacy provenance")); err != nil {
		t.Fatal(err)
	}
	if err := saveDistillApplicationReceiptStoreV2(brainDir, store); err != nil {
		t.Fatal(err)
	}
}

func TestPrivacyOwnsApplicationReceiptStoreV2(t *testing.T) {
	brainDir := writePrivacyFixture(t)
	writePrivacyApplicationReceipt(t, brainDir, "secret-sess")
	plan, err := buildSessionPurgePlan(brainDir, "secret-sess")
	if err != nil {
		t.Fatal(err)
	}
	if reset := plan.DistillApplicationReceiptsV2Reset; reset == nil || reset.Path != distillApplicationReceiptsV2Path || reset.Bytes <= 0 {
		t.Fatalf("purge plan did not disclose receipt reset: %+v", reset)
	}
	if err := executeSessionPurge(brainDir, "secret-sess", plan, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	got, err := loadDistillApplicationReceiptStoreV2ForPrivacy(brainDir)
	if err != nil || len(got.entries) != 0 {
		t.Fatalf("privacy purge retained application receipts: %+v, %v", got.entries, err)
	}
}

func TestPrivacyVerifyFindsTombstonedApplicationReceiptOwner(t *testing.T) {
	brainDir := writePrivacyFixture(t)
	tombstonePrivacyFixture(t, brainDir)
	writePrivacyApplicationReceipt(t, brainDir, "secret-sess")
	report, err := verifySessionPrivacy(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, finding := range report.Findings {
		if finding.SessionID == "secret-sess" && finding.Artifact == "distill_application_receipts_v2" {
			return
		}
	}
	t.Fatalf("privacy verify missed tombstoned application receipt: %+v", report.Findings)
}

func TestPrivacyReceiptVerifyFailsClosedButPurgeResetsOpaqueAndOversized(t *testing.T) {
	for name, payload := range map[string][]byte{
		"opaque":   []byte("provider/application receipt bytes\n"),
		"corrupt":  []byte(`{"type":"header","version":999}` + "\n"),
		"oversize": nil,
	} {
		t.Run(name, func(t *testing.T) {
			brainDir := writePrivacyFixture(t)
			tombstonePrivacyFixture(t, brainDir)
			path := filepath.Join(brainDir, filepath.FromSlash(distillApplicationReceiptsV2Path))
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if name == "oversize" {
				f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
				if err != nil {
					t.Fatal(err)
				}
				if err := f.Truncate(distillApplicationReceiptsV2MaxBytes + 1); err != nil {
					_ = f.Close()
					t.Fatal(err)
				}
				if err := f.Close(); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(path, payload, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := verifySessionPrivacy(brainDir); err == nil || !strings.Contains(err.Error(), memoryErrStateCorrupt) {
				t.Fatalf("privacy verify accepted invalid receipt state: %v", err)
			}
			if err := purgeDistillApplicationReceiptsV2(brainDir); err != nil {
				t.Fatal(err)
			}
			store, err := loadDistillApplicationReceiptStoreV2ForPrivacy(brainDir)
			if err != nil || len(store.entries) != 0 {
				t.Fatalf("purge did not reset invalid receipts: %+v, %v", store.entries, err)
			}
		})
	}
}

func TestPrivacyApplicationReceiptAtomicTempOrphanIsInventoriedAndRemoved(t *testing.T) {
	brainDir := writePrivacyFixture(t)
	tombstonePrivacyFixture(t, brainDir)
	rel := "facts/distill-v2/.applications.ndjson.tmp-Receipt123"
	path := filepath.Join(brainDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("opaque receipt staging"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := buildSessionPurgePlan(brainDir, "secret-sess")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, artifact := range plan.DerivedStores {
		if artifact.Path == rel {
			found = true
		}
	}
	if !found {
		t.Fatalf("purge plan omitted receipt atomic orphan: %+v", plan.DerivedStores)
	}
	if err := executeSessionPurge(brainDir, "secret-sess", plan, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("receipt atomic orphan survived purge: %v", err)
	}
}

func TestPrivacyPurgePreservesFactWithOnlySharedV2Anchor(t *testing.T) {
	brainDir := writePrivacyFixture(t)
	candidateID := distillCandidateStableIDV1(distillCandidateIDPrefixV1, "shared-legacy")
	paths := []string{"workflow.testing.rules"}
	fact := factRecord{
		ID: factRecordID("Always preserve the legacy rule.", paths), Paths: paths,
		Kind: factKindConvention, Text: "Always preserve the legacy rule.", Branch: "main",
		Origin: factOriginDistilled, Status: factStatusActive,
		Provenance: []factAnchor{{
			SessionID: "secret-sess", Transcript: "sessions/main/secret.jsonl", Line: 1,
			DistillTurnID: distillCandidateApplicationAnchorIDV2(candidateID, false),
		}},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := writeFacts(brainDir, "main", []factRecord{fact}); err != nil {
		t.Fatal(err)
	}
	plan, err := buildSessionPurgePlan(brainDir, "secret-sess")
	if err != nil {
		t.Fatal(err)
	}
	if plan.FactsDeleted != 0 || plan.FactAnchorsStripped != 1 {
		t.Fatalf("shared-v2 purge plan = deleted:%d stripped:%d", plan.FactsDeleted, plan.FactAnchorsStripped)
	}
	if err := executeSessionPurge(brainDir, "secret-sess", plan, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	facts, err := loadFacts(brainDir, "main")
	if err != nil || len(facts) != 1 || facts[0].ID != fact.ID || len(facts[0].Provenance) != 0 {
		t.Fatalf("privacy purge removed shared legacy fact: %+v err=%v", facts, err)
	}
}
