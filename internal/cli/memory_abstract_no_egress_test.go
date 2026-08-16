package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHostedAbstractsFailClosedUnderGlobalNoEgressBeforeSideEffects(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "1")
	t.Setenv("ENTIRE_BRAIN_LOCAL_ONLY", "")
	providerCalls := 0
	factoryCalls := 0
	hosted := &fakeAbstractor{hosted: true, beforeReturn: func() { providerCalls++ }}
	oldFactory := memoryAbstractorFactory
	memoryAbstractorFactory = func(memoryAbstractsConfig) (ConversationAbstractor, error) {
		factoryCalls++
		return hosted, nil
	}
	t.Cleanup(func() { memoryAbstractorFactory = oldFactory })

	t.Run("configure", func(t *testing.T) {
		opts, brainDir := memoryAdminCommandFixture(t)
		cmd := newMemoryConfigureCommand(opts)
		cmd.SilenceErrors, cmd.SilenceUsage = true, true
		out, err := execute(t, cmd, "abstracts", "--enable", "--provider", "codex", "--allow-hosted-egress", "--json")
		if err == nil || !strings.Contains(err.Error(), "no_egress") || !strings.Contains(err.Error(), memoryErrProviderUnavail) {
			t.Fatalf("hosted configure error=%v\n%s", err, out)
		}
		var receipt memoryOperationReceipt
		if decodeErr := json.Unmarshal([]byte(out), &receipt); decodeErr != nil || receipt.Operation != "configure_abstracts" || receipt.ErrorCode != memoryErrProviderUnavail || receipt.FinishedAt.IsZero() {
			t.Fatalf("configure failure receipt=%+v decode=%v\n%s", receipt, decodeErr, out)
		}
		if _, state, loadErr := loadMemoryConfigChecked(brainDir); loadErr != nil || state != memoryConfigAbsent {
			t.Fatalf("hosted configure changed config: state=%s err=%v", state, loadErr)
		}
		if jobs := loadMemoryJobs(brainDir); len(jobs) != 0 {
			t.Fatalf("hosted configure created jobs: %+v", jobs)
		}
	})

	t.Run("manual enqueue and generation", func(t *testing.T) {
		opts, brainDir := memoryAdminCommandFixture(t)
		ref := addMemoryAdminConversationFixture(t, brainDir, time.Date(2026, 8, 10, 7, 0, 0, 0, time.UTC))
		cmd := newMemoryAbstractCommand(opts)
		cmd.SilenceErrors, cmd.SilenceUsage = true, true
		out, err := execute(t, cmd, ref, "--provider", "codex", "--model", "hosted-test", "--allow-hosted-egress", "--json")
		if err == nil || !strings.Contains(err.Error(), "no_egress") || !strings.Contains(err.Error(), memoryErrProviderUnavail) {
			t.Fatalf("hosted manual enqueue error=%v\n%s", err, out)
		}
		var receipt memoryOperationReceipt
		if decodeErr := json.Unmarshal([]byte(out), &receipt); decodeErr != nil || receipt.Operation != "abstract_generate" || receipt.ErrorCode != memoryErrProviderUnavail || receipt.FinishedAt.IsZero() {
			t.Fatalf("manual failure receipt=%+v decode=%v\n%s", receipt, decodeErr, out)
		}
		if jobs := loadMemoryJobs(brainDir); len(jobs) != 0 {
			t.Fatalf("hosted manual request created durable work: %+v", jobs)
		}
		if _, state, loadErr := loadMemoryConfigChecked(brainDir); loadErr != nil || state != memoryConfigAbsent {
			t.Fatalf("hosted manual request changed config: state=%s err=%v", state, loadErr)
		}

		view, viewErr := loadConversationSessionViewByRef(brainDir, ref)
		if viewErr != nil {
			t.Fatal(viewErr)
		}
		config := memoryConfig{SchemaVersion: memoryConfigSchemaVersion, Abstracts: memoryAbstractsConfig{
			Enabled: true, Provider: "codex", Model: "hosted-test", HostedEgressAllowed: true,
		}}
		if _, generateErr := generateSessionAbstractContextGuarded(context.Background(), brainDir, brainDir, view, config, time.Date(2026, 8, 10, 7, 1, 0, 0, time.UTC), nil); generateErr == nil || !strings.Contains(generateErr.Error(), "no_egress") {
			t.Fatalf("hosted generation error=%v", generateErr)
		}
		if saveErr := saveMemoryConfig(brainDir, config); saveErr != nil {
			t.Fatalf("prepare configured generation: %v", saveErr)
		}
		if generateErr := runSessionAbstractJob(context.Background(), brainDir, brainDir, ref, sessionViewDigest(view), time.Date(2026, 8, 10, 7, 2, 0, 0, time.UTC)); generateErr == nil || !strings.Contains(generateErr.Error(), "no_egress") {
			t.Fatalf("durable worker generation error=%v", generateErr)
		}
		if receipts, receiptErr := loadAbstractEgressReceiptsChecked(brainDir); receiptErr != nil || len(receipts) != 0 {
			t.Fatalf("hosted generation wrote egress receipts=%+v err=%v", receipts, receiptErr)
		}
		if _, ok := loadSessionAbstract(brainDir, sessionViewDigest(view)); ok {
			t.Fatal("hosted generation published an abstract")
		}
	})

	t.Run("automatic reconciliation", func(t *testing.T) {
		_, brainDir := memoryAdminCommandFixture(t)
		_ = addMemoryAdminConversationFixture(t, brainDir, time.Date(2026, 8, 10, 7, 10, 0, 0, time.UTC))
		config := memoryConfig{SchemaVersion: memoryConfigSchemaVersion, Abstracts: memoryAbstractsConfig{
			Enabled: true, Automatic: true, Provider: "codex", Model: "hosted-test", HostedEgressAllowed: true,
		}}
		if err := saveMemoryConfig(brainDir, config); err != nil {
			t.Fatal(err)
		}
		if err := enqueueAutomaticAbstractReconciliation(brainDir, "worker", time.Date(2026, 8, 10, 7, 11, 0, 0, time.UTC)); err == nil || !strings.Contains(err.Error(), "no_egress") {
			t.Fatalf("automatic hosted reconciliation error=%v", err)
		}
		if jobs := loadMemoryJobs(brainDir); len(jobs) != 0 {
			t.Fatalf("automatic hosted reconciliation created jobs: %+v", jobs)
		}
	})

	if factoryCalls != 0 || providerCalls != 0 {
		t.Fatalf("hosted provider resolution/call escaped global no-egress: factory=%d provider=%d", factoryCalls, providerCalls)
	}
}

