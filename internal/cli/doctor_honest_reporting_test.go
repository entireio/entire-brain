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

// doctor_honest_reporting_test.go covers three ways `doctor` lied.
//
//  1. It printed `error` findings and exited 0, so nothing could gate on it.
//  2. Its facts check tested for a FILE, not for a readable store, so it
//     handed out a clean bill of health on a store `facts status` and `recall`
//     both refused to read -- the store `status` had just sent the user here
//     to understand.
//  3. It warned on conditions no user action could ever clear, and warned
//     without saying why, so a healthy install was indistinguishable from a
//     broken one.

// --- 1. the exit code -------------------------------------------------------

// TestDoctorExitsNonZeroOnErrorFindings is the CI contract. Before the fix
// this returned nil: doctor printed
//
//	facts: error (1 fact(s) declared across 1 branch(es), but the store is missing for: feature)
//
// and exited 0, which makes it useless as a gate and actively misleading in a
// script that checks the status.
func TestDoctorExitsNonZeroOnErrorFindings(t *testing.T) {
	opts, _, brainDir := factsAvailabilityFixture(t)
	removeFactStore(t, brainDir, "feature")

	out, err := execute(t, NewRootCommand(opts), "doctor")
	if !errors.Is(err, errDoctorGate) {
		t.Fatalf("doctor returned %v on a report containing an `error` finding, want the gate error\n%s", err, out)
	}
	if !strings.Contains(out, "facts: error") {
		t.Fatalf("doctor must print the WHOLE report before failing the gate:\n%s", out)
	}
	// Consistent with `status --fail-on`: the report already named the failing
	// check, so the gate does not print a second copy of the news.
	if !errors.Is(err, errRenderedCommand) {
		t.Fatalf("doctor gate error must be marked rendered, got %v", err)
	}
}

// TestDoctorFailOnNoneRestoresTheReportOnlyBehaviour: callers that only want
// the report keep it, explicitly.
func TestDoctorFailOnNoneRestoresTheReportOnlyBehaviour(t *testing.T) {
	opts, _, brainDir := factsAvailabilityFixture(t)
	removeFactStore(t, brainDir, "feature")

	out, err := execute(t, NewRootCommand(opts), "doctor", "--fail-on", "none")
	if err != nil {
		t.Fatalf("doctor --fail-on none: %v\n%s", err, out)
	}
	if !strings.Contains(out, "facts: error") {
		t.Fatalf("doctor --fail-on none must still report the error:\n%s", out)
	}
}

// TestDoctorJSONEmitsTheReportBeforeFailingTheGate mirrors
// TestStatusFailOnUnsafeAndReleaseEmitJSONBeforeError: the machine-readable
// contract is complete on stdout even when the command exits nonzero.
func TestDoctorJSONEmitsTheReportBeforeFailingTheGate(t *testing.T) {
	opts, _, brainDir := factsAvailabilityFixture(t)
	removeFactStore(t, brainDir, "feature")

	out, err := execute(t, NewRootCommand(opts), "doctor", "--json")
	if !errors.Is(err, errDoctorGate) {
		t.Fatalf("doctor --json returned %v, want the gate error\n%s", err, out)
	}
	report := decodeDoctorReport(t, out)
	state, ok := factsDoctorCheckState(report.Checks)
	if !ok || state != "error" {
		t.Fatalf("doctor --json facts check = %q present=%v, want error\n%s", state, ok, out)
	}
}

func TestDoctorRejectsAnUnknownFailOnValue(t *testing.T) {
	opts, _, _ := factsAvailabilityFixture(t)
	out, err := execute(t, NewRootCommand(opts), "doctor", "--fail-on", "sometimes")
	if err == nil || !strings.Contains(err.Error(), "--fail-on must be one of") {
		t.Fatalf("doctor --fail-on sometimes = %v, want a value error\n%s", err, out)
	}
	for _, accepted := range []string{doctorFailOnError, doctorFailOnWarn, doctorFailOnNone} {
		if !strings.Contains(err.Error(), accepted) {
			t.Fatalf("the --fail-on error does not name %q: %v", accepted, err)
		}
	}
}

