package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The failure these pin was found on a real machine, not imagined: a field
// RETIRED from the code without a schema bump leaves every manifest the older
// build wrote unwritable by this one. Writers refuse it on purpose -- rewriting
// from a struct that never saw the field would erase what another build
// recorded -- so the condition is permanent, and the worker relaunched against
// it on a 1m..30m chain for two hours per repository, logging only
// "prepass_failed".

// The exact field and shape taken from a manifest on disk.
func writeManifestWithRetiredField(t *testing.T, dir string) {
	t.Helper()
	body := map[string]any{
		"schema_version": brainManifestSchemaVersion,
		"repo_key":       "local/demo-1234",
		"generated_at":   "2026-10-11T22:00:00Z",
		"suppressed_warnings": []string{
			"checkpoint ref refs/remotes/origin/entire/checkpoints/v1 unreadable for 0a2fd94192c1",
		},
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, exportManifestFileName), data, 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

func TestAWriterRefusingARetiredFieldNamesTheRemedy(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeManifestWithRetiredField(t, dir)

	_, err := loadBrainManifestForReplace(dir)
	if err == nil {
		t.Fatal("a manifest a writer cannot round-trip must be refused, or it silently erases another build's state")
	}
	msg := err.Error()
	if !strings.Contains(msg, "refresh --force") {
		t.Fatalf("the refusal names no way out, which is how this ran for two hours unattended: %s", msg)
	}
}

// Readers must still be able to open it. Availability is the reader's concern;
// not destroying state is the writer's -- if this regressed, a single retired
// field would take every query down with it.
func TestAReaderStillOpensAManifestWithARetiredField(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeManifestWithRetiredField(t, dir)

	manifest, err := loadBrainManifest(dir)
	if err != nil {
		t.Fatalf("readers must tolerate a retired field: %v", err)
	}
	if manifest == nil || manifest.RepoKey != "local/demo-1234" {
		t.Fatalf("the manifest decoded but lost its contents: %+v", manifest)
	}
}

func TestRetiredManifestFieldIsRecognisedAsDurable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeManifestWithRetiredField(t, dir)
	_, err := loadBrainManifestForReplace(dir)
	if err == nil {
		t.Fatal("fixture did not produce the failure, so this guard would pass on any change")
	}

	if !errRetiredManifestField(err) {
		t.Fatalf("a retired field is durable and must stop the relaunch chain: %v", err)
	}
	// The exact string the worker sees, through the delta-export path that
	// wraps it -- that wrapper is why the check cannot rely on the typed error.
	wrapped := wrapTestError("delta session export", err)
	if !errRetiredManifestField(wrapped) {
		t.Fatalf("the wrapped form the worker actually receives was not recognised: %v", wrapped)
	}
}

// A transient failure must NOT be classified as durable, or one bad run stops
// the brain updating until something else happens to restart the chain.
func TestAnOrdinaryFailureStillRetries(t *testing.T) {
	t.Parallel()
	for _, err := range []error{
		wrapTestError("delta session export", os.ErrDeadlineExceeded),
		wrapTestError("delta session export", os.ErrPermission),
	} {
		if errRetiredManifestField(err) {
			t.Fatalf("a transient failure was treated as durable, so it would never be retried: %v", err)
		}
	}
	if errRetiredManifestField(nil) {
		t.Fatal("no error is not a durable failure")
	}
}

func TestTheRemedyMessageKeepsTheOriginalCause(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeManifestWithRetiredField(t, dir)
	_, err := loadBrainManifestForReplace(dir)

	msg := retiredManifestFieldMessage(err)
	if !strings.Contains(msg, "suppressed_warnings") {
		t.Fatalf("the remedy replaced the cause instead of adding to it: %s", msg)
	}
	if !strings.Contains(msg, "refresh --force") {
		t.Fatalf("the message states a problem and no action: %s", msg)
	}
	if retiredManifestFieldMessage(nil) != "" {
		t.Fatal("no error must produce no message")
	}
}

