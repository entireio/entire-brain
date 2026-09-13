package cli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// host_cli_degrade_test.go pins the promise the README makes and the binary
// broke: a brain stays refreshable on a machine with no `entire` on PATH.
//
// The reproduction was exact. On a fresh store the FIRST refresh degraded —
//
//	refresh: export sessions: unavailable, using seed baseline done
//
// because the tolerance for a failed export was gated on `manifest == nil ||
// manifest.Sources == nil`, and a fresh store has no manifest. Once that first
// run wrote one, the identical condition became terminal:
//
//	refresh: export sessions: reading local checkpoint stores failed
//	complete routed checkpoint discovery failed: list checkpoints: entire
//	[checkpoint explain --json --search-all]: exec: "entire": executable file
//	not found in $PATH
//
// (`$PATH` there is the Unix spelling of a message exec.Error writes per-OS;
// Windows says `%PATH%`. Nothing below asserts on it.)
//
// exit 1, forever, while `status` went on printing "run `entire-brain refresh
// --agent none`" — the very command that produced it.

// hostCLINotFound is what exec.Command hands back when LookPath cannot find the
// binary. Constructing the real type matters: the fix keys on the error's
// STRUCTURE, not on its rendered text, and a test that scripted a plain
// errors.New would pass against a substring match too.
func hostCLINotFound(binary string) error {
	return &exec.Error{Name: binary, Err: exec.ErrNotFound}
}

// TestHostCLIMissingSeparatesAnAbsentBinaryFromAFailedCommand is the whole
// safety argument for the degrade in one table. Forgiving too much here is the
// worse defect of the two: it would answer real data loss with "unavailable,
// using seed baseline", which is the masking refresh.go has already had to undo
// once.
func TestHostCLIMissingSeparatesAnAbsentBinaryFromAFailedCommand(t *testing.T) {
	t.Parallel()
	wrapped := func(err error) error {
		// The shape the export path actually produces: runExecCommand wraps the
		// launch failure, listRoutedCheckpoints wraps that, discoverCheckpoints
		// wraps that again.
		return errors.Join(errors.New("complete routed checkpoint discovery failed"),
			errors.New("list checkpoints: "+err.Error()), err)
	}
	for name, testCase := range map[string]struct {
		err    error
		binary string
		want   bool
	}{
		"binary not on PATH":         {err: hostCLINotFound("entire"), binary: "entire", want: true},
		"wrapped by the export path": {err: wrapped(hostCLINotFound("entire")), binary: "entire", want: true},
		"custom --entire-binary":     {err: hostCLINotFound("entire-test"), binary: "entire-test", want: true},
		"empty binary means entire":  {err: hostCLINotFound("entire"), binary: "", want: true},

		// Everything below is a host CLI that IS installed, or a different
		// binary entirely. Each one must stay fatal.
		"no error at all":     {err: nil, binary: "entire", want: false},
		"command exited 1":    {err: errors.New("entire [checkpoint explain]: exit status 1"), binary: "entire", want: false},
		"unreadable store":    {err: errors.New("reading local checkpoint stores failed"), binary: "entire", want: false},
		"a DIFFERENT binary":  {err: hostCLINotFound("git"), binary: "entire", want: false},
		"the name was reworn": {err: hostCLINotFound("entire"), binary: "entire-test", want: false},
		"permission denied":   {err: &exec.Error{Name: "entire", Err: os.ErrPermission}, binary: "entire", want: false},
		// The exact SENTENCE the missing binary produces, but carried by a plain
		// error: something in between stringified it, so nothing has proved the
		// binary was ever the cause. Text is not evidence.
		"the text without the type": {
			err:    errors.New(`exec: "entire": executable file not found in $PATH`),
			binary: "entire",
			want:   false,
		},
	} {
		if got := hostCLIMissing(testCase.err, testCase.binary); got != testCase.want {
			t.Errorf("%s: hostCLIMissing = %v, want %v", name, got, testCase.want)
		}
	}
}

