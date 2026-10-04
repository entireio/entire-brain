package cli

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// entire-graph refuses git metadata subprocesses in a PARTIAL CLONE, because a
// promisor remote means an ordinary git command can silently fetch over the
// network while the provider runs --no-network. The refusal is right; its
// output is not. The provider emits an empty commit and tree with warnings[]
// and partial_failures[] both EMPTY, so nothing on the wire says why, and brain
// refused with "semantic snapshot header missing commit" -- a symptom, leaving
// the user to guess.
//
// Measured by toggling one config section on a single repository: with
// promisor=true and partialclonefilter set the commit comes back "", without
// them it resolves, and resolves again when they are removed. A partial clone
// is ordinary practice on a large repository, so this silently costs that
// repository its whole semantic index.
func TestHeaderRefusalNamesAPartialClone(t *testing.T) {
	t.Parallel()

	base := errors.New("semantic snapshot header missing commit")

	// A partial clone: the cause is named, and so is the remedy.
	partial := annotateSemanticHeaderRefusal(context.Background(),
		fakeGitConfigRunner{out: "remote.origin.promisor true\n"}, t.TempDir(), base)
	if partial == nil {
		t.Fatal("the original error must survive annotation")
	}
	got := partial.Error()
	if !strings.Contains(got, "missing commit") {
		t.Errorf("the original error must still be readable: %q", got)
	}
	// The assertion is on SUBSTANCE, not wording: name the shape, the config
	// key that identifies it, and the remedy. "no network" was dropped from the
	// message deliberately -- this string is recorded as a freshness axis detail
	// and must fit statusCauseWidth, which a repo guard enforces, so the
	// reasoning lives in the code comment where length is free.
	for _, want := range []string{"partial clone", "promisor", "--filter"} {
		if !strings.Contains(got, want) {
			t.Errorf("the annotation must mention %q: %q", want, got)
		}
	}
	// errors.Is must still find the original, or callers matching on it break.
	if !errors.Is(partial, base) {
		t.Error("the annotation must wrap the original error, not replace it")
	}

	// An ordinary clone: no annotation, because a wrong cause is worse than none.
	ordinary := annotateSemanticHeaderRefusal(context.Background(),
		fakeGitConfigRunner{out: ""}, t.TempDir(), base)
	if ordinary.Error() != base.Error() {
		t.Errorf("a full clone must not be told it is a partial one: %q", ordinary.Error())
	}

	// A git probe that fails must not replace a real error with a worse one.
	broken := annotateSemanticHeaderRefusal(context.Background(),
		fakeGitConfigRunner{err: errors.New("git exploded")}, t.TempDir(), base)
	if broken.Error() != base.Error() {
		t.Errorf("a failed probe must leave the error untouched: %q", broken.Error())
	}

	// An unrelated error is passed through: this annotation is only about a
	// header the provider could not stamp.
	other := errors.New("semantic snapshot header missing repo_key")
	if got := annotateSemanticHeaderRefusal(context.Background(),
		fakeGitConfigRunner{out: "remote.origin.promisor true\n"}, t.TempDir(), other); got.Error() != other.Error() {
		t.Errorf("an unrelated error must pass through unchanged: %q", got.Error())
	}
	if annotateSemanticHeaderRefusal(context.Background(), fakeGitConfigRunner{}, t.TempDir(), nil) != nil {
		t.Error("a nil error must stay nil")
	}
}

// fakeGitConfigRunner answers the one `git config --get-regexp` probe this
// annotation makes, so the test does not need a real partial clone to exercise
// both branches.
type fakeGitConfigRunner struct {
	out string
	err error
}

func (f fakeGitConfigRunner) Run(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
	if f.err != nil {
		return nil, nil, f.err
	}
	return []byte(f.out), nil, nil
}

// THE CALL SITE. The test above exercises the annotation directly and stays
// green when the call site is removed -- a correct diagnostic nothing invokes
// is indistinguishable from no diagnostic, and that gap has bitten this stack
// repeatedly.
func TestSemanticHeaderValidationGoesThroughTheAnnotation(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("semantic.go")
	if err != nil {
		t.Fatalf("read semantic.go: %v", err)
	}
	src := string(data)
	if !strings.Contains(src, "func annotateSemanticHeaderRefusal") {
		t.Fatal("read the wrong file; this guard would be vacuous")
	}
	if !strings.Contains(src, "return annotateSemanticHeaderRefusal(ctx, opts.Runner, repoDir, err)") {
		t.Error("validateLiveSemanticHeader's error is returned unannotated, so a partial clone still reports " +
			"only \"missing commit\" and the user has to guess the cause")
	}
}
