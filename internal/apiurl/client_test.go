package apiurl

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWithoutRedirectsKeepsPostAtValidatedTarget(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			leaked := 0
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked++ }))
			defer target.Close()
			origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", target.URL)
				w.WriteHeader(status)
				io.WriteString(w, "redirect refused")
			}))
			defer origin.Close()
			original := origin.Client()
			called := false
			original.CheckRedirect = func(*http.Request, []*http.Request) error { called = true; return nil }
			req, _ := http.NewRequest(http.MethodPost, origin.URL, strings.NewReader("private facts"))
			req.Header.Set("Authorization", "Bearer secret")
			resp, err := WithoutRedirects(original).Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != status || string(body) != "redirect refused" || leaked != 0 || called {
				t.Fatalf("status=%d body=%q leaked=%d custom callback=%v", resp.StatusCode, body, leaked, called)
			}
			original.CheckRedirect(nil, nil)
			if !called {
				t.Fatal("original client mutated")
			}
		})
	}
}