// A brain outlives its repository -- scratch clones, benchmark cells, a
// checkout someone deleted. Nine such brains were sitting on this machine,
// three of them with roughly 2,650 recorded prepass failures each, because the
// worker kept relaunching against a repository that was not coming back.
//
// The error text is the one a real orphaned brain produces, captured by
// deleting a repository out from under its brain and running the worker.
func TestAMissingRepositoryIsDurable(t *testing.T) {
	t.Parallel()
	err := wrapTestError("delta session export",
		wrapTestError("complete routed checkpoint discovery failed: list checkpoints",
			wrapTestError("entire [checkpoint explain --json --search-all]: exit status 1", errNotAGitRepoForTest)))

	if !errRepositoryGone(err) {
		t.Fatalf("an orphaned brain must stop relaunching: %v", err)
	}
	if !errDurablePrepassFailure(err) {
		t.Fatalf("the worker asks the general classifier, so it must agree: %v", err)
	}
	msg := durablePrepassMessage(err)
	// Spelling, not text: the remedy renders through brainInvocation, which
	// resolves to the plugin dispatch or the standalone binary depending on the
	// install. Pinning the literal "entire brain reset" would pass only on one
	// of them, which is the defect brainInvocation exists to prevent.
	if !strings.Contains(msg, brainInvocation(os.LookupEnv, "reset")) {
		t.Fatalf("the message names no way to clear the orphan: %s", msg)
	}
	if !strings.Contains(msg, "not a git repository") {
		t.Fatalf("the remedy replaced the cause instead of adding to it: %s", msg)
	}
}

// Two durable causes now share one path, and they must not be confused: a
// retired field is repaired by refresh, an orphan by reset. Telling someone to
// reset a brain that only needed a refresh would destroy work for nothing.
func TestTheTwoDurableCausesKeepTheirOwnRemedies(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeManifestWithRetiredField(t, dir)
	_, err := loadBrainManifestForReplace(dir)
	if err == nil {
		t.Fatal("fixture produced no error, so this guard would pass on anything")
	}

	if got := durablePrepassMessage(err); !strings.Contains(got, "refresh --force") || strings.Contains(got, "reset") {
		t.Fatalf("a retired field must be sent to refresh, never to reset: %s", got)
	}

	orphan := wrapTestError("delta session export", errNotAGitRepoForTest)
	if got := durablePrepassMessage(orphan); !strings.Contains(got, "reset") || strings.Contains(got, "refresh --force") {
		t.Fatalf("an orphaned brain must be sent to reset, not refresh: %s", got)
	}
}

// A transient failure must still be retried, or one bad run stops the brain
// updating until something unrelated restarts the chain.
func TestATransientFailureIsNotDurable(t *testing.T) {
	t.Parallel()
	for _, err := range []error{
		wrapTestError("delta session export", os.ErrDeadlineExceeded),
		wrapTestError("delta session export", os.ErrPermission),
		nil,
	} {
		if errDurablePrepassFailure(err) {
			t.Fatalf("a transient failure classed durable would never be retried: %v", err)
		}
	}
}

// The remedy must appear exactly ONCE. loadBrainManifestForReplace attaches it
// and the worker asks for the message again, so an unconditional append printed
// the whole explanation twice. Found by weakening, not by reading: deleting the
// retired-field branch of durablePrepassMessage made the output BETTER, which is
// only possible if that branch was doing harm.
func TestTheRemedyIsStatedExactlyOnce(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeManifestWithRetiredField(t, dir)
	_, err := loadBrainManifestForReplace(dir)
	if err == nil {
		t.Fatal("fixture produced no error, so this guard would pass on anything")
	}
	for name, msg := range map[string]string{
		"retiredManifestFieldMessage": retiredManifestFieldMessage(err),
		"durablePrepassMessage":       durablePrepassMessage(err),
	} {
		if n := strings.Count(msg, "refresh --force"); n != 1 {
			t.Errorf("%s states the remedy %d times, want 1: %s", name, n, msg)
		}
	}
}

