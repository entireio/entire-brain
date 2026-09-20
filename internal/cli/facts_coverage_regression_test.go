package cli

import (
	"strings"
	"testing"
	"time"
)

func TestManualFactUpdateDoesNotClaimDistillation(t *testing.T) {
	d := writeDistillFixture(t, time.Now())
	if err := updateFactSourceManifest(d, time.Now()); err != nil {
		t.Fatal(err)
	}
	note := emptyResultBlindSpot(d)
	if strings.Contains(note, "all captured sessions are distilled") || strings.Contains(note, "facts last distilled") {
		t.Fatalf("manual update implies distillation: %s", note)
	}
}

func TestPreserveDistillTokenUsage(t *testing.T) {
	previous := &factSourceManifest{TokenUsage: &distillTokenUsageSummary{Calls: 2}}
	source := &factSourceManifest{}
	preserveFactDistillEvidence(source, previous)
	if source.TokenUsage == nil || source.TokenUsage.Calls != 2 {
		t.Fatalf("token usage was discarded: %+v", source.TokenUsage)
	}
}

func TestRecentFactManifestDoesNotProveSessionCoverage(t *testing.T) {
	now := time.Now()
	d := writeDistillFixture(t, now)
	m, err := loadBrainManifest(d)
	if err != nil {
		t.Fatal(err)
	}
	m.Sources.Facts = &factSourceManifest{GeneratedAt: now, FailedChunks: 1, Branch: "feature"}
	if err := writeBrainManifestAndReadme(d, *m); err != nil {
		t.Fatal(err)
	}
	if note := emptyResultBlindSpot(d); strings.Contains(note, "all captured sessions are distilled") {
		t.Fatalf("partial run claimed complete coverage: %s", note)
	}
}

func TestManualUpdatePreservesDistillTimestampAndUsage(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	d := writeDistillFixture(t, now)
	m, err := loadBrainManifest(d)
	if err != nil {
		t.Fatal(err)
	}
	last := now.Add(-time.Hour)
	m.Sources.Facts = &factSourceManifest{GeneratedAt: last, LastDistilledAt: last, TokenUsage: &distillTokenUsageSummary{Calls: 2}}
	if err := writeBrainManifestAndReadme(d, *m); err != nil {
		t.Fatal(err)
	}
	if err := updateFactSourceManifest(d, now); err != nil {
		t.Fatal(err)
	}
	m, err = loadBrainManifest(d)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Sources.Facts.LastDistilledAt.Equal(last) || !m.Sources.Facts.GeneratedAt.Equal(now) || m.Sources.Facts.TokenUsage == nil || m.Sources.Facts.TokenUsage.Calls != 2 {
		t.Fatalf("manual update corrupted distillation evidence: %+v", m.Sources.Facts)
	}
}