// TestDoctorGatePolicies pins the gate itself, away from any machine's PATH,
// brain or filesystem: `error` is the default and does not fail on warnings,
// `warn` is stricter, `none` never fails.
func TestDoctorGatePolicies(t *testing.T) {
	clean := doctorReport{
		Dirs:   []doctorCheckResult{{Name: "plugin data dir", State: "ok"}},
		Checks: []doctorCheckResult{{Name: "facts", State: "ok"}},
	}
	warned := doctorReport{
		Dirs:   []doctorCheckResult{{Name: "plugin data dir", State: "ok"}},
		Checks: []doctorCheckResult{{Name: "capture", State: "warn"}},
	}
	errored := doctorReport{
		Dirs:   []doctorCheckResult{{Name: "plugin data dir", State: "ok"}},
		Checks: []doctorCheckResult{{Name: "facts", State: "error"}},
	}
	// A directory finding has to gate too: a caller asking "is doctor happy"
	// does not care which section noticed.
	erroredDir := doctorReport{
		Dirs: []doctorCheckResult{{Name: "plugin data dir", State: "error"}},
	}
	for _, tc := range []struct {
		name   string
		report doctorReport
		failOn string
		fails  bool
	}{
		{"clean/error", clean, doctorFailOnError, false},
		{"clean/warn", clean, doctorFailOnWarn, false},
		{"warn/error", warned, doctorFailOnError, false},
		{"warn/warn", warned, doctorFailOnWarn, true},
		{"warn/none", warned, doctorFailOnNone, false},
		{"error/error", errored, doctorFailOnError, true},
		{"error/warn", errored, doctorFailOnWarn, true},
		{"error/none", errored, doctorFailOnNone, false},
		{"dir-error/error", erroredDir, doctorFailOnError, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := doctorGateFailure(tc.report, tc.failOn)
			if tc.fails != (err != nil) {
				t.Fatalf("doctorGateFailure(%s) = %v, want failure=%v", tc.failOn, err, tc.fails)
			}
			if err == nil {
				return
			}
			if !errors.Is(err, errDoctorGate) {
				t.Fatalf("gate failure is not errDoctorGate: %v", err)
			}
			// A gate that will not name what tripped it is a second riddle.
			for _, finding := range append(append([]doctorCheckResult{}, tc.report.Dirs...), tc.report.Checks...) {
				if finding.State == "ok" {
					continue
				}
				if finding.State == "warn" && tc.failOn != doctorFailOnWarn {
					continue
				}
				if !strings.Contains(err.Error(), finding.Name) {
					t.Fatalf("gate failure %q does not name the %s finding %q", err, finding.State, finding.Name)
				}
			}
		})
	}
}

func TestNormalizeDoctorFailOnDefaultsToError(t *testing.T) {
	for _, value := range []string{"", "  ", "ERROR", "Error"} {
		got, err := normalizeDoctorFailOn(value)
		if err != nil || got != doctorFailOnError {
			t.Fatalf("normalizeDoctorFailOn(%q) = %q, %v; want error, nil", value, got, err)
		}
	}
	if _, err := normalizeDoctorFailOn("unsafe"); err == nil {
		t.Fatalf("normalizeDoctorFailOn accepted a status gate name")
	}
}

// --- 2. the facts check must PARSE ------------------------------------------

