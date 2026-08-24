package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ashtom/entire-brain/internal/factmerge"
)

func writePrivacyRelationshipStore(t *testing.T, brainDir, sessionID string) {
	t.Helper()
	left := factmerge.Record{ID: "fact:left", Branch: "main", Status: factmerge.StatusActive, Kind: "invariant", Paths: []string{"architecture.data.flow"}, Locus: []string{"internal/cli/graph.go"}}
	right := factmerge.Record{ID: "fact:right", Branch: "main", Status: factmerge.StatusActive, Kind: "invariant", Paths: []string{"architecture.cache.policy"}, Locus: []string{"internal/cli/graph.go"}}
	proposal, ok, err := factmerge.BuildPossibleSameSubject(left, right, []factmerge.RelationshipOwner{{CandidateID: "candidate:v2", SourceSessionID: sessionID}})
	if err != nil || !ok {
		t.Fatalf("build relationship = %+v, ok=%v, err=%v", proposal, ok, err)
	}
	store := newDistillRelationshipStoreV2()
	if err := store.Put(proposal); err != nil {
		t.Fatal(err)
	}
	if err := saveDistillRelationshipStoreV2(brainDir, store); err != nil {
		t.Fatal(err)
	}
}

func TestPrivacyOwnsRelationshipStoreAndTransactionArtifact(t *testing.T) {
	brainDir := writePrivacyFixture(t)
	writePrivacyRelationshipStore(t, brainDir, "secret-sess")
	plan, err := buildSessionPurgePlan(brainDir, "secret-sess")
	if err != nil {
		t.Fatal(err)
	}
	if reset := plan.DistillRelationshipStoreV2Reset; reset == nil || reset.Path != distillRelationshipStoreV2Path || reset.Bytes <= 0 {
		t.Fatalf("purge plan did not disclose relationship reset: %+v", reset)
	}
	if err := executeSessionPurge(brainDir, "secret-sess", plan, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	store, err := loadDistillRelationshipStoreV2ForPrivacy(brainDir)
	if err != nil || len(store.entries) != 0 {
		t.Fatalf("privacy purge retained relationship observations: %+v, %v", store.entries, err)
	}
	tx, present, err := loadPrivacyTransactionChecked(brainDir, "secret-sess")
	if err != nil || !present {
		t.Fatalf("privacy transaction missing after purge: %+v, %v, present=%v", tx, err, present)
	}
	found := false
	for _, artifact := range tx.Artifacts {
		if artifact.Path == distillRelationshipStoreV2Path {
			found = true
		}
	}
	if !found {
		t.Fatalf("transaction omitted relationship reset: %+v", tx.Artifacts)
	}
}

func TestPrivacyVerifyFindsTombstonedRelationshipOwner(t *testing.T) {
	brainDir := writePrivacyFixture(t)
	tombstonePrivacyFixture(t, brainDir)
	writePrivacyRelationshipStore(t, brainDir, "secret-sess")
	report, err := verifySessionPrivacy(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, finding := range report.Findings {
		if finding.SessionID == "secret-sess" && finding.Artifact == "distill_relationship_store_v2" {
			return
		}
	}
	t.Fatalf("privacy verify missed tombstoned relationship owner: %+v", report.Findings)
}

func TestPrivacyRelationshipVerifyFailsClosedButPurgeResetsOpaqueAndOversized(t *testing.T) {
	for name, payload := range map[string][]byte{
		"opaque":   []byte("relationship provider bytes\n"),
		"corrupt":  []byte(`{"type":"header","version":999}` + "\n"),
		"oversize": nil,
	} {
		t.Run(name, func(t *testing.T) {
			brainDir := writePrivacyFixture(t)
			tombstonePrivacyFixture(t, brainDir)
			path := filepath.Join(brainDir, filepath.FromSlash(distillRelationshipStoreV2Path))
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if name == "oversize" {
				f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
				if err != nil {
					t.Fatal(err)
				}
				if err := f.Truncate(distillRelationshipStoreV2MaxBytes + 1); err != nil {
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
				t.Fatalf("privacy verify accepted invalid relationship state: %v", err)
			}
			if err := purgeDistillRelationshipStoreV2(brainDir); err != nil {
				t.Fatal(err)
			}
			store, err := loadDistillRelationshipStoreV2ForPrivacy(brainDir)
			if err != nil || len(store.entries) != 0 {
				t.Fatalf("purge did not reset invalid relationships: %+v, %v", store.entries, err)
			}
		})
	}
}

func TestPrivacyRelationshipAtomicTempOrphanIsInventoriedAndRemoved(t *testing.T) {
	brainDir := writePrivacyFixture(t)
	tombstonePrivacyFixture(t, brainDir)
	rel := "facts/distill-v2/.relationships.ndjson.tmp-Relationship123"
	path := filepath.Join(brainDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("opaque relationship staging"), 0o600); err != nil {
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
		t.Fatalf("purge plan omitted relationship atomic orphan: %+v", plan.DerivedStores)
	}
	if err := executeSessionPurge(brainDir, "secret-sess", plan, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("relationship atomic orphan survived purge: %v", err)
	}
}
