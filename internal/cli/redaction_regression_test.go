package cli

import (
	"strings"
	"testing"
)

func TestRedactQuotedSecretAssignments(t *testing.T) {
	for _, input := range []string{
		`{"API_KEY":"audit_fake_credential_123456"}`,
		`{"password": "audit fake credential value"}`,
		`PASSWORD='audit fake credential value'`,
		`{"secret": "audit escaped\" credential value"}`,
		`PASSWORD="audit unterminated credential value`,
	} {
		got := redactText(input)
		for _, secretPart := range []string{"audit", "credential", "value"} {
			if strings.Contains(got, secretPart) {
				t.Errorf("secret fragment survived: input %q, output %q", input, got)
			}
		}
		if !strings.Contains(got, "REDACTED") {
			t.Errorf("missing redaction: %q", got)
		}
	}
}

func TestHookReadsActualFailureTail(t *testing.T) {
	const marker = "ACTUAL FAILURE: final compile error"
	input := strings.Repeat("x", 2<<20) + "\n" + marker + "\n"
	got := hookReadFailureTail(strings.NewReader(input))
	if len(got) != hookFailureTailBytes || !strings.HasSuffix(got, marker+"\n") {
		t.Fatalf("got %d bytes without expected final diagnostic", len(got))
	}
}