// TestDoctorReportsAnUnparseableFactStore is the defect in one test: with a
// truncated final line, `facts status` and `recall` both exit 1 with
//
//	parse facts.ndjson line 1: unexpected end of JSON input
//
// `status` collapses that into "run `entire-brain doctor`", and doctor -- which
// only stat()ed the file -- answered `facts: ok (1 fact(s) across 1 branch(es))`.
func TestDoctorReportsAnUnparseableFactStore(t *testing.T) {
	opts, repoDir, brainDir := factsAvailabilityFixture(t)

	checks, _ := brainDoctorReadOnlyReport(context.Background(), opts, repoDir)
	if state, ok := factsDoctorCheckState(checks); !ok || state != "ok" {
		t.Fatalf("healthy doctor facts = %q present=%v, want ok", state, ok)
	}

	truncateFactStore(t, brainDir, "feature")

	// Prove the store is really dead to the readers first, so the doctor
	// assertion below is about doctor and not about a fixture that still works.
	readerErrs := map[string]error{}
	for _, args := range [][]string{{"facts", "status"}, {"recall", "postgres", "--branch", "feature"}} {
		_, err := execute(t, NewRootCommand(opts), args...)
		readerErrs[strings.Join(args, " ")] = err
		if err == nil {
			t.Fatalf("%s still succeeded on the corrupt store; fixture is wrong", strings.Join(args, " "))
		}
	}

	checks, _ = brainDoctorReadOnlyReport(context.Background(), opts, repoDir)
	detail, state := "", ""
	for _, check := range checks {
		if check.Name == "facts" {
			state, detail = check.State, check.Detail
		}
	}
	if state != "error" {
		t.Fatalf("doctor facts = %q on a store every reader refuses to read, want error (detail %q)", state, detail)
	}
	if !strings.Contains(detail, "feature") {
		t.Fatalf("doctor must name the unreadable branch: %q", detail)
	}
	// Doctor must report what the readers report, not its own opinion: it
	// calls the same loader, so the reason is the same string.
	for name, readerErr := range readerErrs {
		if !strings.Contains(detail, readerErr.Error()) {
			t.Fatalf("doctor detail %q does not carry the reason %s gave: %v", detail, name, readerErr)
		}
	}
}

// TestDoctorExitsNonZeroOnAnUnparseableFactStore joins the two defects: the
// end-to-end behaviour a CI job actually observes.
func TestDoctorExitsNonZeroOnAnUnparseableFactStore(t *testing.T) {
	opts, _, brainDir := factsAvailabilityFixture(t)
	truncateFactStore(t, brainDir, "feature")

	out, err := execute(t, NewRootCommand(opts), "doctor")
	if !errors.Is(err, errDoctorGate) {
		t.Fatalf("doctor on a corrupt fact store returned %v, want the gate error\n%s", err, out)
	}
	if !strings.Contains(out, "facts: error") {
		t.Fatalf("doctor still reports a clean facts layer:\n%s", out)
	}
}

// TestUnreadableFactBranchStoresIsDisjointFromMissing keeps the two failure
// modes from being conflated: an absent store is not an unreadable one, and
// each is reported in its own words.
func TestUnreadableFactBranchStoresIsDisjointFromMissing(t *testing.T) {
	_, _, brainDir := factsAvailabilityFixture(t)
	source := &factSourceManifest{Facts: 1, Branches: []string{"feature"}}

	if got := unreadableFactBranchStores(brainDir, source); len(got) != 0 {
		t.Fatalf("healthy store reported unreadable: %+v", got)
	}

	truncateFactStore(t, brainDir, "feature")
	unreadable := unreadableFactBranchStores(brainDir, source)
	if len(unreadable) != 1 || unreadable[0].Branch != "feature" || unreadable[0].Err == nil {
		t.Fatalf("unreadableFactBranchStores = %+v, want one failing branch", unreadable)
	}
	if got := missingFactBranchStores(brainDir, source); len(got) != 0 {
		t.Fatalf("a present-but-corrupt store must not be reported as missing: %v", got)
	}

	removeFactStore(t, brainDir, "feature")
	if got := unreadableFactBranchStores(brainDir, source); len(got) != 0 {
		t.Fatalf("an absent store must not be reported as unreadable: %+v", got)
	}
	if got := missingFactBranchStores(brainDir, source); len(got) != 1 {
		t.Fatalf("missingFactBranchStores = %v, want [feature]", got)
	}
}

// --- 3. warnings that mean something ----------------------------------------

