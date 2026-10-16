package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// doctor printed 40 lines and 6.4KB on a HEALTHY brain of this repository: 34
// passing findings, six that said anything, and exactly one that named a
// command. That is how a diagnostic teaches people to stop reading it, and
// then the one line that mattered is lost among the thirty-four that did not.
//
// The default report now prints only findings a reader can act on, and every
// one of them ends with a command. The full report is one documented flag
// away, and `--json` -- the machine contract -- is untouched by either.

// healthyDoctorReport is a report with nothing wrong anywhere.
func healthyDoctorReport() doctorReport {
	return doctorReport{
		Env:            map[string]string{"ENTIRE_CLI_VERSION": "1.2.3"},
		RepoRoot:       "/repo",
		RepoRootSource: "cwd",
		Dirs: []doctorCheckResult{
			{Name: "plugin config dir", State: "ok", Detail: "/cfg"},
			{Name: "plugin data dir", State: "ok", Detail: "/data"},
		},
		// Exists: a directory that DENIES writes necessarily exists; without it
		// the fixture describes a missing path, whose remedy is its parent.
		DirectoryHealth: []memoryInstallDirectoryHealth{{Path: "/cfg", Exists: true}, {Path: "/data", Exists: true}},
		Checks: []doctorCheckResult{
			{Name: "manifest", State: "ok", Detail: "/data"},
			{Name: "capture", State: "ok", Detail: "263 exported sessions"},
			{Name: "facts", State: "ok", Detail: "1194 fact(s)"},
			{Name: "history_fts", State: "ok", Detail: "BM25 index fresh"},
		},
	}
}

// TestHealthyDoctorSaysSoInTwoLines is the bar this change was measured
// against: on a healthy brain, doctor says so, and says nothing else.
func TestHealthyDoctorSaysSoInTwoLines(t *testing.T) {
	out := &bytes.Buffer{}
	renderDoctorActionable(out, healthyDoctorReport())

	lines := nonEmptyLines(out.String())
	if len(lines) > 2 {
		t.Fatalf("a healthy brain produced %d lines, want at most 2:\n%s", len(lines), out)
	}
	if !strings.Contains(out.String(), "healthy") {
		t.Fatalf("a healthy brain must be TOLD it is healthy, not left to count passing lines:\n%s", out)
	}
	// The count is kept: "I printed nothing" and "I checked nothing" must not
	// look the same.
	if !strings.Contains(out.String(), "6 check(s) passed") {
		t.Fatalf("the short report dropped the number of checks that ran:\n%s", out)
	}
	if !strings.Contains(out.String(), "`"+doctorVerboseInvocation+"`") {
		t.Fatalf("the short report must name the flag that restores the long one:\n%s", out)
	}
}

// TestDoctorDefaultReportDropsEveryPassingLine: the saving comes from silence
// about what is fine, and from nothing else.
func TestDoctorDefaultReportDropsEveryPassingLine(t *testing.T) {
	report := healthyDoctorReport()
	report.Checks = append(report.Checks, doctorCheckResult{Name: "semantic", State: "warn", Detail: "branch_tip=stale"})

	out := &bytes.Buffer{}
	renderDoctorActionable(out, report)

	for _, name := range []string{"manifest", "capture", "facts", "history_fts", "plugin config dir", "plugin data dir"} {
		if strings.Contains(out.String(), name+": ok") {
			t.Fatalf("the short report still prints the passing finding %q:\n%s", name, out)
		}
	}
	if !strings.Contains(out.String(), "semantic: warn") {
		t.Fatalf("the short report dropped the one finding that is not ok:\n%s", out)
	}
}

