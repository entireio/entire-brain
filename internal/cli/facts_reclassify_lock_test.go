package cli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// envHoldBrainWriteLock names the brain directory the helper process below
// locks. It is only ever set by TestFactsReclassifyWaitsForOtherProcess.
const envHoldBrainWriteLock = "BRAIN_TEST_HOLD_BRAIN_WRITE_LOCK"

// TestHelperHoldBrainWriteLock is not a test: it is the CHILD process the
// cross-process lock test re-execs. It takes the brain write lock, announces
// that on stdout, holds it until its parent writes a line to stdin, and exits.
// The brain lock is an flock(2) advisory lock, so only a real second process
// proves the contract an in-process mutex would fake.
func TestHelperHoldBrainWriteLock(t *testing.T) {
	brainDir := os.Getenv(envHoldBrainWriteLock)
	if brainDir == "" {
		t.Skip("helper process for TestFactsReclassifyWaitsForOtherProcess")
	}
	unlock, err := acquireBrainWriteLockTimeout(brainDir, 60*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "helper: acquire brain write lock: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("HELPER-LOCKED")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	unlock()
}

// seedReclassifyBrain writes one kind-less fact plus a manifest and returns the
// brain dir and branch.
func seedReclassifyBrain(t *testing.T, now time.Time) (string, string) {
	t.Helper()
	brainDir, branch := t.TempDir(), "main"
	seed := []factRecord{{
		ID: "fact:inv", Paths: []string{"constraints.api.shape"},
		Text: "The token must never be logged.", Branch: branch,
		Origin: factOriginDistilled, Status: factStatusActive,
		Provenance: []factAnchor{{SessionID: "s1"}}, UpdatedAt: now,
	}}
	if err := writeFacts(brainDir, branch, seed); err != nil {
		t.Fatal(err)
	}
	if err := writeBrainManifestAndReadme(brainDir, exportManifest{SchemaVersion: brainManifestSchemaVersion, GeneratedAt: now}); err != nil {
		t.Fatal(err)
	}
	return brainDir, branch
}

func reclassifyTestCommand() (*cobra.Command, *bytes.Buffer) {
	cmd := &cobra.Command{}
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	return cmd, out
}

// TestFactsReclassifyWaitsForOtherProcess proves `facts reclassify` performs its
// load-mutate-write of facts.ndjson under the brain write lock. A second PROCESS
// holds the lock; while it does, reclassify must not have rewritten the store.
// Without the lock it rewrites it immediately and any transaction the other
// process is in the middle of is clobbered.
func TestFactsReclassifyWaitsForOtherProcess(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	brainDir, branch := seedReclassifyBrain(t, now)

	helperCtx, cancelHelper := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelHelper()
	helper := exec.CommandContext(helperCtx, os.Args[0], "-test.run=^TestHelperHoldBrainWriteLock$", "-test.timeout=120s")
	helper.Env = append(os.Environ(), envHoldBrainWriteLock+"="+brainDir)
	stdin, err := helper.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := helper.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	helper.Stderr = os.Stderr
	if err := helper.Start(); err != nil {
		t.Skipf("cannot re-exec the test binary as a lock holder: %v", err)
	}
	var done chan error
	workerDone := false
	defer func() {
		_ = stdin.Close()
		if helper.Process != nil {
			_ = helper.Process.Kill()
		}
		_ = helper.Wait()
		if done != nil && !workerDone {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Errorf("facts reclassify worker did not stop during cleanup")
			}
		}
	}()

	scanner := bufio.NewScanner(stdout)
	ready := make(chan error, 1)
	go func() {
		for scanner.Scan() {
			if strings.Contains(scanner.Text(), "HELPER-LOCKED") {
				ready <- nil
				return
			}
		}
		if err := scanner.Err(); err != nil {
			ready <- err
			return
		}
		ready <- errors.New("helper exited before reporting the brain write lock")
	}()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatal(err)
		}
	case <-helperCtx.Done():
		t.Fatalf("helper process never reported holding the brain write lock: %v", helperCtx.Err())
	}

	cmd, _ := reclassifyTestCommand()
	opts := Options{Version: "test", Now: func() time.Time { return now }}
	done = make(chan error, 1)
	go func() { done <- runFactsReclassify(cmd, opts, brainDir, branch, false, false) }()

	time.Sleep(500 * time.Millisecond)
	midFlight, err := loadFacts(brainDir, branch)
	if err != nil {
		t.Fatal(err)
	}
	if len(midFlight) != 1 || midFlight[0].Kind != "" {
		t.Fatalf("facts reclassify rewrote facts.ndjson while another process held the brain write lock: %+v", midFlight)
	}

	if _, err := io.WriteString(stdin, "go\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		workerDone = true
		if err != nil {
			t.Fatalf("reclassify after the lock was released: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("facts reclassify did not finish after the lock was released")
	}
	after, err := loadFacts(brainDir, branch)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || after[0].Kind != factKindInvariant {
		t.Fatalf("reclassify did not persist the kind once it held the lock: %+v", after)
	}
}