// TestDoctorReconciliationWarningCarriesItsReason is the "warn with no reason
// at all" bug, and it was a type bug, not a wording one: the projection
// receipt's state is a projectionStateReadState (a named string type), and
// doctor read it with a `.(string)` assertion that failed for every state
// except the literal "current" that memoryAggregateHealth writes itself. So
// every non-current reconciliation printed
//
//	memory_reconciliation: warn
//
// with an empty detail, while the JSON payload beside it said "absent".
func TestDoctorReconciliationWarningCarriesItsReason(t *testing.T) {
	brainDir := t.TempDir()
	now := time.Date(2026, 8, 10, 2, 0, 0, 0, time.UTC)
	// A brain whose manifest declares a projection receipt that is not on
	// disk: the ordinary "this brain was never reconciled here" state, and the
	// one a user hits after a partial restore.
	if err := writeBrainManifestAndReadme(brainDir, exportManifest{
		SchemaVersion: brainManifestSchemaVersion,
		GeneratedAt:   now,
		Sources: &brainSources{History: &historySourceManifest{
			GeneratedAt:           now,
			ProjectionStateDigest: "sha256:" + strings.Repeat("ab", sha256HexPairsForTest),
		}},
	}); err != nil {
		t.Fatal(err)
	}
	snapshot := memoryReadOnlyHealth(brainDir, now)

	reconciliation, ok := snapshot.Payload["reconciliation"].(map[string]any)
	if !ok {
		t.Fatalf("no reconciliation payload: %#v", snapshot.Payload)
	}
	// The payload itself must carry a plain string, or every reader doing the
	// obvious assertion silently gets "".
	if _, isString := reconciliation["state"].(string); !isString {
		t.Fatalf("reconciliation state is %T, not a string; readers asserting .(string) will see \"\"", reconciliation["state"])
	}

	checks := memoryDoctorChecks(snapshot)
	var found bool
	for _, check := range checks {
		if check.Name != "memory_reconciliation" {
			continue
		}
		found = true
		if check.State == "ok" {
			t.Fatalf("reconciliation reported ok with no receipt on disk: %+v", check)
		}
		if strings.TrimSpace(check.Detail) == "" {
			t.Fatalf("memory_reconciliation: %s with no reason at all -- the reader cannot act on it", check.State)
		}
		if !strings.Contains(check.Detail, string(projectionStateAbsent)) {
			t.Fatalf("memory_reconciliation detail %q does not name the observed state", check.Detail)
		}
		// A state is not advice; the reader is owed the repair.
		if !strings.Contains(check.Detail, projectionStateAction(projectionStateAbsent)) {
			t.Fatalf("memory_reconciliation detail %q does not say how to clear it", check.Detail)
		}
	}
	if !found {
		t.Fatalf("no memory_reconciliation check: %+v", checks)
	}
}

// TestDoctorDoesNotWarnOnChecksThatCanNeverPass covers the two permanent
// warnings. Both were unreachable-by-construction:
//
//   - memory_install warned on "present_unproven", which is the SUCCESS state
//     (the executable was found on PATH). Doctor never executes it, so no user
//     action could ever clear the warning.
//   - memory_host_adapter required observability != "not_observable", and
//     memoryInstallHealth hard-codes it to "not_observable".
func TestDoctorDoesNotWarnOnChecksThatCanNeverPass(t *testing.T) {
	checks := memoryDoctorChecks(memoryReadOnlyHealthSnapshot{Payload: map[string]any{
		"install": map[string]any{
			"entire_binary": memoryInstallBinaryHealth{State: "present_unproven", Path: "/usr/local/bin/entire"},
			"host_adapter":  memoryHostAdapterHealth{State: "not_observable", Authority: "entire-cli", Observability: "not_observable"},
		},
	}})
	byName := map[string]doctorCheckResult{}
	for _, check := range checks {
		byName[check.Name] = check
	}
	if got := byName["memory_install"]; got.State != "ok" {
		t.Fatalf("memory_install = %+v with the executable found on PATH, want ok", got)
	}
	if got := byName["memory_install"]; !strings.Contains(got.Detail, "/usr/local/bin/entire") {
		t.Fatalf("memory_install must still say where it found the executable: %+v", got)
	}
	if got := byName["memory_host_adapter"]; got.State != "ok" {
		t.Fatalf("memory_host_adapter = %+v for a check this process never performs, want ok", got)
	}
	// Not silenced -- it still says the Entire CLI owns the answer.
	if got := byName["memory_host_adapter"]; !strings.Contains(got.Detail, "Entire CLI") {
		t.Fatalf("memory_host_adapter dropped the pointer to the authority: %+v", got)
	}
}