// TestUnhealthyDoctorKeepsEveryActionableLine is the negative control: the
// quiet default must not be quiet about problems. A brain with many findings
// prints many lines -- nothing is capped, collapsed or summarised away.
func TestUnhealthyDoctorKeepsEveryActionableLine(t *testing.T) {
	report := healthyDoctorReport()
	report.Dirs[1] = doctorCheckResult{Name: "plugin data dir", State: "warn", Detail: "/data: mode bits currently deny writes (mode 0o500)"}
	broken := []doctorCheckResult{
		{Name: "facts", State: "error", Detail: "1 fact(s) declared, store missing for: feature"},
		{Name: "manifest", State: "error", Detail: "ERR_UNSUPPORTED_VERSION: upgrade"},
		{Name: "history_index", State: "error", Detail: "declared but unreadable"},
		{Name: "memory_schemas", State: "error", Detail: "2 observed schema health error(s)"},
		{Name: "memory_overlay", State: "warn", Detail: "stale"},
		{Name: "memory_vectors", State: "warn", Detail: "pending"},
		{Name: "conversation", State: "warn", Detail: "no conversation projection"},
		{Name: "semantic", State: "error", Detail: "store=unsafe (file is not a database)"},
		{Name: "memory_reconciliation", State: "warn", Detail: "absent"},
		{Name: "setup semantic index", State: "error", Detail: `unknown command "graph" for "entire"`},
	}
	report.Checks = broken

	out := &bytes.Buffer{}
	renderDoctorActionable(out, report)
	printed := out.String()

	for _, finding := range broken {
		if !strings.Contains(printed, finding.Name+": "+finding.State) {
			t.Fatalf("the short report dropped the actionable finding %q:\n%s", finding.Name, printed)
		}
	}
	if !strings.Contains(printed, "plugin data dir: warn") {
		t.Fatalf("the short report dropped an unwritable plugin directory:\n%s", printed)
	}
	// One line per finding, plus the summary. Nothing elided.
	if got, want := len(nonEmptyLines(printed)), len(broken)+2; got != want {
		t.Fatalf("short report printed %d lines for %d findings, want %d:\n%s", got, len(broken), want, printed)
	}
	if !strings.Contains(printed, "6 error") {
		t.Fatalf("the summary miscounted the errors:\n%s", printed)
	}
}

// TestEveryActionableLineNamesACommand is the second half of the bar: a
// finding without a remedy is a complaint, not a diagnosis.
func TestEveryActionableLineNamesACommand(t *testing.T) {
	report := healthyDoctorReport()
	report.Dirs[0] = doctorCheckResult{Name: "plugin config dir", State: "warn", Detail: "/cfg: mode bits currently deny writes (mode 0o500)"}
	report.Checks = nil
	for name := range doctorRemedies {
		report.Checks = append(report.Checks, doctorCheckResult{Name: name, State: "warn", Detail: "something is wrong"})
	}
	// A check added later with no table entry still has to carry a command.
	report.Checks = append(report.Checks,
		doctorCheckResult{Name: "a_check_invented_after_this_test", State: "error", Detail: "no table entry"},
		doctorCheckResult{Name: "setup semantic index", State: "error", Detail: "component failed"},
	)

	out := &bytes.Buffer{}
	renderDoctorActionable(out, report)

	lines := nonEmptyLines(out.String())
	for _, line := range lines[:len(lines)-1] { // all but the summary
		if strings.Count(line, "`") < 2 {
			t.Fatalf("an actionable line names no command to run: %q", line)
		}
	}
	// The path is shell-quoted now (an unquoted path with a space chmods the
	// wrong thing), so this asserts the INTENT — the chmod and the directory are
	// both named — rather than the exact concatenation it used to produce.
	if !strings.Contains(out.String(), "chmod u+w "+shellQuote("/cfg")) {
		t.Fatalf("an unwritable plugin directory must name the chmod and the path:\n%s", out)
	}
}

