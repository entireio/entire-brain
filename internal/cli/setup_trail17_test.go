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