// TestHostCLIMissingExportErrorNamesTheCauseAndTheRemedy holds the degraded
// message to the shape `setup` already uses for the missing semantic provider:
// a code, the condition in words, the raw detail, and the step that fixes it.
func TestHostCLIMissingExportErrorNamesTheCauseAndTheRemedy(t *testing.T) {
	t.Parallel()
	launch := hostCLINotFound("entire")
	err := hostCLIMissingExportError(launch, "entire")
	message := err.Error()
	// Only the words this package writes are asserted as words. exec.Error
	// renders the variable per-OS -- `$PATH` on Unix, `%PATH%` on Windows -- so
	// pinning its text made a green branch red on the Windows shards alone. The
	// raw detail still has to survive for a bug report, so it is asserted as
	// "whatever the platform's own error says", and the CAUSE is asserted
	// structurally, which is how the detection itself works.
	for _, want := range []string{
		hostCLIMissingCode + ":",
		"is not on PATH",
		launch.Error(),
		"install the Entire CLI",
	} {
		if !strings.Contains(message, want) {
			t.Errorf("degraded message is missing %q:\n%s", want, message)
		}
	}
	// Wrapped, not replaced: a bug report keeps the original, and the predicate
	// still recognises the result.
	if !errors.Is(err, exec.ErrNotFound) {
		t.Error("the original launch failure must survive wrapping")
	}
	if !hostCLIMissing(err, "entire") {
		t.Error("the restated error must still read as an absent host CLI")
	}
}

// refreshWithoutHostCLI runs a full `refresh` against a fixture whose Entire
// CLI is not installed, returning the combined output and the exit error.
// exportFails decides whether the host CLI is ABSENT or merely broken.
func refreshWithoutHostCLI(t *testing.T, exportErr error, args ...string) (string, string, error) {
	t.Helper()
	repoDir := seedFixtureRepo(t)
	dataDir := filepath.Join(t.TempDir(), "data")
	runner := seedFixtureRunner(repoDir)
	addRefreshSemanticFixture(runner, repoDir)
	runner.responses[fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1MainRef)] = fakeCommandResponse{err: os.ErrNotExist}
	runner.responses[fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1OriginRef)] = fakeCommandResponse{err: os.ErrNotExist}
	// The session export reaches the host CLI here.
	runner.responses[fakeCommandKey("entire-test", "checkpoint", "explain", "--json", "--search-all")] = fakeCommandResponse{err: exportErr}
	runner.responses[fakeCommandKey("entire-test", "checkpoint", "explain", "--json")] = fakeCommandResponse{err: exportErr}

	brainDir := filepath.Join(dataDir, repoStoreDirName, "gh", "example", "repo")
	opts := Options{
		Version: "test-version",
		Env: EntireEnv{
			RepoRoot:        repoDir,
			PluginConfigDir: filepath.Join(t.TempDir(), "config"),
			PluginDataDir:   dataDir,
			PluginStateDir:  filepath.Join(t.TempDir(), "state"),
			PluginCacheDir:  filepath.Join(t.TempDir(), "cache"),
		},
		Runner: runner,
	}
	// The clock ADVANCES between runs, because two refreshes never share one.
	// A frozen clock makes the second projection byte-identical to the first,
	// and the publisher then takes its duplicate-generation path — a path with
	// its own pre-existing weakness on a brain that has no sessions source, and
	// one that has nothing to do with the host CLI. Pinning an artificial clock
	// collision here would be testing the fixture, not the defect.
	clock := time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)
	opts.Now = func() time.Time { return clock }
	run := func() (string, error) {
		defer func() { clock = clock.Add(time.Minute) }()
		return execute(t, NewRootCommand(opts), append([]string{"refresh", "--entire-binary", "entire-test"}, args...)...)
	}
	// Run ONE: the first refresh on a store with no manifest. This is the run
	// that always degraded, and it is the baseline the second run must match.
	first, firstErr := run()
	if firstErr != nil {
		t.Fatalf("the FIRST refresh already fails; the fixture no longer reproduces the defect: %v\n%s", firstErr, first)
	}
	if _, err := os.Stat(filepath.Join(brainDir, exportManifestFileName)); err != nil {
		t.Fatalf("the first refresh wrote no manifest, so the second run is not the case under test: %v", err)
	}
	// Run TWO: same machine, same condition, manifest now present.
	second, secondErr := run()
	return first, second, secondErr
}