// TestDoctorRemediesAreRealCommands resolves the table against the live
// command tree, so a renamed verb fails the build instead of sending a user to
// a command that no longer exists.
func TestDoctorRemediesAreRealCommands(t *testing.T) {
	root := NewRootCommand(Options{Version: "test"})
	checked := 0
	for name, remedy := range doctorRemedies {
		rest, ok := strings.CutPrefix(remedy, "entire brain ")
		if !ok {
			continue // a host-CLI or shell remedy; not ours to resolve
		}
		var path []string
		for _, field := range strings.Fields(rest) {
			if strings.HasPrefix(field, "-") || strings.HasPrefix(field, "<") {
				break
			}
			path = append(path, field)
		}
		found, _, err := root.Find(path)
		if err != nil {
			t.Fatalf("doctor sends %q to `%s`, which does not exist: %v", name, remedy, err)
		}
		// Find falls back to the nearest parent, so confirm the leaf is real.
		if found.Name() != path[len(path)-1] {
			t.Fatalf("doctor sends %q to `%s`, but %q is not a command (resolved to %q)",
				name, remedy, path[len(path)-1], found.CommandPath())
		}
		for _, field := range strings.Fields(rest) {
			if !strings.HasPrefix(field, "--") {
				continue
			}
			flag := strings.TrimPrefix(strings.SplitN(field, "=", 2)[0], "--")
			if found.Flags().Lookup(flag) == nil && found.PersistentFlags().Lookup(flag) == nil && root.PersistentFlags().Lookup(flag) == nil {
				t.Fatalf("doctor sends %q to `%s`, but %s has no --%s flag", name, remedy, found.CommandPath(), flag)
			}
		}
		checked++
	}
	if checked < 15 {
		t.Fatalf("only resolved %d remedies; the extractor is probably broken", checked)
	}
	// The flag the short report advertises has to exist too.
	doctor, _, err := root.Find([]string{"doctor"})
	if err != nil || doctor.Flags().Lookup("verbose") == nil {
		t.Fatalf("the short report advertises `%s`, but doctor has no --verbose flag", doctorVerboseInvocation)
	}
}

// TestDoctorVerboseRestoresTheFullReport: everything the short report drops is
// still exactly one flag away, unchanged.
func TestDoctorVerboseRestoresTheFullReport(t *testing.T) {
	report := healthyDoctorReport()
	out := &bytes.Buffer{}
	renderDoctorVerbose(out, report)
	printed := out.String()

	for _, want := range []string{
		"ENTIRE_CLI_VERSION=1.2.3",
		"repo root: /repo (inferred from cwd)",
		"plugin config dir: ok (/cfg)",
		"manifest: ok (/data)",
		"history_fts: ok (BM25 index fresh)",
	} {
		if !strings.Contains(printed, want) {
			t.Fatalf("the full report lost %q:\n%s", want, printed)
		}
	}
}

// TestLongDetailsAreTruncatedInTheShortReportOnly: the blind-spot groups carry
// every skipped path -- 3.5KB on this repository -- which is a list to read
// under --verbose, not a wall to scroll past when looking for what to fix.
func TestLongDetailsAreTruncatedInTheShortReportOnly(t *testing.T) {
	long := "44 data files skipped (minified/bundled): " + strings.Repeat("benchmarks/agent-brain/a-very-long-path.json, ", 60)
	report := doctorReport{Checks: []doctorCheckResult{{Name: "semantic blind spots", State: "warn", Detail: long}}}

	short := &bytes.Buffer{}
	renderDoctorActionable(short, report)
	if len(short.String()) > 400 {
		t.Fatalf("the short report printed %d bytes for one finding:\n%s", len(short.String()), short)
	}
	if !strings.Contains(short.String(), "44 data files skipped") {
		t.Fatalf("truncation ate the part that says WHAT happened:\n%s", short)
	}

	full := &bytes.Buffer{}
	renderDoctorVerbose(full, report)
	if !strings.Contains(full.String(), long) {
		t.Fatalf("the full report truncated a detail it must print whole:\n%s", full)
	}
}

