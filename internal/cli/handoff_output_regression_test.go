package cli

import (
	"bytes"
	"encoding/json"
	"github.com/spf13/cobra"
	"strings"
	"testing"
	"time"
)

func TestHandoffRootTextAndJSONKeepSessionLimitAndProvenance(t *testing.T) {
	opts, _ := privacyRegressionCommandFixture(t, time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC))
	out, err := execute(t, NewRootCommand(opts), "brief", "--handoff", "--sessions", "1", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var packet handoffPacket
	if err := json.Unmarshal([]byte(out), &packet); err != nil {
		t.Fatal(err)
	}
	if len(packet.Sessions) != 1 || packet.Sessions[0].SessionID != shortSessionID("secret-sess") || packet.Sessions[0].Branch != "main" {
		t.Fatalf("limited latest session=%+v", packet.Sessions)
	}
	text, err := execute(t, NewRootCommand(opts), "brief", "--handoff", "--sessions", "1")
	if err != nil || !strings.Contains(text, "# Handoff — last 1 session(s)") || !strings.Contains(text, "## "+packet.Sessions[0].SessionID) || !strings.Contains(text, "main") {
		t.Fatalf("text packet=%s err=%v", text, err)
	}
}

func TestHandoffTextRendersResumptionEvidenceAndBoundsLongContent(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 30, 0, 0, time.UTC)
	packet := handoffPacket{
		Branches: []string{"main", "feature"}, LastDistilledAt: now, SessionsSinceDistill: 2,
		Warnings:       []string{"semantic source stale"},
		Sessions:       []handoffSession{{SessionID: "session-one", Branch: "main", Agent: "codex", CreatedAt: now, Request: "fix regression", Decisions: []string{"keep backwards compatibility"}, Validations: []string{"go test passed"}}},
		RecentFacts:    []factRecord{{Kind: factKindDecision, Text: "maintain stable IDs"}},
		Consolidations: []briefConsolidation{{Type: "workflow", Status: "current", Verdict: "accepted", Title: "release safely", PatternID: "pattern:release"}},
	}
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	renderHandoff(cmd, packet)
	for _, want := range []string{"across main, feature", "facts last distilled 2026-09-18; 2 session(s)", "warning: semantic source stale", "## session-one (main, codex, 2026-09-18 10:30)", "request: fix regression", "  - keep backwards compatibility", "  ✓ go test passed", "[decision] maintain stable IDs", "[workflow current/accepted] release safely   id pattern:release"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("rendered handoff missing %q: %s", want, out.String())
		}
	}
	packet.Sessions[0].Request = strings.Repeat("x", 250) + "TAIL_MUST_BE_TRUNCATED"
	packet.Sessions[0].Decisions = []string{strings.Repeat("d", 250) + "TAIL_MUST_BE_TRUNCATED"}
	packet.Sessions[0].Validations = []string{strings.Repeat("v", 250) + "TAIL_MUST_BE_TRUNCATED"}
	out.Reset()
	renderHandoff(cmd, packet)
	if strings.Contains(out.String(), "TAIL_MUST_BE_TRUNCATED") {
		t.Fatal("handoff did not bound long evidence")
	}
}
