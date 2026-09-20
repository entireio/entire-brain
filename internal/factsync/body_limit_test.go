package factsync

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/entireio/entire-brain/internal/httpx"
)

// Every fact-set and proposal 200 used to be read with
//
//	json.NewDecoder(resp.Body).Decode(&out)
//
// which streams the PARSE but allocates the VALUES in full. The head's `data` field
// is a base64 []byte, so a peer answering a well-formed
// `{"found":true,"ref":"r","data":"<8 GiB of base64>"}` allocates gigabytes on the
// member's machine. Measured against a mock that does exactly that, `facts sync`
// peaked at 9.8 GB RSS and the proposal list at 10 GB before failing — an OOM on a
// laptop, and the workspace daemon is long-lived. The request timeout does not help:
// it bounds time, and a LAN peer delivers tens of gigabytes inside five minutes.
//
// The ceiling must also REFUSE rather than truncate. A fact-set head cut short at a
// record boundary parses cleanly as a shorter fact set, and the next CAS would push
// that shortened set back as the head — silently deleting other members' facts.

// oversizedBody streams a well-formed JSON object whose one string field is padded
// past the limit, so the refusal is about SIZE and not about malformed JSON.
func oversizedBody(t *testing.T, prefix string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if _, err := io.WriteString(w, prefix); err != nil {
			return
		}
		chunk := strings.Repeat("QUJD", 1024) // 4 KiB of valid base64
		for written := 0; int64(written) <= httpx.MaxJSONBodyBytes; written += len(chunk) {
			if _, err := io.WriteString(w, chunk); err != nil {
				return
			}
		}
		_, _ = io.WriteString(w, `"}`)
	}
}

func withSmallBodyLimit(t *testing.T, limit int64) {
	t.Helper()
	prev := httpx.MaxJSONBodyBytes
	httpx.MaxJSONBodyBytes = limit
	t.Cleanup(func() { httpx.MaxJSONBodyBytes = prev })
}

func TestCurrentRefusesAnOversizedHead(t *testing.T) {
	withSmallBodyLimit(t, 64<<10)
	srv := httptest.NewServer(oversizedBody(t, `{"found":true,"ref":"r1","version":1,"data":"`))
	defer srv.Close()

	h := &HTTPServer{BaseURL: srv.URL, Token: "tok"}
	_, plaintext, found, err := h.Current(t.Context(), "01HZZPROBE0000000000000000", "main")
	if err == nil {
		t.Fatalf("Current accepted an oversized head (found=%v, %d bytes); it must refuse", found, len(plaintext))
	}
	if !errors.Is(err, httpx.ErrBodyTooLarge) {
		t.Errorf("Current failed with %v; want it to wrap httpx.ErrBodyTooLarge so the cause is distinguishable", err)
	}
	if found || plaintext != nil {
		t.Errorf("Current returned data alongside its refusal (found=%v, %d bytes); a partial head must never reach the merge", found, len(plaintext))
	}
}

func TestAdvanceRefusesAnOversizedResponse(t *testing.T) {
	withSmallBodyLimit(t, 64<<10)
	srv := httptest.NewServer(oversizedBody(t, `{"newRef":"`))
	defer srv.Close()

	h := &HTTPServer{BaseURL: srv.URL, Token: "tok"}
	ref, err := h.Advance(t.Context(), "01HZZPROBE0000000000000000", "main", "old", []byte("{}\n"))
	if err == nil {
		t.Fatalf("Advance accepted an oversized response (ref %q)", ref)
	}
	if !errors.Is(err, httpx.ErrBodyTooLarge) {
		t.Errorf("Advance failed with %v; want httpx.ErrBodyTooLarge", err)
	}
}

func TestListProposalsRefusesAnOversizedQueue(t *testing.T) {
	withSmallBodyLimit(t, 64<<10)
	srv := httptest.NewServer(oversizedBody(t, `{"ref":"`))
	defer srv.Close()

	h := &HTTPServer{BaseURL: srv.URL, Token: "tok"}
	set, err := h.ListProposals(t.Context(), "01HZZPROBE0000000000000000", "main")
	if err == nil {
		t.Fatalf("ListProposals accepted an oversized queue (%d proposals); it must refuse", len(set.Proposals))
	}
	if !errors.Is(err, httpx.ErrBodyTooLarge) {
		t.Errorf("ListProposals failed with %v; want httpx.ErrBodyTooLarge", err)
	}
}

// A body AT the ceiling is still served: the bound exists to stop the absurd, not to
// shrink the real payload, and a refusal that also rejected legitimate responses
// would be traded one failure for another.
func TestCurrentAcceptsABodyWithinTheLimit(t *testing.T) {
	withSmallBodyLimit(t, 1<<20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// 300 KiB of base64 -> ~225 KiB of plaintext, comfortably inside 1 MiB.
		_, _ = io.WriteString(w, `{"found":true,"ref":"r1","version":1,"data":"`+strings.Repeat("QUJD", 76800)+`"}`)
	}))
	defer srv.Close()

	h := &HTTPServer{BaseURL: srv.URL, Token: "tok"}
	ref, plaintext, found, err := h.Current(t.Context(), "01HZZPROBE0000000000000000", "main")
	if err != nil {
		t.Fatalf("Current refused a body inside the limit: %v", err)
	}
	if !found || ref != "r1" || len(plaintext) == 0 {
		t.Fatalf("Current lost a valid head: found=%v ref=%q bytes=%d", found, ref, len(plaintext))
	}
}