func nonEmptyLines(s string) []string {
	var lines []string
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// Raised as a shell-injection finding. The stated threat model is weak — the
// path comes from the reader's own ENTIRE_PLUGIN_* variables or home directory
// — but the quoting is right for a duller reason that bites far more often: an
// unquoted path containing a SPACE chmods the wrong thing, and "/Users/me/My
// Brain" is an ordinary path on a Mac.
func TestTheDirectoryRemedyIsSafeToPaste(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ name, path, want string }{
		{"space", "/Users/me/My Brain/data", `chmod u+w '/Users/me/My Brain/data'`},
		{"backtick", "/tmp/x`whoami`", "chmod u+w '/tmp/x`whoami`'"},
		{"semicolon", "/tmp/a;rm -rf b", `chmod u+w '/tmp/a;rm -rf b'`},
	} {
		if got := doctorDirectoryRemedy(memoryInstallDirectoryHealth{Path: c.path, Exists: true}); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
	// A path containing a single quote must close, escape and reopen, or the
	// quoting itself becomes the injection.
	got := doctorDirectoryRemedy(memoryInstallDirectoryHealth{Path: "/tmp/it's", Exists: true})
	if strings.Contains(got, "it's'") && !strings.Contains(got, `'\''`) {
		t.Errorf("an embedded single quote escapes the quoting: %q", got)
	}
	// And an empty path must still fall back rather than suggest chmod on ''.
	if r := doctorDirectoryRemedy(memoryInstallDirectoryHealth{Path: "  "}); strings.Contains(r, "chmod") {
		t.Errorf("an empty path produced a chmod suggestion: %q", r)
	}
}

// Not every unhealthy directory is a permission problem, and chmod was being
// suggested for all of them. Raised as a finding, and correct.
func TestTheDirectoryRemedyMatchesTheFailure(t *testing.T) {
	t.Parallel()

	// Does not exist: the detail names the nearest existing PARENT as the thing
	// denying writes, so chmodding the missing path is advice about nothing.
	// Rooted in a real temp directory so the ancestor is deterministic: the
	// remedy climbs to the nearest EXISTING one, which for a literal
	// "/var/root/..." depends on the host. Separator handling comes free from
	// filepath.Join -- spelling it as "/var/..." made this unsatisfiable on
	// Windows, where Dir yields backslashes.
	existingRoot := t.TempDir()
	missingPath := filepath.Join(existingRoot, "entire", "plugins", "data")
	missing := doctorDirectoryRemedy(memoryInstallDirectoryHealth{
		Path: missingPath, State: "creatable_unproven", Exists: false,
	})
	if !strings.Contains(missing, shellQuote(existingRoot)) {
		t.Errorf("a path that cannot be created must point at an ancestor that exists: %q", missing)
	}
	if strings.Contains(missing, shellQuote(missingPath)) {
		t.Errorf("chmod was suggested on a path that does not exist: %q", missing)
	}

	// Unsafe: a symlink or non-directory is not fixed by a mode change.
	unsafe := doctorDirectoryRemedy(memoryInstallDirectoryHealth{
		Path: "/tmp/linked", State: "unsafe", Exists: true,
	})
	if strings.Contains(unsafe, "chmod") {
		t.Errorf("chmod cannot fix an unsafe path: %q", unsafe)
	}

	// This used to assert that an explicit RecommendedAction always won, which
	// an earlier finding asked for. That was wrong, and the model it encoded is
	// corrected here rather than deleted: the field is never empty on a real
	// run, so "always wins" meant the remedy was ALWAYS prose, rendered inside
	// "run `...`" as though a sentence were a command. The guidance belongs in
	// the Detail, which doctorDirectoryFinding already carries.
	own := doctorDirectoryRemedy(memoryInstallDirectoryHealth{
		Path: "/tmp/x", State: "unavailable", Exists: true, RecommendedAction: "inspect the directory path",
	})
	if own == "inspect the directory path" {
		t.Errorf("a prose recommendation was returned as the command to run: %q", own)
	}
	if !strings.HasPrefix(own, "chmod u+w ") {
		t.Errorf("the remedy is not a runnable command: %q", own)
	}

	// The ordinary case is unchanged.
	plain := doctorDirectoryRemedy(memoryInstallDirectoryHealth{Path: "/cfg", State: "denied", Exists: true})
	if plain != "chmod u+w "+shellQuote("/cfg") {
		t.Errorf("an existing unwritable directory must still name the chmod: %q", plain)
	}
}

// The fixture-built cases above all leave RecommendedAction empty. No real
// doctor run ever does: every constructor seeds the field and then overwrites
// it, so an earlier version of this remedy returned that prose for EVERY real
// finding and the chmod branches were unreachable. The line rendered as
//
//	run `grant write access before running a mutation; mode bits currently deny writes`
//
// Raised as a HIGH finding on this PR, and correct. This drives the REAL
// inspection functions instead, which is the only way that defect is visible.
func TestTheRealInspectionProducesARunnableRemedy(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores mode bits, so no directory here is unwritable")
	}
	t.Parallel()

	denied := filepath.Join(t.TempDir(), "denied")
	if err := os.Mkdir(denied, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(denied, 0o700) })

	for name, probe := range map[string]struct{ path, wantCmd, wantTarget string }{
		"exists but denies writes": {denied, "chmod u+w ", denied},
		// "child" does not exist either, so the remedy must name `denied`, the
		// nearest ancestor that does.
		"cannot be created": {filepath.Join(denied, "child", "data"), "chmod u+w ", denied},
	} {
		health := inspectAbsoluteInstallDirectory(probe.path)
		if health.RecommendedAction == "" {
			t.Fatalf("%s: the real inspection left RecommendedAction empty, so this guard proves nothing", name)
		}
		remedy := doctorDirectoryRemedy(health)
		if !strings.HasPrefix(remedy, probe.wantCmd) {
			t.Errorf("%s: remedy is not a runnable command: %q", name, remedy)
		}
		if !strings.Contains(remedy, shellQuote(probe.wantTarget)) {
			t.Errorf("%s: remedy does not name %s: %q", name, probe.wantTarget, remedy)
		}
		if remedy == health.RecommendedAction {
			t.Errorf("%s: the prose recommendation was rendered as a command: %q", name, remedy)
		}
		// The guidance is not lost -- it is the Detail this remedy is appended to.
		finding := doctorDirectoryFinding("plugin data directory", health)
		if finding.Detail == "" {
			t.Errorf("%s: the finding carries no detail, so dropping the prose loses it", name)
		}
	}
}

