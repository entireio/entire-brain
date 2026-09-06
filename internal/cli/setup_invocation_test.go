package cli

import (
	"strings"
	"testing"
)

// TestSetupPrintsTheCommandTheReaderTyped: the brain ships as an external
// command of the Entire CLI, so a reader who types `entire brain setup` was
// answered with a Next block spelled `entire-brain overview` — and on a managed
// plugin install that spelling does not resolve in their shell at all.
//
// Reverting setup_render.go's Next block to string literals still compiles, and
// fails here.
func TestSetupPrintsTheCommandTheReaderTyped(t *testing.T) {
	t.Setenv(envCLIVersion, "1.2.3")
	f := newSetupTestFixture(t)
	printed := runSetupForTest(t, f, defaultSetupOptions(), &recordedSetup{})

	for _, want := range []string{
		setupPluginCommand + " overview",
		setupPluginCommand + ` brief "<task>"`,
		setupPluginCommand + " status",
	} {
		if !strings.Contains(printed, want) {
			t.Fatalf("a reader who typed `%s setup` must be answered in that spelling; %q missing from:\n%s", setupPluginCommand, want, printed)
		}
	}
	if strings.Contains(printed, setupBrainBinaryName+" overview") {
		t.Fatalf("setup must not hand a plugin reader a command their shell cannot resolve:\n%s", printed)
	}
}

// TestSetupKeepsTheStandaloneSpellingWithoutTheHostSignal is the other half:
// a reader who ran this binary directly may not have the Entire CLI at all, so
// the standalone name stays the honest answer.
func TestSetupKeepsTheStandaloneSpellingWithoutTheHostSignal(t *testing.T) {
	t.Setenv(envCLIVersion, "")
	f := newSetupTestFixture(t)
	printed := runSetupForTest(t, f, defaultSetupOptions(), &recordedSetup{})

	if !strings.Contains(printed, setupBrainBinaryName+" overview") {
		t.Fatalf("with no host signal setup must print its own name:\n%s", printed)
	}
	if strings.Contains(printed, setupPluginCommand+" overview") {
		t.Fatalf("setup must not invent a host CLI the reader may not have:\n%s", printed)
	}
}

// TestSetupCommandPrefixIgnoresAnEmptyMarker: exporting NAME= to clear a
// variable is how a reader turns a signal off, so a present-but-empty
// ENTIRE_CLI_VERSION is not a plugin launch.
func TestSetupCommandPrefixIgnoresAnEmptyMarker(t *testing.T) {
	t.Parallel()
	empty := func(string) (string, bool) { return "  ", true }
	if got := setupCommandPrefix(empty); got != setupBrainBinaryName {
		t.Fatalf("a blank marker is not a signal: got %q, want %q", got, setupBrainBinaryName)
	}
	set := func(string) (string, bool) { return "1.2.3", true }
	if got := setupCommandPrefix(set); got != setupPluginCommand {
		t.Fatalf("the host's unconditional marker is the signal: got %q, want %q", got, setupPluginCommand)
	}
	if got := setupCommandPrefix(nil); got != setupBrainBinaryName {
		t.Fatalf("no lookup means no signal: got %q", got)
	}
}

// TestBothSpellingsAreTheSameWidth: the summary and Next blocks are
// column-aligned, and one of these strings is substituted into every one of
// them. A future rename of the plugin verb must fail loudly here rather than
// silently clipping a column.
func TestBothSpellingsAreTheSameWidth(t *testing.T) {
	t.Parallel()
	if len(setupBrainBinaryName) != len(setupPluginCommand) {
		t.Fatalf("the two spellings must stay the same width or every width budget in setup's output shifts: %q (%d) vs %q (%d)",
			setupBrainBinaryName, len(setupBrainBinaryName), setupPluginCommand, len(setupPluginCommand))
	}
}

// TestSetupHintsFollowTheReadersSpelling sweeps the hint strings, which are the
// other place setup names a command to run and which were spelled with the
// standalone name unconditionally.
func TestSetupHintsFollowTheReadersSpelling(t *testing.T) {
	t.Parallel()
	for name, hint := range map[string]func(string) string{
		"repo key mismatch": setupRepoKeyMismatchHint,
		"dirty worktree":    setupDirtyWorktreeHint,
		"no sessions":       setupNoSessionsHint,
	} {
		text := hint(setupPluginCommand)
		if strings.Contains(text, setupBrainBinaryName+" ") {
			t.Fatalf("the %s hint must not hard-code the standalone spelling: %s", name, text)
		}
		if !strings.Contains(text, setupPluginCommand+" ") {
			t.Fatalf("the %s hint must name a command the reader can actually run: %s", name, text)
		}
	}
}

// TestRepoKeyMismatchHintNoLongerBlamesTheRepo: the old hint told the reader to
// add a git remote. It fired on repositories that already had one, and no
// remote would have reconciled the two derivations anyway — that class is now
// fixed at ingest, so the advice must not linger.
func TestRepoKeyMismatchHintNoLongerBlamesTheRepo(t *testing.T) {
	t.Parallel()
	hint := setupRepoKeyMismatchHint(setupBrainBinaryName)
	for _, dead := range []string{"no git remote", "git remote add origin", "upgrade the entire CLI"} {
		if strings.Contains(hint, dead) {
			t.Fatalf("obsolete advice %q still in the repo-key hint: %s", dead, hint)
		}
	}
}
