package apiurl

import (
	"strings"
	"testing"
)

func TestValidateNeverPrintsMalformedURLCredentials(t *testing.T) {
	t.Setenv(EnvAllowInsecure, "")
	for _, raw := range []string{
		"http://alice:audit-secret@example.com",
		"https://audit-secret@example.com",
		"ftp://alice:audit-secret@example.com",
		"https://alice:audit-secret@example.com:bad",
		"https://alice:audit-secret@example.com/%zz",
		"https://alice:audit-secret@invalid\nhost",
	} {
		t.Run(raw, func(t *testing.T) {
			_, err := Validate(raw)
			if err == nil || !strings.Contains(err.Error(), "api_url:") {
				t.Fatalf("expected URL refusal: %v", err)
			}
			if strings.Contains(err.Error(), "audit-secret") {
				t.Fatalf("credential leaked: %v", err)
			}
		})
	}
}
