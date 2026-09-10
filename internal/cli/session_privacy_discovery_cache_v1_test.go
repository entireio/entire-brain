package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPurgeDistillDiscoveryCacheV1RemovesStateAndAtomicTempsAreClassified(t *testing.T) {
	root := t.TempDir()
	cache := newDistillDiscoveryCacheV1()
	cache.Entries["session-private"] = distillDiscoveryCacheEntryV1{SourceSHA256: distillDiscoveryDigestV1("source"), EvidenceSHA256: distillDiscoveryDigestV1("evidence")}
	if err := saveDistillDiscoveryCacheV1(root, cache); err != nil {
		t.Fatal(err)
	}
	if err := purgeDistillDiscoveryCacheV1(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(distillDiscoveryCacheRelV1))); !os.IsNotExist(err) {
		t.Fatalf("discovery cache survived purge: %v", err)
	}
	if !isCandidateAtomicTempArtifactRel("facts/distill-v2/.discovery.json.tmp-Test123") {
		t.Fatal("discovery atomic temp is not covered by privacy cleanup")
	}
}

func TestPrivacyDiscoveryCacheLoaderFailsClosedOnOpaqueState(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, filepath.FromSlash(distillDiscoveryCacheRelV1))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"version":1,"candidate_schema":4,"entries":{},"opaque":"private"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadDistillDiscoveryCacheV1ForPrivacy(root); err == nil {
		t.Fatal("privacy loader accepted opaque discovery state")
	}
}

func TestPrivacyDiscoveryCacheLoaderRejectsTrailingOpaqueData(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, filepath.FromSlash(distillDiscoveryCacheRelV1))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	data := `{"version":1,"candidate_schema":4,"entries":{}} {"opaque":"private"}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadDistillDiscoveryCacheV1ForPrivacy(root); err == nil {
		t.Fatal("privacy loader accepted trailing opaque discovery data")
	}
}

func TestPrivacyVerifyFindsTombstonedDiscoveryEntry(t *testing.T) {
	brainDir := writePrivacyFixture(t)
	tombstonePrivacyFixture(t, brainDir)
	cache := newDistillDiscoveryCacheV1()
	cache.Entries["secret-sess"] = distillDiscoveryCacheEntryV1{SourceSHA256: distillDiscoveryDigestV1("source"), EvidenceSHA256: distillDiscoveryDigestV1("evidence")}
	if err := saveDistillDiscoveryCacheV1(brainDir, cache); err != nil {
		t.Fatal(err)
	}
	report, err := verifySessionPrivacy(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, finding := range report.Findings {
		if finding.SessionID == "secret-sess" && finding.Artifact == "distill_discovery_cache" {
			return
		}
	}
	t.Fatalf("privacy verify missed tombstoned discovery entry: %+v", report.Findings)
}

func TestPrivacyPurgePlanAndReceiptIncludeDiscoveryReset(t *testing.T) {
	brainDir := writePrivacyFixture(t)
	cache := newDistillDiscoveryCacheV1()
	cache.Entries["secret-sess"] = distillDiscoveryCacheEntryV1{SourceSHA256: distillDiscoveryDigestV1("source"), EvidenceSHA256: distillDiscoveryDigestV1("evidence")}
	if err := saveDistillDiscoveryCacheV1(brainDir, cache); err != nil {
		t.Fatal(err)
	}
	plan, err := buildSessionPurgePlan(brainDir, "secret-sess")
	if err != nil {
		t.Fatal(err)
	}
	if plan.DistillDiscoveryCacheV1Reset == nil || plan.DistillDiscoveryCacheV1Reset.Path != distillDiscoveryCacheRelV1 {
		t.Fatalf("purge plan omitted discovery reset: %+v", plan)
	}
	if err := executeSessionPurge(brainDir, "secret-sess", plan, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	tx, present, err := loadPrivacyTransactionChecked(brainDir, "secret-sess")
	if err != nil || !present {
		t.Fatalf("load transaction: present=%v err=%v", present, err)
	}
	for _, artifact := range tx.Artifacts {
		if artifact.Path == distillDiscoveryCacheRelV1 {
			return
		}
	}
	t.Fatalf("privacy receipt omitted planned discovery reset: %+v", tx.Artifacts)
}
