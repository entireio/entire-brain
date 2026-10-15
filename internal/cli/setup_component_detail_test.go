package cli

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/entireio/entire-brain/internal/entityindex"
	"github.com/entireio/entire-brain/internal/providerdetail"
)

// hostHelpScreen is a stand-in for what ANY stage that shells out to another
// program can hand back: that program's whole stderr.
func hostHelpScreen() string {
	var b strings.Builder
	b.WriteString("entire [graph version --json]: exit status 1: Usage:\n  entire [flags]\n\nAvailable Commands:\n")
	for i := 0; i < 50; i++ {
		b.WriteString("  cmd  does a thing\n")
	}
	b.WriteString("\nError: Invalid usage: unknown command \"graph\" for \"entire\"\n\nDid you mean this?\n\tgrant\n")
	return b.String()
}

// TestSetupComponentFailureDetailIsAlwaysOneLine is the CLASS guard.
//
// This defect existed because ONE component (the semantic index) had been
// taught to summarise a subprocess dump and its neighbour (the entity index)
// had not, and nothing asserted the property across components -- so the fix
// for one did not protect the other. Assert it for EVERY setup component, at
// every place a reader sees the detail: the progress line, the summary block
// and the non-zero-exit reason string.
func TestSetupComponentFailureDetailIsAlwaysOneLine(t *testing.T) {
	t.Parallel()
	const maxDetailLines = 1

	names := make([]string, 0, len(setupComponentLabels))
	for name := range setupComponentLabels {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) < 2 {
		t.Fatalf("fixture is vacuous: %d component(s) to check", len(names))
	}

	dump := hostHelpScreen()
	if strings.Count(dump, "\n") < 50 {
		t.Fatalf("fixture is vacuous: the dump is only %d lines", strings.Count(dump, "\n")+1)
	}

	for _, name := range names {
		component := newSetupComponent(name, errors.New(dump), setupBrainBinaryName)
		if !component.failed() {
			t.Fatalf("%s: a non-nil error produced state %q", name, component.State)
		}
		checks := map[string]string{
			"Detail":       component.Detail,
			"failure line": setupComponentFailureLine(component, "--", setupBrainBinaryName),
			"exit reason":  setupInstantFailureReasons(nil, []setupComponent{component}),
			"summary row":  setupComponentLabel(component.Name) + ": " + component.Detail,
		}
		for where, text := range checks {
			if lines := strings.Count(text, "\n") + 1; lines > maxDetailLines {
				t.Errorf("%s: %s spans %d lines, want <= %d:\n%s", name, where, lines, maxDetailLines, text)
			}
			if strings.Contains(text, "Available Commands") || strings.Contains(text, "Did you mean this?") {
				t.Errorf("%s: %s still carries the host CLI's help screen:\n%s", name, where, text)
			}
		}
		if !strings.Contains(component.Detail, `unknown command "graph"`) {
			t.Errorf("%s: summarising threw away the cause: %q", name, component.Detail)
		}
	}
}

// TestSetupComponentFlattenKeepsASummarisedDetailWhole: the class guard is a
// net for the component nobody has fixed yet, not a truncator. A producer that
// already composed one line -- summary PLUS the remedy sentence, deliberately
// longer than a summary cap -- must reach the reader intact.
func TestSetupComponentFlattenKeepsASummarisedDetailWhole(t *testing.T) {
	t.Parallel()
	detail := semanticProviderUnverifiedError("entire", []semanticWarning{{
		Code: "provider_doctor_failed", Detail: hostHelpScreen(),
	}}).Error()
	if len([]rune(detail)) <= providerdetail.MaxSummary {
		t.Fatalf("fixture is vacuous: the detail is only %d runes", len([]rune(detail)))
	}
	component := newSetupComponent(brainComponentSemantic, errors.New(detail), setupBrainBinaryName)
	if component.Detail != detail {
		t.Fatalf("an already-summarised detail was altered:\n got %q\nwant %q", component.Detail, detail)
	}
}

// identityFailingRunner fails the provider probe with a host-CLI usage dump.
type identityFailingRunner struct{}

func (identityFailingRunner) Run(context.Context, string, string, ...string) ([]byte, []byte, error) {
	return nil, nil, errors.New(hostHelpScreen())
}

// TestEntityAndSemanticAgreeOnMissingProvider: two components, one cause, one
// message. Before the fix the semantic index named the install step in one
// line while the entity index printed sixty lines of someone else's help.
func TestEntityAndSemanticAgreeOnMissingProvider(t *testing.T) {
	t.Parallel()
	remedy := providerdetail.MissingRemedy("entire", func(string) (string, bool) { return "", false })

	semantic := semanticProviderUnverifiedError("entire", []semanticWarning{{
		Code: "provider_doctor_failed", Detail: hostHelpScreen(),
	}}).Error()
	_, entityErr := entityindex.ProviderIdentity(context.Background(), identityFailingRunner{}, t.TempDir(), "entire")
	if entityErr == nil {
		t.Fatal("entity index reported no error for a missing provider")
	}
	entity := entityErr.Error()

	for label, text := range map[string]string{"semantic index": semantic, "entity index": entity} {
		if !strings.Contains(text, remedy) {
			t.Errorf("%s does not carry the shared remedy sentence:\n got %s\nwant ...%s", label, text, remedy)
		}
		if lines := strings.Count(text, "\n") + 1; lines != 1 {
			t.Errorf("%s spans %d lines, want 1:\n%s", label, lines, text)
		}
	}
}