// Every non-ok line is supposed to end with a command the reader can run.
// doctorRemedy declined to add one whenever the detail already contained a
// backtick, and truncateDoctorDetail cut that same detail at 160 runes knowing
// nothing about where the command sat. The history_freshness warning carries an
// unbounded incompleteness clause before its command, so the printed line ended
//
//	...; run `entire brain ref…
//
// a command that cannot be run, with no remedy after it because the untruncated
// detail had a backtick. Raised as a finding on this PR, and correct.
func TestATruncatedDetailNeverPrintsHalfACommand(t *testing.T) {
	t.Parallel()

	detail := "short-term memory covers the gap only partially (3 of 11 transcripts consolidated; " +
		"8 pending and 2 unreadable after the last delta build); run `entire brain refresh` to consolidate"
	if len([]rune(detail)) <= doctorActionableDetailBudget {
		t.Fatalf("fixture must exceed the budget: %d <= %d", len([]rune(detail)), doctorActionableDetailBudget)
	}

	check := doctorCheckResult{Name: "history_freshness", State: "warn", Detail: detail}
	printed := doctorActionableLine{finding: check, remedy: doctorRemedy(check)}.detail()

	// Backticks are balanced: an odd count is a span that was cut open.
	if strings.Count(printed, "`")%2 == 1 {
		t.Errorf("the line ends inside an unterminated command span: %s", printed)
	}
	if !strings.HasSuffix(printed, "`") {
		t.Errorf("the line does not end with a command: %s", printed)
	}
	if strings.Contains(printed, "run`") || strings.Contains(printed, "run…") {
		t.Errorf("a dangling lead-in was left where the command was cut: %s", printed)
	}

	// A detail that keeps whole commands after truncation gets no second one.
	twoCommands := "exported sessions changed since the last index build; run `entire brain refresh delta` for " +
		"immediate freshness or `entire brain refresh` to consolidate (a missed session-end hook is repaired the same way)"
	c2 := doctorCheckResult{Name: "history_freshness", State: "warn", Detail: twoCommands}
	p2 := doctorActionableLine{finding: c2, remedy: doctorRemedy(c2)}.detail()
	if strings.Count(p2, "`")%2 == 1 {
		t.Errorf("a multi-command detail was cut open: %s", p2)
	}
	if strings.Count(p2, "entire brain refresh") < 2 {
		t.Errorf("truncation dropped a command the budget could hold: %s", p2)
	}
}