// TestRefreshDegradesWithoutTheHostCLIOnEveryRun is the defect itself: run one
// and run one hundred are the same machine, so they must be the same answer.
func TestRefreshDegradesWithoutTheHostCLIOnEveryRun(t *testing.T) {
	first, second, err := refreshWithoutHostCLI(t, hostCLINotFound("entire-test"))
	if err != nil {
		t.Fatalf("refresh over an existing manifest still fails without the host CLI: %v\n%s", err, second)
	}
	if !strings.Contains(first, "refreshed brain:") || !strings.Contains(second, "refreshed brain:") {
		t.Fatalf("both runs must finish and say so:\nFIRST\n%s\nSECOND\n%s", first, second)
	}
	for _, want := range []string{
		// The condition, on the progress line.
		"`entire` is not on PATH",
		// The named cause and the remedy, on the degraded stage's own note.
		hostCLIMissingCode + ":",
		"install the Entire CLI",
	} {
		if !strings.Contains(second, want) {
			t.Fatalf("the degraded refresh does not say %q:\n%s", want, second)
		}
	}
	if strings.Contains(second, "export sessions: unavailable, using seed baseline") {
		t.Fatalf("an absent host CLI is not a seed fallback; say which it is:\n%s", second)
	}
}

// TestRefreshStillFailsWhenTheHostCLIIsPresentAndTheExportBreaks is the other
// half, and the one that decides whether the fix is safe. The host CLI RAN and
// the export failed: that is a fault with a repair, and hiding it behind a
// progress line is the defect this whole release was cut to eliminate.
func TestRefreshStillFailsWhenTheHostCLIIsPresentAndTheExportBreaks(t *testing.T) {
	_, second, err := refreshWithoutHostCLI(t, errors.New("reading local checkpoint stores failed"))
	if err == nil {
		t.Fatalf("a genuine export failure was swallowed:\n%s", second)
	}
	if !strings.Contains(err.Error(), "reading local checkpoint stores failed") {
		t.Fatalf("the genuine failure lost its own words: %v", err)
	}
	if strings.Contains(second, hostCLIMissingCode) {
		t.Fatalf("a command that RAN must never be reported as an absent binary:\n%s", second)
	}
}

// TestSemanticProviderAbsenceFollowsTheHostCLI pins the line drawn for the
// second stage that runs the host CLI. Fixing only the export moved the same
// fatal error one stage down: `semantic index: verifying provider failed` then
// aborted the run for the identical reason.
//
// The distinction is the same one the export draws, arrived at by probe rather
// than by inference: a host CLI that is present but has no `graph` subcommand
// is an installable plugin, the error already names `scripts/install.sh`, and
// that stays fatal.
func TestSemanticProviderAbsenceFollowsTheHostCLI(t *testing.T) {
	t.Parallel()
	if hostCLISemanticProviderAbsent("some-other-graph-binary") {
		t.Error("a graph binary that is not the host CLI is not answered by the host CLI probe")
	}
	if hostCLIOnPath() != !hostCLISemanticProviderAbsent("") {
		t.Error("the default graph binary IS the host CLI, so the two answers must agree")
	}
	if hostCLIOnPath() != !hostCLISemanticProviderAbsent(entireBinaryName) {
		t.Error("naming the host CLI explicitly must answer the same as defaulting to it")
	}
}
