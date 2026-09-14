package apiurl

import (
	"strings"
	"testing"
)

// A base URL carrying userinfo is refused on every scheme. It authenticates nothing
// against this API (the hosted endpoints take a bearer token), it puts a cleartext
// password into a value this plugin prints, and net/http promotes it to an
// Authorization: Basic header on any request that does not already carry one — so a
// tokenless call would egress a credential the member never configured for it.
func TestValidateRejectsCredentialsInTheURL(t *testing.T) {
	for name, raw := range map[string]string{
		"https user and password": "https://alice:hunter2@api.entire.io",
		"https user only":         "https://alice@api.entire.io",
		"http loopback":           "http://alice:hunter2@127.0.0.1:8080",
		"https with a path":       "https://alice:hunter2@api.entire.io/api/v1",
		"empty password":          "https://alice:@api.entire.io",
	} {
		t.Run(name, func(t *testing.T) {
			got, err := Validate(raw)
			if err == nil {
				t.Fatalf("Validate(%q) = %q, nil; a base URL with credentials must be refused", raw, got)
			}
			if !strings.HasPrefix(err.Error(), "invalid_api_url:") {
				t.Errorf("error %q lacks the invalid_api_url: prefix", err)
			}
			// The refusal must not be the thing that leaks the password.
			if strings.Contains(err.Error(), "hunter2") {
				t.Errorf("the refusal printed the password in cleartext: %v", err)
			}
		})
	}
}

// The loopback carve-out and ordinary https URLs are unaffected.
func TestValidateStillAcceptsCredentialFreeURLs(t *testing.T) {
	for _, raw := range []string{
		"https://api.entire.io",
		"https://api.entire.io/",
		"http://127.0.0.1:8080",
		"http://localhost:8080",
	} {
		if _, err := Validate(raw); err != nil {
			t.Errorf("Validate(%q) = %v; want it accepted", raw, err)
		}
	}
}
