package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeAbstractor struct {
	fabricate bool
	hosted    bool
}

func (f *fakeAbstractor) Identity() (string, string, string) {
	kind := "local"
	if f.hosted {
		kind = "hosted"
	}
	return kind, "fake", "fake-1"
}

func (f *fakeAbstractor) Abstract(input conversationAbstractInput) (sessionAbstract, error) {
	var artifact sessionAbstract
	evidence := input.Windows[0][0].ConversationID
	if f.fabricate {
		evidence = "conversation:fabricated"
	}
	artifact.Overview = abstractStatement{Text: "session walked five staged failures", EvidenceIDs: []string{evidence}}
	artifact.Decisions = []abstractStatement{{Text: "the flag parser was the culprit", EvidenceIDs: []string{evidence}}}
	return artifact, nil
}

func sessionViewForTest(t *testing.T, brainDir string) conversationSessionView {
	t.Helper()
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := loadFreshHistory(brainDir, manifest.Sources.History)
	if err != nil {
		t.Fatal(err)
	}
	views := buildConversationSessionViews(fresh, manifest.RepoKey, manifest, loadSessionReadGuard(brainDir, manifest))
	if len(views) != 1 {
		t.Fatalf("views = %d, want 1", len(views))
	}
	for _, view := range views {
		return view
	}
	panic("unreachable")
}

