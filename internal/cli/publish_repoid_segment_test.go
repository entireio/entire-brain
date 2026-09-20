package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"testing"

	"github.com/entireio/entire-brain/internal/repoid"
)

// `brain publish` concatenates the repo id into its upload target and attaches the
// member's bearer token AND the whole brain artifact bundle to the request. The id
// was only url.PathEscape'd, which cannot express the rule that matters: "." and ".."
// are RFC 3986 unreserved, so PathEscape leaves them untouched and
//
//	/api/v1/repos/../brain/artifacts
//
// goes out on the wire, where any dot-segment-normalizing hop collapses it to
// /api/v1/brain/artifacts — a different endpoint, handed the bundle and the token.
//
// These tests assert the RESULTING REQUEST — its path, or that none was made. They
// deliberately do not compare an id against url.PathEscape of itself, which is the
// tautology that let the hole survive its first fix.

// TestPostBrainArtifactsRejectsRepoIDThatIsNotOneSafeSegment: nothing is sent.
func TestPostBrainArtifactsRejectsRepoIDThatIsNotOneSafeSegment(t *testing.T) {
	hostile := map[string]string{
		"empty":           "",
		"blank":           "   ",
		"dot":             ".",
		"dotdot":          "..",
		"dotdot padded":   " .. ",
		"triple dot":      "...",
		"slash":           "a/b",
		"leading slash":   "/admin",
		"backslash":       `a\b`,
		"encoded slash":   "..%2f..",
		"query":           "x?admin=1",
		"fragment":        "x#frag",
		"space":           "repo 1",
		"traversal chain": "../../admin",
	}
	for name, repoID := range hostile {
		t.Run(name, func(t *testing.T) {
			var sent int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				sent++
				fmt.Fprint(w, `{}`)
			}))
			defer srv.Close()

			_, err := postBrainArtifacts(context.Background(), srv.URL, repoID, "secret", publishRequestBody{})
			if err == nil {
				t.Fatalf("repo id %q was accepted; want a refusal before any request", repoID)
			}
			if !errors.Is(err, repoid.ErrInvalid) {
				t.Errorf("repo id %q: error = %v; want it to wrap repoid.ErrInvalid", repoID, err)
			}
			if sent != 0 {
				t.Errorf("repo id %q: %d request(s) reached the server; want the token and the bundle to never leave the process", repoID, sent)
			}
		})
	}
}

// TestPostBrainArtifactsLandsOnTheIntendedPath pins the whole upload target for ids
// that are allowed through: prefix and suffix survive, the id is exactly one segment,
// nothing leaks into the query, and the path is already in normal form.
func TestPostBrainArtifactsLandsOnTheIntendedPath(t *testing.T) {
	for _, repoID := range []string{
		"01JABCDEFGHJKMNPQRSTVWXYZ0",
		"repo-123",
		"a.b.c",
		"repo_1~x",
	} {
		t.Run(repoID, func(t *testing.T) {
			var gotPath, gotQuery string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath, gotQuery = r.URL.EscapedPath(), r.URL.RawQuery
				fmt.Fprint(w, `{}`)
			}))
			defer srv.Close()

			if _, err := postBrainArtifacts(context.Background(), srv.URL, repoID, "secret", publishRequestBody{}); err != nil {
				t.Fatalf("postBrainArtifacts: %v", err)
			}
			want := publishAPIPathPrefix + repoID + publishAPIPathSuffix
			if gotPath != want {
				t.Fatalf("request path = %q, want %q", gotPath, want)
			}
			if gotQuery != "" {
				t.Errorf("query string = %q, want empty", gotQuery)
			}
			if cleaned := path.Clean(gotPath); cleaned != gotPath {
				t.Errorf("path %q is not in normal form: a normalizing hop rewrites it to %q", gotPath, cleaned)
			}
		})
	}
}

// TestPublishCannotReachAnotherRoute is the end-to-end consequence, served by a real
// http.ServeMux, which does the dot-segment cleanup a bare handler does not.
func TestPublishCannotReachAnotherRoute(t *testing.T) {
	reached := make(chan string, 8)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/", func(w http.ResponseWriter, r *http.Request) {
		reached <- "repos:" + r.URL.Path
		fmt.Fprint(w, `{}`)
	})
	// The sibling route a traversal lands on once "/repos/../" is normalized away.
	mux.HandleFunc("/api/v1/brain/", func(w http.ResponseWriter, r *http.Request) {
		reached <- "elsewhere:" + r.URL.Path
		fmt.Fprint(w, `{}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	if _, err := postBrainArtifacts(context.Background(), srv.URL, "..", "secret", publishRequestBody{}); err == nil {
		t.Fatal(`repo id ".." was accepted`)
	}
	close(reached)
	for hit := range reached {
		if strings.HasPrefix(hit, "elsewhere:") {
			t.Fatalf(`repo id ".." traversed out of the repos prefix and reached %s`, hit)
		}
		t.Fatalf(`repo id ".." produced a request at all: %s`, hit)
	}
}

// TestRunPublishRefusesBadRepoIDBeforeTouchingDisk covers the command-level check:
// the id is validated next to the URL floor, before any git or disk work, so a
// malformed target fails with the whole brain still local.
func TestRunPublishRefusesBadRepoIDBeforeTouchingDisk(t *testing.T) {
	t.Setenv(envBrainAllowHosted, "1")

	err := runPublish(context.Background(), nil, Options{}, publishCommandOptions{
		repoID: "../../admin",
		apiURL: "https://api.example.com",
		token:  "secret",
	})
	if err == nil {
		t.Fatal("runPublish accepted a repo id that is not one safe path segment")
	}
	if !errors.Is(err, repoid.ErrInvalid) {
		t.Errorf("runPublish error = %v; want it to wrap repoid.ErrInvalid", err)
	}
}
