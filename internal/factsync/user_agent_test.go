package factsync

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestUserAgentNamesTheBuild pins the header the server reads to tell which
// client builds are still in the field. Both wire calls must carry it: the
// feed is what a stuck client retries, and the upsert is what a healthy one
// does most.
func TestUserAgentNamesTheBuild(t *testing.T) {
	const repoID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	for _, tc := range []struct {
		name    string
		version string
		want    string
	}{
		{name: "release build", version: "0.4.0", want: "entire-brain/0.4.0"},
		{name: "dev build", version: "dev", want: "entire-brain/dev"},
		// An unset version must not send a bare prefix naming no build at all.
		{name: "unset version", version: "", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seen := make(map[string]string)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen[r.Method] = r.Header.Get("User-Agent")
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					_, _ = w.Write([]byte(`{"items":[],"nextPageToken":""}`))
					return
				}
				_, _ = w.Write([]byte(`{"added":0,"changed":0,"unchanged":0}`))
			}))
			defer srv.Close()

			h := &HTTPServer{BaseURL: srv.URL, Token: "tok", Version: tc.version}
			if _, err := h.FactsFeedPage(context.Background(), repoID, "", 10); err != nil {
				t.Fatalf("feed: %v", err)
			}
			if _, err := h.UpsertFacts(context.Background(), repoID, nil); err != nil {
				t.Fatalf("upsert: %v", err)
			}
			for _, method := range []string{http.MethodGet, http.MethodPost} {
				got := seen[method]
				if tc.want == "" {
					// Go's default agent is fine; a prefix with no version is not.
					if got == "entire-brain/" {
						t.Fatalf("%s sent a versionless agent %q", method, got)
					}
					continue
				}
				if got != tc.want {
					t.Fatalf("%s User-Agent = %q, want %q", method, got, tc.want)
				}
			}
		})
	}
}
