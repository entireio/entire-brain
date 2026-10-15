package cli

import (
	"bytes"
	"strings"
	"testing"
)

// The published guide documents `inspect impact --symbol ValidateToken`. No
// such flag exists -- the symbol is positional here and on the sibling
// commands -- so a reader following the docs got cobra's bare "unknown flag"
// and no reason to suspect the page rather than their own typing.
//
// The flag is deliberately NOT added: `inspect context` and `inspect tests`
// take their symbol positionally too, so accepting it on one of the three
// would bend a consistent interface around a typo.
func TestImpactNamesThePositionalFormWhenSymbolIsPassedAsAFlag(t *testing.T) {
	t.Parallel()

	f := newVerifyFixture(t)
	var out bytes.Buffer
	cmd := NewRootCommand(f.opts)
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"inspect", "impact", "--symbol", "ValidateToken"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("an unknown flag must still be an error")
	}
	got := err.Error()
	if !strings.Contains(got, "unknown flag") {
		t.Errorf("the original error must survive: %q", got)
	}
	if !strings.Contains(got, "not a flag") || !strings.Contains(got, "<symbol>") {
		t.Errorf("the error must name the positional form: %q", got)
	}
}

// Only --symbol is special-cased. Every other flag error stays exactly as it
// was, or the hint becomes noise on unrelated mistakes.
func TestOtherFlagErrorsAreUnchanged(t *testing.T) {
	t.Parallel()

	f := newVerifyFixture(t)
	var out bytes.Buffer
	cmd := NewRootCommand(f.opts)
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"inspect", "impact", "--nonsense", "X"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("an unknown flag must be an error")
	}
	if strings.Contains(err.Error(), "not a flag") {
		t.Errorf("the positional hint must not fire on unrelated flags: %q", err.Error())
	}
}

// wrapJSONErrorRendering walks every child and used to REPLACE each one's
// flag-error handler, silently discarding any a command had set for itself.
// That is how the hint above vanished without a trace. It must chain.
func TestFlagErrorHandlersChainRatherThanClobber(t *testing.T) {
	t.Parallel()

	src := readGoSourceForTest(t, "root.go")
	if !strings.Contains(src, "prior := flagErrorFuncOf(cmd)") {
		t.Error("the wrapper no longer captures a command's own flag-error handler, " +
			"so any per-command hint is discarded again")
	}
	if !strings.Contains(src, "if prior != nil {") {
		t.Error("the captured handler is never called")
	}
}