// Every remedy is printed inside "run `...`". A remedy carrying an unquoted
// `<` or `>` is not a command: the shell reads it as a redirection and the line
// fails with a redirection error instead of doing anything. `repo` carried
// `cd <your repository>` and did exactly that.
//
// This is the third separate "the remedy is not actually runnable" finding on
// this branch, so it guards the CLASS rather than the instance: a placeholder
// added later has to survive being pasted.
func TestEveryRemedySurvivesBeingPasted(t *testing.T) {
	t.Parallel()

	unquoted := func(s string) string {
		var out strings.Builder
		inQuote := false
		for _, r := range s {
			if r == '\'' {
				inQuote = !inQuote
				continue
			}
			if !inQuote {
				out.WriteRune(r)
			}
		}
		return out.String()
	}

	checked := 0
	for name, remedy := range doctorRemedies {
		checked++
		if bare := unquoted(remedy); strings.ContainsAny(bare, "<>") {
			t.Errorf("remedy for %q redirects when pasted: %s", name, remedy)
		}
	}
	if checked < 15 {
		t.Fatalf("only %d remedies walked; the table is not being read", checked)
	}

	// Non-vacuity: the detector must actually catch the shape it describes.
	if bare := unquoted("cd <your repository> && entire brain doctor"); !strings.ContainsAny(bare, "<>") {
		t.Fatal("the unquoted-metacharacter check cannot see a bare redirect, so this guard proves nothing")
	}
	if bare := unquoted("cd '<your repository>' && entire brain doctor"); strings.ContainsAny(bare, "<>") {
		t.Fatal("a quoted placeholder was reported as a redirect")
	}
}

// filepath.Dir climbs exactly one level. The default data directory sits
// several missing components deep, so the suggested chmod named a directory
// that did not exist either and failed with "No such file or directory" --
// contradicting the comment's own claim of "the nearest existing parent".
// Raised as a finding on this PR, and correct.
func TestTheRemedyNamesAnAncestorThatExists(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	missing := filepath.Join(root, "entire", "plugins", "data", "brain")

	remedy := doctorDirectoryRemedy(memoryInstallDirectoryHealth{
		Path: missing, State: "creatable_unproven", Exists: false,
	})
	if !strings.Contains(remedy, shellQuote(root)) {
		t.Errorf("the remedy does not name the nearest existing ancestor %s: %s", root, remedy)
	}
	for _, absent := range []string{"entire", "plugins", "data", "brain"} {
		if strings.Contains(remedy, shellQuote(filepath.Join(root, absent))) {
			t.Errorf("the remedy names %q, which does not exist: %s", absent, remedy)
		}
	}
}
