package cli

import (
	"io"
	"os"
	"strings"
	"sync"
	"testing"
)

func TestSecurityToggleFailsClosedOnTypos(t *testing.T) {
	cases := map[string]bool{
		"":         false,
		"0":        false,
		"false":    false,
		"off":      false,
		"disabled": false,
		"1":        true,
		"true":     true,
		"on":       true,
		"enabled":  true,
		// Unrecognized / typo'd values fail closed (treated as enabled).
		"ture":       true,
		"yes please": true,
		"2":          true,
	}
	for val, want := range cases {
		t.Setenv("ENTIRE_BRAIN_NO_EGRESS", val)
		if got := securityToggleEnabled("ENTIRE_BRAIN_NO_EGRESS"); got != want {
			t.Errorf("securityToggleEnabled(%q) = %v, want %v", val, got, want)
		}
	}
}

func TestSecurityToggleWarningRedactsUnrecognizedValue(t *testing.T) {
	securityToggleWarned = sync.Map{}
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "secret-token-value")

	out := captureStderr(t, func() {
		if !securityToggleEnabled("ENTIRE_BRAIN_NO_EGRESS") {
			t.Fatal("securityToggleEnabled() = false; want fail-closed true for unrecognized value")
		}
	})
	if strings.Contains(out, "secret-token-value") {
		t.Fatalf("warning leaked raw env value: %q", out)
	}
	if !strings.Contains(out, "ENTIRE_BRAIN_NO_EGRESS") {
		t.Fatalf("warning omitted env var name: %q", out)
	}
}

func TestRejectAgentForNoEgressDefaultDenies(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "1")
	for _, ok := range []string{"ollama", "none", ""} {
		if err := rejectAgentForNoEgress(ok); err != nil {
			t.Errorf("agent %q should be allowed under no-egress: %v", ok, err)
		}
	}
	// Known egressing agents and any unknown/future name must be denied.
	for _, bad := range []string{"codex", "claude-code", "command", "auto", "some-new-agent"} {
		if err := rejectAgentForNoEgress(bad); err == nil {
			t.Errorf("agent %q should be denied under no-egress", bad)
		}
	}
}

func TestRejectAgentForNoEgressNoopWhenDisabled(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "")
	t.Setenv("ENTIRE_BRAIN_LOCAL_ONLY", "")
	if err := rejectAgentForNoEgress("codex"); err != nil {
		t.Errorf("no-egress disabled: codex should be allowed, got %v", err)
	}
}

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	os.Stderr = w
	defer func() { os.Stderr = old }()

	fn()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}