func TestSessionAbstractLifecycle(t *testing.T) {
	brainDir, _ := writeSessionNavigationFixture(t)
	now := time.Date(2026, 8, 8, 16, 0, 0, 0, time.UTC)
	view := sessionViewForTest(t, brainDir)

	// Disabled (the default): status only, never a generation call.
	if status, artifact := sessionAbstractStatus(brainDir, view); status != abstractStatusDisabled || artifact != nil {
		t.Fatalf("default status = %s %v, want disabled", status, artifact)
	}
	// Enabled with no provider available: structured provider_unavailable.
	config := loadMemoryConfig(brainDir)
	config.Abstracts.Enabled = true
	config.Abstracts.Provider = "fake"
	if err := saveMemoryConfig(brainDir, config); err != nil {
		t.Fatal(err)
	}
	if memoryAbstractorFactory != nil {
		t.Fatal("default build must ship no abstract provider")
	}
	if _, err := generateSessionAbstract(brainDir, view, config, now); err == nil || !strings.Contains(err.Error(), memoryErrProviderUnavail) {
		t.Fatalf("no-provider generation must be provider_unavailable: %v", err)
	}
	if status, _ := sessionAbstractStatus(brainDir, view); status != abstractStatusUnavailable {
		t.Fatalf("status = %s, want provider_unavailable", status)
	}

	// A registered fake provider generates a validated, evidence-linked
	// artifact; a hosted provider without the egress acknowledgement is
	// refused.
	oldFactory := memoryAbstractorFactory
	defer func() { memoryAbstractorFactory = oldFactory }()
	memoryAbstractorFactory = func(memoryAbstractsConfig) (ConversationAbstractor, error) {
		return &fakeAbstractor{hosted: true}, nil
	}
	if _, err := generateSessionAbstract(brainDir, view, config, now); err == nil || !strings.Contains(err.Error(), "hosted egress") {
		t.Fatalf("hosted provider without egress ack must be refused: %v", err)
	}
	memoryAbstractorFactory = func(memoryAbstractsConfig) (ConversationAbstractor, error) {
		return &fakeAbstractor{}, nil
	}
	artifact, err := generateSessionAbstract(brainDir, view, config, now)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if artifact.SessionRef != view.Ref || artifact.Generator.Provider != "fake" || artifact.Coverage.TotalTurns != 5 {
		t.Fatalf("artifact identity: %+v", artifact)
	}
	status, current := sessionAbstractStatus(brainDir, view)
	if status != abstractStatusCurrent || current == nil {
		t.Fatalf("status after generation = %s", status)
	}

	// The session outline carries the status without generating anything.
	found, _, err := getUnifiedBatchOptions("", brainDir, "main", []string{view.Ref}, getOptions{})
	if err != nil || len(found) != 1 {
		t.Fatalf("outline get: %v", err)
	}
	if found[0].AbstractStatus != abstractStatusCurrent || found[0].Abstract == nil {
		t.Fatalf("outline abstract status: %+v", found[0].AbstractStatus)
	}

	// Fabricated citations are discarded, never stored.
	memoryAbstractorFactory = func(memoryAbstractsConfig) (ConversationAbstractor, error) {
		return &fakeAbstractor{fabricate: true}, nil
	}
	stale := view
	if _, err := generateSessionAbstract(brainDir, stale, config, now.Add(time.Minute)); err == nil || !strings.Contains(err.Error(), "not an exchange of session") {
		t.Fatalf("fabricated citation must be discarded: %v", err)
	}

	// A source append changes the session digest: the stored artifact reads
	// stale, with no text served.
	rel := "sessions/main/20260808T090000Z_nav.jsonl"
	full := filepath.Join(brainDir, filepath.FromSlash(rel))
	data, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	extra := `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"step 6: appended"}]}}` + "\n" +
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Decision 6: appended."}]}}` + "\n"
	if err := os.WriteFile(full, append(data, []byte(extra)...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := writeBrainHistoryIndexAndSource(brainDir, now.Add(time.Hour), nil); err != nil {
		t.Fatal(err)
	}
	newView := sessionViewForTest(t, brainDir)
	if status, artifact := sessionAbstractStatus(brainDir, newView); status != abstractStatusStale || artifact != nil {
		t.Fatalf("post-append status = %s %v, want stale with no text", status, artifact)
	}

	// Privacy: excluding the session removes its artifacts, and verify flags
	// a survivor.
	if err := withBrainWriteLock(brainDir, func() error {
		plan, planErr := buildSessionPurgePlan(brainDir, "nav-sess")
		if planErr != nil {
			return planErr
		}
		return executeSessionCleanup(brainDir, "nav-sess", plan, now.Add(2*time.Hour), "test", true)
	}); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(filepath.Join(brainDir, filepath.FromSlash(abstractsDirRel)))
	if len(entries) != 0 {
		t.Fatalf("cleanup must remove the session's abstracts: %d left", len(entries))
	}
}

func TestMemoryMigrateDetectsAndRepairsOldOverlay(t *testing.T) {
	brainDir, _, _ := shortTermFixture(t)
	buildShortTerm(t, brainDir)
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	overlay, state := loadHistoryShortTermState(brainDir, manifest.Sources.History)
	if state != shortTermStateCurrent {
		t.Fatalf("overlay state = %s", state)
	}
	overlay.ReconcilerVersion = historyShortTermReconcilerVersion - 1
	if err := saveHistoryShortTerm(brainDir, overlay); err != nil {
		t.Fatal(err)
	}
	findings := detectMemoryMigrations(brainDir, manifest.Sources.History)
	foundOverlay := false
	for _, finding := range findings {
		if finding.Path == historyShortTermPath && finding.State == memoryErrMigrationRequired {
			foundOverlay = true
		}
	}
	if !foundOverlay {
		t.Fatalf("migrate must flag the old-reconciler overlay: %+v", findings)
	}
	// The migration action (delta rebuild) restores a current overlay.
	if err := withBrainWriteLock(brainDir, func() error {
		_, err := buildHistoryShortTermLocked(brainDir, time.Date(2026, 8, 8, 17, 0, 0, 0, time.UTC))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, state := loadHistoryShortTermState(brainDir, manifest.Sources.History); state != shortTermStateCurrent {
		t.Fatalf("post-migration overlay state = %s", state)
	}
}

// TestProjectionReceiptsCommitThroughManifest is the R0.4 kill-boundary
// proof for the receipt publication: a receipt file written without the
// manifest's digest commit (the crash window) is never trusted, so readers
// stay on the previous receipt set and jobs re-enqueue.
func TestProjectionReceiptsCommitThroughManifest(t *testing.T) {
	brainDir, _, _ := shortTermFixture(t)
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := loadProjectionState(brainDir, manifest.Sources.History); !ok {
		t.Fatal("fixture full build must publish receipts")
	}
	// Simulate the crash: a newer receipt file lands but the manifest commit
	// never happened.
	state := projectionState{SchemaVersion: projectionSchemaVersion, ReconcilerVersion: projectionReconcilerVersion, GeneratedAt: time.Now().UTC()}
	if _, err := writeProjectionState(brainDir, state); err != nil {
		t.Fatal(err)
	}
	if _, ok := loadProjectionState(brainDir, manifest.Sources.History); ok {
		t.Fatal("an uncommitted receipt file must not be trusted")
	}
}
