package entityindex

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// hostUsageDump is what the Entire CLI actually writes to stderr when it is
// asked for a `graph` command it does not have: the whole help screen, with
// the one sentence that matters marked "Error:" forty lines down, followed by
// a "Did you mean this?" for a command the reader never typed.
func hostUsageDump() string {
	var b strings.Builder
	b.WriteString(`entire [graph version --json]: exit status 1: Usage:` + "\n")
	b.WriteString("  entire [flags]\n  entire [command]\n\nAvailable Commands:\n")
	for i := 0; i < 40; i++ {
		b.WriteString("  command" + string(rune('a'+i%26)) + "   does a thing\n")
	}
	b.WriteString("\nError: Invalid usage: unknown command \"graph\" for \"entire\"\n")
	b.WriteString("\nDid you mean this?\n\tgrant\n")
	return b.String()
}

// identityErrorRunner fails `graph version --json` with a caller-supplied
// stderr, which is the only input ProviderIdentity's reporting depends on.
type identityErrorRunner struct{ err error }

func (r identityErrorRunner) Run(context.Context, string, string, ...string) ([]byte, []byte, error) {
	return nil, nil, r.err
}

func providerIdentityErrorText(t *testing.T, cause error) string {
	t.Helper()
	_, err := ProviderIdentity(context.Background(), identityErrorRunner{err: cause}, t.TempDir(), "entire")
	if err == nil {
		t.Fatal("ProviderIdentity returned no error for a failing provider")
	}
	return err.Error()
}

// TestProviderIdentityReducesUsageDumpToOneLine is the defect itself: the
// entity index embedded the host CLI's entire help screen in its failure, so
// `setup` printed it twice and buried its own report below the fold.
func TestProviderIdentityReducesUsageDumpToOneLine(t *testing.T) {
	t.Parallel()
	got := providerIdentityErrorText(t, errors.New(hostUsageDump()))
	if lines := strings.Count(got, "\n") + 1; lines != 1 {
		t.Fatalf("entity index failure spans %d lines, want 1:\n%s", lines, got)
	}
	if !strings.HasPrefix(got, "read graph identity revision: ") {
		t.Fatalf("lost the prefix callers match on: %q", got)
	}
	if strings.Contains(got, "Available Commands") || strings.Contains(got, "Did you mean this?") {
		t.Fatalf("failure still carries the host CLI's help screen: %q", got)
	}
	if !strings.Contains(got, `unknown command "graph" for "entire"`) {
		t.Fatalf("summary dropped the sentence the CLI marked Error: %q", got)
	}
}

// TestProviderIdentityMissingProviderNamesRemedy: a reader who has not
// installed entire-graph must be told so, and told the command that fixes it.
func TestProviderIdentityMissingProviderNamesRemedy(t *testing.T) {
	t.Parallel()
	got := providerIdentityErrorText(t, errors.New(hostUsageDump()))
	for _, want := range []string{
		"the entire-graph semantic provider is not installed",
		"entire has no `graph` command",
		"entire plugin install graph",
		"setup`",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("remedy missing %q in: %s", want, got)
		}
	}
	// The remedy must be runnable by the reader who hits this. The form this
	// replaced, `scripts/install.sh` from the entire-brain checkout, is not:
	// someone who installed the brain as a plugin has no source checkout.
	if strings.Contains(got, "scripts/install.sh") {
		t.Fatalf("remedy tells a plugin user to run a script from a checkout they do not have: %s", got)
	}
}

// TestProviderIdentityDetectsMarkerBelowFirstLine pins the invariant the
// shared summary rule is built on: detection runs against the FULL detail, so
// shortening what is SHOWN never changes what is RECOGNISED. The marker in a
// real host-CLI dump is forty lines down, and a detector that only read the
// summarised first line would silently stop naming the remedy.
func TestProviderIdentityDetectsMarkerBelowFirstLine(t *testing.T) {
	t.Parallel()
	dump := hostUsageDump()
	first := strings.SplitN(dump, "\n", 2)[0]
	if strings.Contains(strings.ToLower(first), "unknown command") {
		t.Fatalf("fixture is vacuous: the marker is already on the first line: %q", first)
	}
	got := providerIdentityErrorText(t, errors.New(dump))
	if !strings.Contains(got, "the entire-graph semantic provider is not installed") {
		t.Fatalf("detection missed a marker that is not on the first line: %s", got)
	}
}

// TestProviderIdentityKeepsCauseUnwrappable: summarising the text must not cost
// callers errors.Is/errors.As on the underlying failure.
func TestProviderIdentityKeepsCauseUnwrappable(t *testing.T) {
	t.Parallel()
	cause := &exec.Error{Name: "entire", Err: exec.ErrNotFound}
	_, err := ProviderIdentity(context.Background(), identityErrorRunner{err: cause}, t.TempDir(), "entire")
	if err == nil {
		t.Fatal("ProviderIdentity returned no error")
	}
	if !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("summarising the detail dropped the wrapped cause: %v", err)
	}
}