func TestGlobalNoEgressRefusesHostedAbstractJobBeforeClaimWrites(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "1")
	t.Setenv("ENTIRE_BRAIN_LOCAL_ONLY", "")
	oldFactory := memoryAbstractorFactory
	factoryCalls := 0
	memoryAbstractorFactory = func(memoryAbstractsConfig) (ConversationAbstractor, error) {
		factoryCalls++
		return &fakeAbstractor{hosted: true}, nil
	}
	t.Cleanup(func() { memoryAbstractorFactory = oldFactory })

	for _, state := range []string{memoryJobStatePending, memoryJobStateRetryable} {
		t.Run(state, func(t *testing.T) {
			_, brainDir := memoryAdminCommandFixture(t)
			ref := addMemoryAdminConversationFixture(t, brainDir, time.Date(2026, 8, 10, 7, 20, 0, 0, time.UTC))
			view, err := loadConversationSessionViewByRef(brainDir, ref)
			if err != nil {
				t.Fatal(err)
			}
			manifest, err := loadBrainManifest(brainDir)
			if err != nil {
				t.Fatal(err)
			}
			config := memoryConfig{SchemaVersion: memoryConfigSchemaVersion, Abstracts: memoryAbstractsConfig{
				Enabled: true, Automatic: false, Provider: "codex", Model: "hosted-test", HostedEgressAllowed: true,
			}}
			if err := saveMemoryConfig(brainDir, config); err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 8, 10, 7, 21, 0, 0, time.UTC)
			digest := sessionViewDigest(view)
			job := memoryJob{
				SchemaVersion: memoryJobSchemaVersion,
				JobID:         memoryJobID(manifest.RepoKey, ref, digest, memoryJobKindSessionAbstract),
				Kind:          memoryJobKindSessionAbstract,
				RepoKey:       manifest.RepoKey,
				SessionID:     view.Records[0].SessionID,
				SessionRef:    ref,
				InputDigest:   digest,
				Trigger:       "worker",
				State:         state,
				Attempt:       1,
				CreatedAt:     now.Add(-time.Minute),
				AvailableAt:   now.Add(-time.Second),
			}
			if err := saveMemoryJob(brainDir, job); err != nil {
				t.Fatal(err)
			}
			jobPath := filepath.Join(brainDir, filepath.FromSlash(memoryJobRel(job.JobID)))
			before, err := os.ReadFile(jobPath)
			if err != nil {
				t.Fatal(err)
			}

			stats, runErr := runMemoryAbstractLane(context.Background(), brainDir, brainDir, now, "owner-must-not-stick")
			if runErr == nil || !strings.Contains(runErr.Error(), "no_egress") || stats.JobsCreated+stats.JobsCompleted+stats.JobsRetried != 0 {
				t.Fatalf("hosted claim stats=%+v error=%v", stats, runErr)
			}
			after, err := os.ReadFile(jobPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, before) {
				t.Fatalf("policy refusal changed job bytes\nbefore=%s\nafter=%s", before, after)
			}
			stored, err := memoryJobByID(brainDir, job.JobID)
			if err != nil || stored.State != state || stored.OwnerToken != "" || stored.HeartbeatAt != nil {
				t.Fatalf("policy refusal changed job: %+v err=%v", stored, err)
			}
		})
	}
	if factoryCalls != 0 {
		t.Fatalf("hosted claim resolved provider %d times", factoryCalls)
	}
}

func TestGlobalNoEgressAllowsExplicitLocalOllamaAbstractConfiguration(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "1")
	t.Setenv("ENTIRE_BRAIN_LOCAL_ONLY", "")
	opts, brainDir := memoryAdminCommandFixture(t)
	local := &privacyCountingAbstractor{}
	oldFactory := memoryAbstractorFactory
	memoryAbstractorFactory = func(memoryAbstractsConfig) (ConversationAbstractor, error) { return local, nil }
	t.Cleanup(func() { memoryAbstractorFactory = oldFactory })

	out, err := execute(t, newMemoryConfigureCommand(opts), "abstracts", "--enable", "--provider", "ollama", "--model", "local-test", "--json")
	if err != nil {
		t.Fatalf("local Ollama configure: %v\n%s", err, out)
	}
	config, state, loadErr := loadMemoryConfigChecked(brainDir)
	if loadErr != nil || state != memoryConfigCurrent || !config.Abstracts.Enabled || config.Abstracts.Provider != "ollama" || local.calls.Load() != 0 {
		t.Fatalf("local config=%+v state=%s provider_calls=%d err=%v", config, state, local.calls.Load(), loadErr)
	}
	if err := rejectMemoryAbstractJobForGlobalNoEgress(brainDir, memoryJob{}); err != nil {
		t.Fatalf("local Ollama job policy: %v", err)
	}
}
