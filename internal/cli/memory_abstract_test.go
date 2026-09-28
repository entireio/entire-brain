package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestConfiguredConversationAbstractorsAreExplicitAndBounded(t *testing.T) {
	oldAvailable := abstractExecutableAvailable
	abstractExecutableAvailable = func(string) bool { return true }
	defer func() { abstractExecutableAvailable = oldAvailable }()
	if _, err := newConfiguredConversationAbstractor(memoryAbstractsConfig{Provider: ""}); err == nil {
		t.Fatal("empty provider must not auto-select an agent")
	}
	if _, err := newConfiguredConversationAbstractor(memoryAbstractsConfig{Provider: "ollama"}); err == nil {
		t.Fatal("ollama requires an explicit model")
	}
	resolved, err := newConfiguredConversationAbstractor(memoryAbstractsConfig{Provider: "codex", Model: "test-model"})
	if err != nil {
		t.Fatal(err)
	}
	agent := resolved.(*agentConversationAbstractor)
	if kind, provider, model := agent.Identity(); kind != "hosted" || provider != "codex" || model != "test-model" {
		t.Fatalf("identity = %s %s %s", kind, provider, model)
	}
	defaultResolved, err := newConfiguredConversationAbstractor(memoryAbstractsConfig{Provider: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	defaultAgent := defaultResolved.(*agentConversationAbstractor)
	defaultAgent.run = func(_ context.Context, _ string, args []string, _ []byte, _ time.Duration) (string, error) {
		for _, arg := range args {
			if arg == "provider-default" {
				t.Fatalf("display-only model identity leaked into command argv: %v", args)
			}
		}
		return `{"overview":{}}`, nil
	}
	if _, err := defaultAgent.Abstract(context.Background(), t.TempDir(), conversationAbstractInput{}); err != nil {
		t.Fatal(err)
	}
	called := 0
	agent.run = func(_ context.Context, _ string, args []string, input []byte, timeout time.Duration) (string, error) {
		called++
		if timeout != abstractProviderTimeout || len(input) == 0 || len(args) == 0 || args[0] != "codex" {
			t.Fatalf("runner contract: timeout=%s args=%v input=%d", timeout, args, len(input))
		}
		return `{"overview":{"text":"bounded overview","evidence_ids":["conversation:e1"]}}`, nil
	}
	artifact, err := agent.Abstract(context.Background(), t.TempDir(), conversationAbstractInput{SessionRef: "conversation-session:s", Windows: [][]conversationTurn{{{ConversationID: "conversation:e1"}}}})
	if err != nil || artifact.Overview.Text != "bounded overview" || called != 1 {
		t.Fatalf("abstract = %+v calls=%d err=%v", artifact, called, err)
	}
}

func TestConfiguredProviderReportsMissingExecutable(t *testing.T) {
	oldAvailable := abstractExecutableAvailable
	abstractExecutableAvailable = func(string) bool { return false }
	defer func() { abstractExecutableAvailable = oldAvailable }()
	_, err := newConfiguredConversationAbstractor(memoryAbstractsConfig{Provider: "codex"})
	if err == nil || !strings.Contains(err.Error(), "executable") {
		t.Fatalf("missing executable error = %v", err)
	}
	_, err = newConfiguredConversationAbstractor(memoryAbstractsConfig{Provider: "ollama", Model: "local-model"})
	if err == nil || !strings.Contains(err.Error(), "executable") {
		t.Fatalf("missing ollama executable error = %v", err)
	}
	brainDir := t.TempDir()
	if err := saveMemoryConfig(brainDir, memoryConfig{Abstracts: memoryAbstractsConfig{Enabled: true, Provider: "codex"}}); err != nil {
		t.Fatal(err)
	}
	if summary := memoryAbstractProviderSummary(brainDir); !strings.Contains(summary, memoryErrProviderUnavail) || !strings.Contains(summary, "executable") {
		t.Fatalf("provider health summary = %q", summary)
	}
}

func TestLongAbstractUsesValidatedWindowSynthesis(t *testing.T) {
	agent := &agentConversationAbstractor{kind: "hosted", provider: "codex", run: nil}
	calls := 0
	agent.run = func(_ context.Context, _ string, _ []string, _ []byte, _ time.Duration) (string, error) {
		calls++
		switch calls {
		case 1:
			return `{"overview":{"text":"first","evidence_ids":["conversation:e1"]}}`, nil
		case 2:
			return `{"overview":{"text":"second","evidence_ids":["conversation:e2"]}}`, nil
		default:
			return `{"overview":{"text":"combined","evidence_ids":["conversation:e1","conversation:e2"]}}`, nil
		}
	}
	artifact, err := agent.Abstract(context.Background(), t.TempDir(), conversationAbstractInput{Windows: [][]conversationTurn{
		{{ConversationID: "conversation:e1", TurnOrdinal: 1}},
		{{ConversationID: "conversation:e2", TurnOrdinal: 2}},
	}})
	if err != nil || calls != 3 || artifact.Overview.Text != "combined" {
		t.Fatalf("artifact=%+v calls=%d err=%v", artifact, calls, err)
	}
}

func TestMemoryConfigCorruptionIsNotDisabled(t *testing.T) {
	brainDir := t.TempDir()
	path := filepath.Join(brainDir, filepath.FromSlash(memoryConfigRel))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, state, err := loadMemoryConfigChecked(brainDir)
	if state != memoryConfigCorrupt || err == nil || !strings.Contains(err.Error(), memoryErrStateCorrupt) {
		t.Fatalf("state=%s err=%v", state, err)
	}
	health := memoryAbstractHealth(brainDir)
	if health["config_state"] != memoryConfigCorrupt || health["error_code"] != memoryConfigCorrupt {
		t.Fatalf("health = %+v", health)
	}
}

func TestMemoryInstallHealthIsReadOnly(t *testing.T) {
	brainDir := t.TempDir()
	_ = memoryInstallHealth(brainDir)
	if _, err := os.Stat(filepath.Join(brainDir, filepath.FromSlash(memoryWorkDirRel))); !os.IsNotExist(err) {
		t.Fatalf("status created or altered the work directory: %v", err)
	}
}

func TestAbstractRetrievalOptionParity(t *testing.T) {
	cmd := newSearchCommand(Options{})
	if cmd.Flags().Lookup("include-abstract") == nil {
		t.Fatal("CLI retrieval must expose --include-abstract")
	}
	ropts, err := mcpRetrievalOptions(map[string]any{"source": "conversation", "include_abstract": true}, "")
	if err != nil || !ropts.IncludeAbstract {
		t.Fatalf("MCP options = %+v err=%v", ropts, err)
	}
	if _, err := retrieveUnifiedWithOptions("", t.TempDir(), "", "q", 1, modeLexical, retrievalOptions{Source: retrievalSourceFact, IncludeAbstract: true}); err == nil || !strings.Contains(err.Error(), "include_abstract") {
		t.Fatalf("non-conversation include_abstract must fail, got %v", err)
	}
}

func TestSessionAbstractEnvelopeIsBounded(t *testing.T) {
	result := unifiedResult{ID: "conversation-session:test", SessionRef: "conversation-session:test", Abstract: &sessionAbstract{
		Overview: abstractStatement{Text: strings.Repeat("a", abstractOverviewMax), EvidenceIDs: []string{"conversation:e"}},
	}}
	for i := 0; i < 50; i++ {
		result.Turns = append(result.Turns, conversationTurn{TurnOrdinal: i + 1, Text: strings.Repeat("x", 2048)})
	}
	result = boundSessionAbstractEnvelope(result)
	data, err := json.Marshal(result)
	if err != nil || len(data) > abstractSessionEnvelopeMax || !result.PacketTruncated {
		t.Fatalf("envelope bytes=%d truncated=%v err=%v", len(data), result.PacketTruncated, err)
	}
}

func TestAbstractGuardPreventsCommit(t *testing.T) {
	brainDir, _ := writeSessionNavigationFixture(t)
	view := sessionViewForTest(t, brainDir)
	oldFactory := memoryAbstractorFactory
	defer func() { memoryAbstractorFactory = oldFactory }()
	memoryAbstractorFactory = func(memoryAbstractsConfig) (ConversationAbstractor, error) { return &fakeAbstractor{}, nil }
	config := memoryConfig{SchemaVersion: memoryConfigSchemaVersion, Abstracts: memoryAbstractsConfig{Enabled: true, Provider: "fake"}}
	guardCalled := false
	_, err := generateSessionAbstractContextGuarded(context.Background(), t.TempDir(), brainDir, view, config, time.Now(), func() error {
		guardCalled = true
		return errors.New(memoryErrCancelled)
	})
	if err == nil || !strings.Contains(err.Error(), memoryErrCancelled) || !guardCalled {
		t.Fatalf("guard err=%v called=%v", err, guardCalled)
	}
	if _, ok := loadSessionAbstract(brainDir, sessionViewDigest(view)); ok {
		t.Fatal("cancelled pre-commit guard must not publish an abstract")
	}
}

func TestHostedAbstractLinearizesProviderReceiptAndArtifactBeforePrivacyCleanup(t *testing.T) {
	brainDir, _ := writeSessionNavigationFixture(t)
	view := sessionViewForTest(t, brainDir)
	now := time.Now().UTC()
	oldFactory := memoryAbstractorFactory
	defer func() { memoryAbstractorFactory = oldFactory }()
	cleanupAttempted := make(chan struct{})
	cleanupDone := make(chan error, 1)
	memoryAbstractorFactory = func(memoryAbstractsConfig) (ConversationAbstractor, error) {
		return &fakeAbstractor{hosted: true, beforeReturn: func() {
			go func() {
				close(cleanupAttempted)
				cleanupDone <- withBrainPrivacySideEffectLock(brainDir, func() error {
					return withBrainWriteLock(brainDir, func() error {
						plan, err := buildSessionPurgePlan(brainDir, "nav-sess")
						if err != nil {
							return err
						}
						return executeSessionCleanup(brainDir, "nav-sess", plan, now.Add(time.Second), "concurrent test", true)
					})
				})
			}()
			<-cleanupAttempted
			select {
			case err := <-cleanupDone:
				t.Fatalf("privacy cleanup linearized during hosted provider call: %v", err)
			case <-time.After(50 * time.Millisecond):
			}
		}}, nil
	}
	config := memoryConfig{SchemaVersion: memoryConfigSchemaVersion, Abstracts: memoryAbstractsConfig{
		Enabled: true, Provider: "fake", HostedEgressAllowed: true,
	}}
	if _, err := generateSessionAbstractContextGuarded(context.Background(), t.TempDir(), brainDir, view, config, now, nil); err != nil {
		t.Fatalf("privacy-linearized generation: %v", err)
	}
	if err := <-cleanupDone; err != nil {
		t.Fatalf("concurrent privacy cleanup: %v", err)
	}
	receipts, err := loadAbstractEgressReceiptsChecked(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(receipts) != 0 {
		t.Fatalf("provider completion recreated a purged egress receipt: %+v", receipts)
	}
	if _, ok := loadSessionAbstract(brainDir, sessionViewDigest(view)); ok {
		t.Fatal("provider completion published an abstract after privacy cleanup")
	}
}

type fakeAbstractor struct {
	fabricate    bool
	hosted       bool
	beforeReturn func()
}

func (f *fakeAbstractor) Identity() (string, string, string) {
	kind := "local"
	provider := "ollama"
	if f.hosted {
		kind = "hosted"
		provider = "codex"
	}
	return kind, provider, "fake-1"
}

func (f *fakeAbstractor) Abstract(_ context.Context, _ string, input conversationAbstractInput) (sessionAbstract, error) {
	if f.beforeReturn != nil {
		f.beforeReturn()
	}
	var artifact sessionAbstract
	evidence := input.Windows[0][0].ConversationID
	if f.fabricate {
		evidence = "conversation:fabricated"
	}
	artifact.Overview = abstractStatement{Text: "session walked five staged failures", EvidenceIDs: []string{evidence}}
	artifact.Decisions = []abstractStatement{{Text: "the flag parser was the culprit", EvidenceIDs: []string{evidence}}}
	return artifact, nil
}

func TestLocalAbstractLinearizesProviderAndPublicationBeforePurge(t *testing.T) {
	brainDir, _ := writeSessionNavigationFixture(t)
	view := sessionViewForTest(t, brainDir)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	oldFactory := memoryAbstractorFactory
	defer func() { memoryAbstractorFactory = oldFactory }()
	purgeAttempted := make(chan struct{})
	purgeDone := make(chan error, 1)
	memoryAbstractorFactory = func(memoryAbstractsConfig) (ConversationAbstractor, error) {
		return &fakeAbstractor{beforeReturn: func() {
			go func() {
				close(purgeAttempted)
				purgeDone <- withBrainPrivacySideEffectLock(brainDir, func() error {
					return withBrainWriteLock(brainDir, func() error {
						plan, err := buildSessionPurgePlan(brainDir, "nav-sess")
						if err != nil {
							return err
						}
						return executeSessionCleanup(brainDir, "nav-sess", plan, now, "race-test", true)
					})
				})
			}()
			<-purgeAttempted
			select {
			case err := <-purgeDone:
				t.Fatalf("purge linearized during local provider call: %v", err)
			case <-time.After(50 * time.Millisecond):
			}
		}}, nil
	}
	config := memoryConfig{SchemaVersion: memoryConfigSchemaVersion, Abstracts: memoryAbstractsConfig{Enabled: true, Provider: "fake"}}
	if _, err := generateSessionAbstractContext(context.Background(), t.TempDir(), brainDir, view, config, now); err != nil {
		t.Fatalf("privacy-linearized generation: %v", err)
	}
	if err := <-purgeDone; err != nil {
		t.Fatalf("purge after provider publication: %v", err)
	}
	if _, ok := loadSessionAbstract(brainDir, sessionViewDigest(view)); ok {
		t.Fatal("purged session content was republished by an in-flight provider")
	}
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
	guard, err := loadSessionReadGuard(brainDir, manifest)
	if err != nil {
		t.Fatal(err)
	}
	views := buildConversationSessionViews(fresh, manifest.RepoKey, manifest, guard)
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
	config.Abstracts.Automatic = true
	if err := saveMemoryConfig(brainDir, config); err != nil {
		t.Fatal(err)
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
	request, err := automaticAbstractWorkRequest(brainDir, view)
	if err != nil || request == nil || request.Kind != memoryJobKindSessionAbstract || request.SessionRef != view.Ref {
		t.Fatalf("automatic request = %+v err=%v", request, err)
	}
	artifact, err := generateSessionAbstract(brainDir, view, config, now)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if artifact.SessionRef != view.Ref || artifact.Generator.Provider != "ollama" || artifact.Coverage.TotalTurns != 5 {
		t.Fatalf("artifact identity: %+v", artifact)
	}
	status, current := sessionAbstractStatus(brainDir, view)
	if status != abstractStatusCurrent || current == nil {
		t.Fatalf("status after generation = %s", status)
	}
	previews, truncated, err := abstractPreviewsForResults(brainDir, []unifiedResult{
		{Source: retrievalSourceConversation, ID: view.Records[0].ID, SessionRef: view.Ref},
		{Source: retrievalSourceConversation, ID: view.Records[1].ID, SessionRef: view.Ref},
	})
	if err != nil || truncated || len(previews) != 1 || previews[0].Overview == nil || previews[0].Overview.Text != artifact.Overview.Text {
		t.Fatalf("previews=%+v truncated=%v err=%v", previews, truncated, err)
	}
	// Hosted generation records metadata-only egress before the call and
	// finalizes it without storing transcript bodies or provider diagnostics.
	config.Abstracts.HostedEgressAllowed = true
	memoryAbstractorFactory = func(memoryAbstractsConfig) (ConversationAbstractor, error) { return &fakeAbstractor{hosted: true}, nil }
	if _, err := generateSessionAbstract(brainDir, view, config, now.Add(time.Second)); err != nil {
		t.Fatalf("hosted generate: %v", err)
	}
	egress, err := os.ReadDir(filepath.Join(brainDir, filepath.FromSlash(abstractEgressDirRel)))
	if err != nil || len(egress) != 1 {
		t.Fatalf("egress receipts=%d err=%v", len(egress), err)
	}
	config.Abstracts.HostedEgressAllowed = false
	memoryAbstractorFactory = func(memoryAbstractsConfig) (ConversationAbstractor, error) { return &fakeAbstractor{}, nil }

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
	if _, err := generateSessionAbstract(brainDir, stale, config, now.Add(time.Minute)); err == nil || !strings.Contains(err.Error(), "validation failed") {
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
	if _, _, err := getUnifiedBatchOptions("", brainDir, "main", []string{newView.Ref}, getOptions{}); err != nil {
		t.Fatalf("stale session get: %v", err)
	}
	enqueued := false
	for _, job := range loadMemoryJobs(brainDir) {
		if job.Kind == memoryJobKindSessionAbstract && job.SessionRef == newView.Ref && job.InputDigest == sessionViewDigest(newView) {
			enqueued = true
		}
	}
	if !enqueued {
		t.Fatal("stale automatic session get did not durably enqueue abstract regeneration")
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

func TestMemoryAdminScopedAndResumeFlags(t *testing.T) {
	if newMemoryRepairCommand(Options{}).Flags().Lookup("session-ref") == nil {
		t.Fatal("repair missing --session-ref")
	}
	if newMemoryRebuildCommand(Options{}).Flags().Lookup("session-ref") == nil {
		t.Fatal("rebuild missing --session-ref")
	}
	if newMemoryMigrateCommand(Options{}).Flags().Lookup("resume") == nil {
		t.Fatal("migrate missing --resume")
	}
	brainDir, _ := writeSessionNavigationFixture(t)
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	view := sessionViewForTest(t, brainDir)
	digest, err := canonicalSessionInputDigest(brainDir, manifest, view.Ref)
	if err != nil || digest == "" {
		t.Fatalf("scope digest=%q err=%v", digest, err)
	}
}

func TestMemoryRebuildFailurePreservesVectorStores(t *testing.T) {
	opts, brainDir := memoryAdminCommandFixture(t)
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Sources == nil {
		manifest.Sources = &brainSources{}
	}
	fixtureDir, transcriptRel, _ := historyProjectionFixture(t)
	fixtureManifest, err := loadBrainManifest(fixtureDir)
	if err != nil {
		t.Fatal(err)
	}
	transcript, err := os.ReadFile(filepath.Join(fixtureDir, filepath.FromSlash(transcriptRel)))
	if err != nil {
		t.Fatal(err)
	}
	if err := writeBrainRelativeFileAtomic(brainDir, transcriptRel, transcript, 0o600); err != nil {
		t.Fatal(err)
	}
	manifest.Sources.Sessions = fixtureManifest.Sources.Sessions
	if err := writeBrainManifestAndReadme(brainDir, *manifest); err != nil {
		t.Fatal(err)
	}
	vectorRel := filepath.ToSlash(filepath.Join(historyDirName, embedStoreDirName, historyVecStoreFileNamePortable))
	vectorPath := filepath.Join(brainDir, filepath.FromSlash(vectorRel))
	if err := os.MkdirAll(filepath.Dir(vectorPath), 0o700); err != nil {
		t.Fatal(err)
	}
	want := []byte("prior-vector-store")
	if err := os.WriteFile(vectorPath, want, 0o600); err != nil {
		t.Fatal(err)
	}
	originalWrite := historyProjectionWriteFile
	defer func() { historyProjectionWriteFile = originalWrite }()
	stageWrites := 0
	historyProjectionWriteFile = func(dir, rel string, data []byte, mode os.FileMode) error {
		stageWrites++
		return errors.New("injected stage write failure")
	}
	out, err := execute(t, newMemoryRebuildCommand(opts), "--all")
	if err == nil || !strings.Contains(err.Error(), "injected stage write failure") {
		t.Fatalf("rebuild error = %v, want injected failure\n%s", err, out)
	}
	if stageWrites == 0 {
		t.Fatal("real rebuild never reached the projection write boundary")
	}
	got, err := os.ReadFile(vectorPath)
	if err != nil || string(got) != string(want) {
		t.Fatalf("prior vector store was not preserved: %q err=%v", got, err)
	}
	generation := strings.Repeat("a", 40)
	if err := writeBrainManifestAndReadme(brainDir, exportManifest{
		SchemaVersion: brainManifestSchemaVersion,
		Sources: &brainSources{History: &historySourceManifest{
			IndexPath:           historyGenerationArtifactPath(generation, historyIndexFileName),
			ProjectionStatePath: historyGenerationArtifactPath(generation, projectionStateFileName),
		}},
	}); err != nil {
		t.Fatal(err)
	}
	artifacts := memoryDerivedArtifactStates(brainDir)
	if err := markProjectionRefreshStates(brainDir, &artifacts); err != nil {
		t.Fatal(err)
	}
	for _, artifact := range artifacts {
		if artifact.Path == vectorRel && artifact.NewState != "preserved" {
			t.Fatalf("vector receipt must be truthful: %+v", artifact)
		}
	}
}

func TestMemoryMigrationProgressIsDurable(t *testing.T) {
	brainDir := t.TempDir()
	want := memoryMigrationProgress{SchemaVersion: 1, OperationID: "op:0123456789abcdef", StartedAt: time.Now().UTC(), Completed: []string{historyShortTermPath}}
	if err := saveMemoryMigrationProgress(brainDir, want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := loadMemoryMigrationProgress(brainDir)
	if err != nil || !ok || got.OperationID != want.OperationID || len(got.Completed) != 1 || got.Completed[0] != historyShortTermPath {
		t.Fatalf("progress = %+v ok=%v err=%v", got, ok, err)
	}
}

func TestMemoryMigrationRewritesLegacyJobs(t *testing.T) {
	brainDir := t.TempDir()
	now := time.Now().UTC()
	job := memoryJob{Kind: memoryJobKindProjection, RepoKey: "local/test", SessionID: "s", SessionRef: "conversation-session:s", InputDigest: "sha256:x", Trigger: "test", State: memoryJobStatePending, CreatedAt: now, AvailableAt: now}
	job.JobID = memoryJobID(job.RepoKey, job.SessionRef, job.InputDigest, job.Kind)
	if err := saveMemoryJob(brainDir, job); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(brainDir, filepath.FromSlash(memoryJobRel(job.JobID)))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), `"schema_version": 2`, `"schema_version": 1`, 1))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	findings := detectMemoryMigrations(brainDir, nil)
	found := false
	for _, finding := range findings {
		if finding.Path == memoryJobRel(job.JobID) && finding.State == memoryErrMigrationRequired {
			found = true
		}
	}
	if !found {
		t.Fatalf("legacy job finding missing: %+v", findings)
	}
	if err := rewriteLegacyMemoryJob(brainDir, memoryJobRel(job.JobID)); err != nil {
		t.Fatal(err)
	}
	if issues := scanMemoryJobSchemas(brainDir); len(issues) != 0 {
		t.Fatalf("job migration did not reach current schema: %+v", issues)
	}
}

// TestProjectionReceiptsCommitThroughManifest is the kill-boundary
// proof for receipt publication: a receipt file written without the manifest
// commit is unreferenced, so readers stay on the previous readable generation.
func TestProjectionReceiptsCommitThroughManifest(t *testing.T) {
	brainDir, _, _ := shortTermFixture(t)
	// shortTermFixture appends a second canonical session after its initial full
	// build. Consolidate that deliberate source change before testing the
	// independent uncommitted-receipt boundary.
	if _, err := writeBrainHistoryIndexAndSource(brainDir, time.Date(2026, 8, 7, 13, 0, 0, 0, time.UTC), nil); err != nil {
		t.Fatal(err)
	}
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, state, err := loadProjectionStateChecked(brainDir, manifest.Sources.History); state != projectionStateCurrent || err != nil {
		t.Fatalf("fixture full build must publish receipts: state=%s err=%v", state, err)
	}
	// Simulate the crash: a newer receipt file lands but the manifest commit
	// never happened.
	state := projectionState{SchemaVersion: projectionSchemaVersion, ReconcilerVersion: projectionReconcilerVersion, GeneratedAt: time.Now().UTC()}
	if _, err := writeProjectionState(brainDir, state); err != nil {
		t.Fatal(err)
	}
	if _, ok := loadProjectionState(brainDir, manifest.Sources.History); !ok {
		t.Fatal("an uncommitted receipt must not invalidate the previous generation")
	}
}
