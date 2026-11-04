package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestJSONChecks(t *testing.T) {
	for _, tc := range []struct {
		command, input, needle, want string
		fail                         bool
	}{
		{"json-contains", `{"a":[{"text":"kept fact"}]}`, "kept", "", false},
		{"json-contains", `{"kept":"other"}`, "kept", "", true},
		{"distill-written", `{"facts_written":0,"facts":[1]}`, "", "", true},
		{"distill-written", `{"memories_written":2}`, "", "", false},
		{"distill-written", `{"facts":[{}]}`, "", "", false},
		{"first-fact", `{"facts":[{"text":"","content":"fallback"}]}`, "", "fallback\n", false},
		{"first-fact", `{"facts":[]}`, "", "", true},
		{"first-fact", `not json`, "", "", true},
	} {
		t.Run(tc.command+tc.input, func(t *testing.T) {
			args := []string{tc.command}
			if tc.needle != "" {
				args = append(args, tc.needle)
			}
			var out bytes.Buffer
			err := run(args, strings.NewReader(tc.input), &out)
			if (err != nil) != tc.fail || out.String() != tc.want {
				t.Fatalf("output=%q error=%v", out.String(), err)
			}
		})
	}
}

func TestRemoteFactPaginationAndBranch(t *testing.T) {
	t.Setenv("ENTIRE_TOKEN", "test-token")
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer test-token" || r.Header.Get("User-Agent") != "entire-release-smoke/1" {
			t.Error("missing request headers")
		}
		if r.URL.Query().Get("pageToken") == "" {
			fmt.Fprint(w, `{"items":[{"record":{"branch":"other","text":"needle"}}],"nextPageToken":"page 2"}`)
		} else {
			fmt.Fprint(w, `{"items":[{"record":{"branch":"wanted","text":"needle"}}]}`)
		}
	}))
	defer server.Close()
	if err := remoteFact(server.URL, "repo", "wanted", "needle"); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("calls=%d", calls)
	}
}

func TestRemoteFactRejectsHTTPFailure(t *testing.T) {
	t.Setenv("ENTIRE_TOKEN", "test-token")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer server.Close()
	if err := remoteFact(server.URL, "repo", "branch", "needle"); err == nil {
		t.Fatal("accepted forbidden response")
	}
}
