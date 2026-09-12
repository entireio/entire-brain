package cli

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSetupPlainRunKeepsMachineDaemonIdentity(t *testing.T) {
	f := newSetupTestFixture(t)
	const name = "entire-brain-watch-shared"
	if _, err := recordSetupWatchPlan(f.env, name, setupWatchPlanEntry{Workspace: "other"}); err != nil {
		t.Fatal(err)
	}
	rec := &recordedSetup{}
	runSetupForTest(t, f, defaultSetupOptions(), rec)
	plan, err := loadSetupWatchPlan(f.env)
	if err != nil {
		t.Fatal(err)
	}
	if plan.DaemonName != name || rec.state.Label != launchdLabel(name) {
		t.Fatalf("plain setup changed shared daemon: plan=%+v daemon=%+v", plan, rec.state)
	}
}

func TestSetupFailedPlanPreservesDaemonIdentity(t *testing.T) {
	f := newSetupTestFixture(t)
	const name = "entire-brain-watch-shared"
	if _, err := recordSetupWatchPlan(f.env, name, setupWatchPlanEntry{Workspace: "other"}); err != nil {
		t.Fatal(err)
	}
	rec := &recordedSetup{}
	opts := defaultSetupOptions()
	opts.daemonName = "replacement"
	steps := rec.steps(f)
	steps.plan = func(setupCommandOptions) (daemonPlan, error) {
		return daemonPlan{}, errors.New("cannot locate executable")
	}
	if err := runSetup(context.Background(), setupTestCommand(t, &bytes.Buffer{}, opts), f.opts, opts, f.repoDir, steps); err != nil {
		t.Fatal(err)
	}
	plan, err := loadSetupWatchPlan(f.env)
	if err != nil {
		t.Fatal(err)
	}
	if plan.DaemonName != name {
		t.Fatalf("failed plan replaced machine identity: %+v", plan)
	}
	recorded, _, err := setupOptionsFromRecord(filepath.Dir(f.storage.HeadPath))
	if err != nil {
		t.Fatal(err)
	}
	if recorded.daemonName == opts.daemonName {
		t.Fatal("failed daemon plan was persisted as installed identity")
	}
}

func TestDistillTextReportsDeferredSessions(t *testing.T) {
	f := newSetupTestFixture(t, "one", "two")
	var output bytes.Buffer
	cmd := setupTestCommand(t, &output, defaultSetupOptions())
	opts := distillCommandOptions{agent: "command", agentCommand: []string{"fake"}, maxSessions: 1, concurrency: 1, maxChunkBytes: defaultDistillChunkSize, timeout: time.Minute,
		run: func(context.Context, string, []string, []byte, time.Duration) (string, error) {
			return "project.tooling.stack\tUse Go.\n", nil
		},
	}
	if err := runDistill(context.Background(), cmd, f.opts, opts, f.repoDir); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "session(s) deferred") {
		t.Fatalf("deferred sessions hidden: %s", output.String())
	}
}

func TestSetupRerunPreservesPinnedAgentAndUnlimitedBudget(t *testing.T) {
	f := newSetupTestFixture(t)
	rec := &recordedSetup{}
	opts := defaultSetupOptions()
	opts.agent = "claude"
	opts.backfillBudget = 0
	cmd := setupTestCommand(t, &bytes.Buffer{}, opts)
	if err := cmd.Flags().Set("agent", "claude"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("backfill-budget", "0"); err != nil {
		t.Fatal(err)
	}
	if err := runSetup(context.Background(), cmd, f.opts, opts, f.repoDir, rec.steps(f)); err != nil {
		t.Fatal(err)
	}
	runSetupForTest(t, f, defaultSetupOptions(), rec)
	plan, err := loadSetupWatchPlan(f.env)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Workspaces) != 1 || plan.Workspaces[0].Agent != "claude" || plan.Workspaces[0].MaxSessions != 0 {
		t.Fatalf("pinned tuning lost: %+v", plan)
	}
}

func TestSemanticMissingCustomBinaryNamesTheBinary(t *testing.T) {
	err := semanticDoctorFailureError("/missing/custom-graph", []semanticWarning{{Code: "provider_doctor_failed", Detail: "no such file or directory"}})
	if !strings.Contains(err.Error(), `graph binary "/missing/custom-graph" was not found`) {
		t.Fatal(err)
	}
}

func TestOnboardingStatusReportsCorruptWatchPlan(t *testing.T) {
	f := newSetupTestFixture(t)
	path, err := setupWatchPlanPath(f.env)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeJSONFile(path, map[string]any{"schema_version": 0}); err != nil {
		t.Fatal(err)
	}
	status := buildBrainOnboardingStatus(context.Background(), f.opts, f.storage, nil, defaultSetupOptions())
	if !strings.Contains(describeDaemonState(status.Daemon, "entire-brain"), "watch plan unavailable") {
		t.Fatalf("corrupt plan hidden: %+v", status)
	}
}

func TestFailedWorkspaceRegistrationPreservesSharedTuning(t *testing.T) {
	f := newSetupTestFixture(t)
	if _, err := recordSetupWatchPlan(f.env, daemonDefaultName, setupWatchPlanEntry{Workspace: setupDefaultWorkspace, Interval: "1h"}); err != nil {
		t.Fatal(err)
	}
	dir, err := workspaceDir(f.env, setupDefaultWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(filepath.Join(dir, workspaceManifestName), []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	rec := &recordedSetup{}
	runSetupForTest(t, f, defaultSetupOptions(), rec)
	plan, err := loadSetupWatchPlan(f.env)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Workspaces[0].Interval != "1h" || rec.installCalls != 0 {
		t.Fatalf("failed registration changed daemon: plan=%+v installs=%d", plan, rec.installCalls)
	}
}