// The worker asks errDurablePrepassFailure, not the specific predicates, so a
// retired field must satisfy the general one too -- otherwise the chain keeps
// relaunching for the very case this was built for.
func TestARetiredFieldSatisfiesTheGeneralClassifier(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeManifestWithRetiredField(t, dir)
	_, err := loadBrainManifestForReplace(dir)
	if err == nil {
		t.Fatal("fixture produced no error")
	}
	if !errDurablePrepassFailure(err) {
		t.Fatalf("the worker would keep relaunching against a retired field: %v", err)
	}
}

// The worker does NOT always see the wrapped form. The live prepass error was
// bare -- "parse brain manifest: ...: json: unknown field" with no remedy --
// because the delta-export path reads the manifest without going through
// loadBrainManifestForReplace. That bare form is the one a reader actually got,
// and the one that named no action at all.
//
// Without this case the retired-field branch of durablePrepassMessage looks
// dead: every other fixture arrives with the remedy already attached, so
// deleting the branch changes nothing and the guard silently stops holding.
func TestABarePrepassErrorStillGetsTheRemedy(t *testing.T) {
	t.Parallel()
	bare := wrapTestError("delta session export",
		wrapTestError("parse brain manifest: "+memoryErrStateCorrupt+": brain manifest cannot be parsed",
			errUnknownFieldForTest))

	if strings.Contains(bare.Error(), "refresh --force") {
		t.Fatal("the fixture already carries the remedy, so it cannot show that the branch adds it")
	}
	if !errRetiredManifestField(bare) {
		t.Fatalf("the bare form must still be recognised as durable: %v", bare)
	}
	msg := durablePrepassMessage(bare)
	if n := strings.Count(msg, "refresh --force"); n != 1 {
		t.Fatalf("the bare error must gain the remedy exactly once, got %d: %s", n, msg)
	}
}

// TestAMissingRepositoryIsDurable above builds its error by hand, and the hand
// built one says "not a git repository". The REAL orphan does not: when the
// repository directory is gone the subprocess never reaches git, it fails at
// chdir with empty stderr, and runExecCommand only folds stderr into the
// message when there is some. So the string match that test exercised could
// not fire on the scenario it was written for, and ~2,650 recorded failures
// per orphaned brain kept being classified as transient.
//
// Raised as a HIGH finding on this PR, and correct. This drives the real
// runner against a directory that is not there.
func TestADeletedRepositoryDirectoryIsDurable(t *testing.T) {
	t.Parallel()

	gone := filepath.Join(t.TempDir(), "repository-was-deleted")
	_, _, runErr := ExecRunner{}.Run(context.Background(), gone, "git", "status")
	if runErr == nil {
		t.Fatal("running in a directory that does not exist must fail")
	}

	// Non-vacuity: if the raw error happened to carry the old substrings, this
	// would pass without the typed check doing anything.
	if strings.Contains(runErr.Error(), "not a git repository") ||
		strings.Contains(runErr.Error(), "repository does not exist") {
		t.Fatalf("the real error already carries the old text, so this guard proves nothing: %v", runErr)
	}

	// Wrapped the way the production chain wraps it.
	err := fmt.Errorf("delta session export: %w",
		fmt.Errorf("complete routed checkpoint discovery failed: list checkpoints: %w", runErr))

	if !errRepositoryGone(err) {
		t.Fatalf("a brain whose repository directory is gone must stop relaunching: %v", err)
	}
	if !errDurablePrepassFailure(err) {
		t.Fatalf("the worker asks the general classifier, so it must agree: %v", err)
	}
	if msg := durablePrepassMessage(err); !strings.Contains(msg, brainInvocation(os.LookupEnv, "reset")) {
		t.Fatalf("the message names no way to clear the orphan: %s", msg)
	}

	// Narrow on purpose: any other missing file is NOT durable. Calling it
	// durable sends the reader to a command that deletes the brain.
	other := fmt.Errorf("read manifest: %w", &fs.PathError{Op: "open", Path: "/x/manifest.json", Err: fs.ErrNotExist})
	if errRepositoryGone(other) {
		t.Errorf("a missing file that is not the working directory was called a gone repository: %v", other)
	}
}
