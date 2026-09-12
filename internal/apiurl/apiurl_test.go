package apiurl

import (
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		raw     string
		optIn   string
		want    string // normalized value on success
		wantErr string // substring of the expected error
	}{
		// https is always fine, and is normalized (trimmed, no trailing slash).
		{name: "https", raw: "https://api.entire.io", want: "https://api.entire.io"},
		{name: "https trailing slash", raw: "https://api.entire.io/", want: "https://api.entire.io"},
		{name: "https padded", raw: "  https://api.entire.io  ", want: "https://api.entire.io"},
		{name: "https with port and path", raw: "https://api.entire.io:8443/base", want: "https://api.entire.io:8443/base"},
		{name: "https uppercase scheme", raw: "HTTPS://api.entire.io", want: "HTTPS://api.entire.io"},

		// The loopback carve-out: this is what local dev and every httptest-based
		// test in this repo rely on.
		{name: "http loopback v4", raw: "http://127.0.0.1:8787", want: "http://127.0.0.1:8787"},
		{name: "http loopback v4 subnet", raw: "http://127.9.9.9:1", want: "http://127.9.9.9:1"},
		{name: "http loopback v6", raw: "http://[::1]:8787", want: "http://[::1]:8787"},
		{name: "http localhost", raw: "http://localhost:8787", want: "http://localhost:8787"},
		{name: "http localhost fqdn", raw: "http://localhost.:8787", want: "http://localhost.:8787"},
		// A "localhost" SUBDOMAIN is not carved out: RFC 6761 says it is loopback,
		// but resolvers vary, so it must take the explicit opt-in.
		// A "localhost" SUBDOMAIN is not carved out: RFC 6761 calls it loopback, but
		// resolvers vary, so it must take the explicit opt-in.
		{name: "http sub localhost", raw: "http://api.localhost:8787", wantErr: "insecure_api_url"},

		// The finding: plaintext to anywhere else is refused.
		{name: "http public host", raw: "http://evil.example", wantErr: "insecure_api_url"},
		{name: "http public ip", raw: "http://198.51.100.7:8080", wantErr: "insecure_api_url"},
		{name: "http lan ip", raw: "http://192.168.1.121:8080", wantErr: "insecure_api_url"},
		{name: "http public v6", raw: "http://[2001:db8::1]:8080", wantErr: "insecure_api_url"},
		// A name that merely LOOKS loopback-ish is not loopback; no DNS is consulted.
		{name: "http localhost lookalike", raw: "http://localhost.evil.example", wantErr: "insecure_api_url"},
		{name: "http not-quite-loopback ip", raw: "http://128.0.0.1", wantErr: "insecure_api_url"},

		// Explicit opt-in unlocks non-loopback plaintext, and only when clearly truthy.
		{name: "opt-in 1", raw: "http://evil.example", optIn: "1", want: "http://evil.example"},
		{name: "opt-in true", raw: "http://evil.example", optIn: "true", want: "http://evil.example"},
		{name: "opt-in enabled", raw: "http://evil.example", optIn: "enabled", want: "http://evil.example"},
		{name: "opt-in 0", raw: "http://evil.example", optIn: "0", wantErr: "insecure_api_url"},
		{name: "opt-in false", raw: "http://evil.example", optIn: "false", wantErr: "insecure_api_url"},
		// Fail-closed: an unrecognized value leaves the floor in place. This is the
		// mirror of securityToggleEnabled, which fails a garbage value ON because it
		// switches a protection ON; this switches one OFF.
		{name: "opt-in garbage", raw: "http://evil.example", optIn: "ture", wantErr: "insecure_api_url"},
		{name: "opt-in yes-ish garbage", raw: "http://evil.example", optIn: "sure", wantErr: "insecure_api_url"},

		// Malformed / non-HTTP targets refuse regardless of the opt-in.
		{name: "empty", raw: "", wantErr: "invalid_api_url"},
		{name: "blank", raw: "   ", wantErr: "invalid_api_url"},
		{name: "schemeless", raw: "api.entire.io", wantErr: "invalid_api_url"},
		{name: "protocol relative", raw: "//api.entire.io", wantErr: "invalid_api_url"},
		{name: "relative path", raw: "/api/v1", wantErr: "invalid_api_url"},
		{name: "no host", raw: "https://", wantErr: "invalid_api_url"},
		{name: "ftp", raw: "ftp://api.entire.io", wantErr: "invalid_api_url"},
		{name: "file", raw: "file:///etc/passwd", wantErr: "invalid_api_url"},
		{name: "gopher", raw: "gopher://api.entire.io", wantErr: "invalid_api_url"},
		{name: "garbage", raw: "not a url at all", wantErr: "invalid_api_url"},
		{name: "control chars", raw: "https://api.entire.io/\x7f", wantErr: "invalid_api_url"},
		{name: "ftp with opt-in", raw: "ftp://api.entire.io", optIn: "1", wantErr: "invalid_api_url"},
		{name: "schemeless with opt-in", raw: "api.entire.io", optIn: "1", wantErr: "invalid_api_url"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvAllowInsecure, tc.optIn)
			got, err := Validate(tc.raw)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("Validate(%q) = %q, nil; want error %q", tc.raw, got, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Validate(%q) error = %v; want it to contain %q", tc.raw, err, tc.wantErr)
				}
				if got != "" {
					t.Fatalf("Validate(%q) returned %q alongside an error; want the empty string", tc.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate(%q) = %v; want %q", tc.raw, err, tc.want)
			}
			if got != tc.want {
				t.Fatalf("Validate(%q) = %q; want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// TestValidateIsIdempotent: a validated URL re-validates to itself, so passing the
// normalized value back through a second layer (the CLI validates, then factsync
// validates again at the request chokepoint) can never change or re-reject it.
func TestValidateIsIdempotent(t *testing.T) {
	t.Setenv(EnvAllowInsecure, "")
	for _, raw := range []string{"https://api.entire.io/", "http://127.0.0.1:8787", "http://[::1]:9/"} {
		once, err := Validate(raw)
		if err != nil {
			t.Fatalf("Validate(%q): %v", raw, err)
		}
		twice, err := Validate(once)
		if err != nil {
			t.Fatalf("Validate(Validate(%q)): %v", raw, err)
		}
		if twice != once {
			t.Fatalf("Validate is not idempotent for %q: %q then %q", raw, once, twice)
		}
	}
}

func TestIsLoopbackHost(t *testing.T) {
	for host, want := range map[string]bool{
		"127.0.0.1":              true,
		"127.255.255.254":        true,
		"::1":                    true,
		"0:0:0:0:0:0:0:1":        true,
		"localhost":              true,
		"LOCALHOST":              true,
		"localhost.":             true,
		"api.localhost":          false,
		"":                       false,
		"0.0.0.0":                false,
		"128.0.0.1":              false,
		"192.168.1.1":            false,
		"10.0.0.1":               false,
		"evil.example":           false,
		"localhost.evil.example": false,
		"notlocalhost":           false,
		"2001:db8::1":            false,
	} {
		if got := IsLoopbackHost(host); got != want {
			t.Errorf("IsLoopbackHost(%q) = %v; want %v", host, got, want)
		}
	}
}
