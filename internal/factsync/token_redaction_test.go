package factsync

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// echoTokenServer answers every request with the Authorization header it received,
// which is what a hostile or badly-written hosted endpoint does.
func echoTokenServer(t *testing.T, status int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"detail":"rejected credential ` + r.Header.Get("Authorization") + `"}`))
	}))
}

const echoedToken = "entire_pat_0123456789abcdef"

func TestFactSetErrorsDoNotEchoTheBearerToken(t *testing.T) {
	cases := map[string]func(*HTTPServer) error{
		"Current": func(h *HTTPServer) error {
			_, _, _, err := h.Current(t.Context(), "01HZZPROBE0000000000000000", "main")
			return err
		},
		"Advance": func(h *HTTPServer) error {
			_, err := h.Advance(t.Context(), "01HZZPROBE0000000000000000", "main", "old", []byte("{}\n"))
			return err
		},
		"ListProposals": func(h *HTTPServer) error {
			_, err := h.ListProposals(t.Context(), "01HZZPROBE0000000000000000", "main")
			return err
		},
		"GetProposal": func(h *HTTPServer) error {
			_, err := h.GetProposal(t.Context(), "01HZZPROBE0000000000000000", "main", "p1")
			return err
		},
	}
	for _, status := range []int{http.StatusUnauthorized, http.StatusConflict, http.StatusNotFound, http.StatusInternalServerError} {
		srv := echoTokenServer(t, status)
		for name, call := range cases {
			h := &HTTPServer{BaseURL: srv.URL, Token: echoedToken}
			err := call(h)
			if err == nil {
				continue // this status is a normal outcome for this call
			}
			if strings.Contains(err.Error(), echoedToken) {
				t.Errorf("%s on HTTP %d echoed the bearer token into its error: %v", name, status, err)
			}
		}
		srv.Close()
	}
}