// TestDoctorStillReportsABrokenEntireInstall is the other half: the states
// that ARE problems must survive the de-noising.
func TestDoctorStillReportsABrokenEntireInstall(t *testing.T) {
	for _, state := range []string{"not_found", "unsafe_relative_path", "lookup_failed"} {
		checks := memoryDoctorChecks(memoryReadOnlyHealthSnapshot{Payload: map[string]any{
			"install": map[string]any{
				"entire_binary": memoryInstallBinaryHealth{State: state, RecommendedAction: "install Entire"},
			},
		}})
		var got doctorCheckResult
		for _, check := range checks {
			if check.Name == "memory_install" {
				got = check
			}
		}
		if got.State != "error" {
			t.Fatalf("memory_install for %q = %+v, want error", state, got)
		}
		if strings.TrimSpace(got.Detail) == "" {
			t.Fatalf("memory_install error for %q carries no reason", state)
		}
	}
}

// TestDoctorDirectoryFindingReportsEvidenceNotEpistemics: a plugin directory
// that does not exist yet is the normal state of a fresh install. It used to
// be a warning that no action could clear, phrased in the internal vocabulary
// ("creatable_unproven", "status performs no probe write"). It is only a
// warning when the mode bits actually say the next write will fail -- which is
// checkable without a probe write, and is what the check is for.
func TestDoctorDirectoryFindingReportsEvidenceNotEpistemics(t *testing.T) {
	present := t.TempDir()
	absent := filepath.Join(present, "not-created-yet")

	found := doctorDirectoryFinding("plugin data dir", inspectAbsoluteInstallDirectory(present))
	if found.State != "ok" {
		t.Fatalf("an existing writable plugin dir = %+v, want ok", found)
	}
	if !strings.Contains(found.Detail, present) {
		t.Fatalf("directory finding must name the path: %+v", found)
	}

	creatable := doctorDirectoryFinding("plugin cache dir", inspectAbsoluteInstallDirectory(absent))
	if creatable.State != "ok" {
		t.Fatalf("a not-yet-created plugin dir under a writable parent = %+v, want ok", creatable)
	}
	for _, jargon := range []string{"creatable_unproven", "present_unproven", "probe write"} {
		if strings.Contains(creatable.Detail, jargon) || strings.Contains(found.Detail, jargon) {
			t.Fatalf("directory finding still speaks internal epistemics (%q): %+v / %+v", jargon, found, creatable)
		}
	}

	// The JSON contract keeps the epistemic state; only the human line changed.
	if health := inspectAbsoluteInstallDirectory(absent); health.State != "creatable_unproven" {
		t.Fatalf("directory health state = %q, want the unchanged machine contract", health.State)
	}

	unsafe := filepath.Join(present, "a-file-not-a-dir")
	if err := os.WriteFile(unsafe, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := doctorDirectoryFinding("plugin state dir", inspectAbsoluteInstallDirectory(unsafe)); got.State != "error" {
		t.Fatalf("a plugin dir path that is a regular file = %+v, want error", got)
	}
}

// --- helpers ----------------------------------------------------------------

// sha256HexPairsForTest builds a syntactically valid sha256 identity (32 hex
// pairs) so the receipt loader reaches the on-disk check instead of rejecting
// the manifest digest outright.
const sha256HexPairsForTest = 32

// truncateFactStore cuts the branch's facts.ndjson mid-record, the shape a
// half-written or partially restored store really has.
func truncateFactStore(t *testing.T, brainDir, branch string) {
	t.Helper()
	path := filepath.Join(brainDir, filepath.FromSlash(factsFileRelPath(branch)))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 40 {
		t.Fatalf("fact store too small to truncate meaningfully: %d bytes", len(data))
	}
	if err := os.WriteFile(path, data[:len(data)/2], 0o600); err != nil {
		t.Fatal(err)
	}
}

func decodeDoctorReport(t *testing.T, out string) doctorReport {
	t.Helper()
	var report doctorReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("decode doctor JSON: %v\n%s", err, out)
	}
	return report
}
