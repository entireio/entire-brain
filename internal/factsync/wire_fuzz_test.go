package factsync

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/factmerge"
)

// Once the fact-set is shared (P1), every byte HTTPServer decodes was produced
// by a remote server: the head blob (base64 in a JSON envelope), the proposal
// set, and the resolve response. These harnesses point the real client at a
// hostile server and assert the only outcomes are "a value" or "an error" —
// never a panic, never a hang.
//
//	go test ./internal/factsync -run xxx -fuzz FuzzName

const fuzzOrigin = "https://fuzz.invalid"

// fuzzTransport answers every request in-process from a fixed status and body.
//
// A real httptest server per execution — or even one shared server — is
// socket-bound: the client abandons a connection whenever a fuzzed body does not
// decode cleanly to EOF, so every request costs a fresh TCP connection. That
// exhausts the machine's ephemeral ports within seconds (tens of thousands of
// TIME_WAIT sockets), collapses the exec rate to double digits, and breaks every
// other test on the box. A RoundTripper drives the identical client code — status
// mapping, JSON decode, base64 `data` — with no sockets at all.
type fuzzTransport struct {
	status int
	body   []byte
}

func (t fuzzTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		_, _ = io.Copy(io.Discard, req.Body)
		_ = req.Body.Close()
	}
	return &http.Response{
		StatusCode:    t.status,
		Status:        fmt.Sprintf("%d %s", t.status, http.StatusText(t.status)),
		Proto:         "HTTP/1.1",
		Header:        http.Header{"Content-Type": []string{"application/json"}},
		Body:          io.NopCloser(bytes.NewReader(t.body)),
		ContentLength: int64(len(t.body)),
		Request:       req,
	}, nil
}

func fuzzClient(status int, body []byte) *http.Client {
	return &http.Client{Transport: fuzzTransport{status: status, body: body}}
}

// FuzzHTTPServerResponses drives every HTTPServer read path against an arbitrary
// status line and body. The base64 `data` field is the size-capped path: huma
// encodes the fact blob as base64 in JSON, so a hostile server controls both the
// encoding and the decoded length.
func FuzzHTTPServerResponses(f *testing.F) {
	f.Add(200, []byte(`{"found":true,"ref":"r","version":1,"data":"aGk="}`))
	f.Add(200, []byte(`{"found":true,"data":"!!!!not base64!!!!"}`))
	f.Add(200, []byte(`{"newRef":"r","changed":true}`))
	f.Add(200, []byte(`{"found":true,"proposals":[{"id":"","proposal":{}}]}`))
	f.Add(200, []byte(`{"found":true,"proposal":{"id":"x","proposal":{"action":"merge"}}}`))
	f.Add(412, []byte(``))
	f.Add(503, []byte(`{`))
	f.Add(200, []byte(`{"data":`))

	f.Fuzz(func(t *testing.T, status int, body []byte) {
		if status < 100 || status > 599 {
			return // net/http refuses to write a non-status code
		}
		h := &HTTPServer{BaseURL: fuzzOrigin, Token: "t", Client: fuzzClient(status, body)}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		_, _, _, _ = h.Current(ctx, "repo", "main")
		_, _ = h.Advance(ctx, "repo", "main", "old", []byte("x"))
		_, _ = h.ListProposals(ctx, "repo", "main")
		_, _ = h.GetProposal(ctx, "repo", "main", "prop-0123456789abcdef")
		_, _ = h.PublishProposals(ctx, "repo", "main", "old", nil)
		_, _ = h.ResolveProposal(ctx, ResolveProposalRequest{
			RepoID:     "repo",
			Branch:     "main",
			ProposalID: "prop-0123456789abcdef",
			Decision:   Accept,
			Facts: []factmerge.Record{{
				ID: "fact:aa", Paths: []string{"a.b.c"}, Text: "t", Branch: "main",
				Origin: "distilled", Status: factmerge.StatusActive,
				Provenance: []factmerge.Anchor{{SessionID: "s", Transcript: "/home/me/x", Line: 3}},
			}},
		})
	})
}

// FuzzSyncAgainstHostileHead runs the full read-merge-CAS loop against a server
// that answers every request with fuzzed bytes. Sync parses the head blob with
// factmerge.ParseNDJSON, so this reaches the fact-set parser through the real
// remote path rather than through a direct reader.
func FuzzSyncAgainstHostileHead(f *testing.F) {
	f.Add([]byte(`{"found":true,"ref":"r","data":"eyJpZCI6ImZhY3Q6YWEifQo="}`))
	f.Add([]byte(`{"found":true,"ref":"r","data":""}`))
	f.Add([]byte(`{"found":true,"ref":"r","data":"AAAA"}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		h := &HTTPServer{BaseURL: fuzzOrigin, Client: fuzzClient(200, body)}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, _ = Sync(ctx, h, "repo", "main", "member", nil, time.Unix(0, 0).UTC())
	})
}

// FuzzFindProposal drives reference resolution. Resolving the WRONG conflict is
// unrecoverable, so the property is strict: a returned proposal must actually
// match the reference by one of the three documented rules.
func FuzzFindProposal(f *testing.F) {
	f.Add("prop-0123456789abcdef", "fact:aa", "fact:bb")
	f.Add("prop", "a", "b")
	f.Add("", "", "")
	f.Add("prop-0123", "x", "y")
	f.Fuzz(func(t *testing.T, ref, candA, candB string) {
		set := OpenProposals([]factmerge.Proposal{
			{Action: "merge", CandidateID: candA, TargetID: "t1", Branch: "main", ProposedBy: "m1"},
			{Action: "supersede", CandidateID: candB, TargetID: "t2", Branch: "main", ProposedBy: "m2"},
		})
		got, err := FindProposal(set, ref)
		if err != nil {
			return
		}
		trimmed := strings.TrimSpace(ref) // the same normalization FindProposal applies
		switch {
		case got.ID == trimmed:
		case got.Proposal.CandidateID == trimmed:
		case len(trimmed) >= len(proposalIDPrefix)+minProposalRefLen && strings.HasPrefix(got.ID, trimmed):
		default:
			t.Fatalf("FindProposal(%q) returned a proposal matching no documented rule: %#v", ref, got)
		}
	})
}

// FuzzParseDecision pins the wire spelling of a decision: anything accepted must
// render back to a spelling that parses to the same value.
func FuzzParseDecision(f *testing.F) {
	f.Add("accept")
	f.Add("  REJECT ")
	f.Add("apply")
	f.Add("")
	f.Fuzz(func(t *testing.T, s string) {
		d, err := ParseDecision(s)
		if err != nil {
			return
		}
		again, err := ParseDecision(d.String())
		if err != nil || again != d {
			t.Fatalf("decision round trip broke: %q -> %v -> %q -> %v (%v)", s, d, d.String(), again, err)
		}
	})
}

func TestFuzzOriginReachesFactDecoder(t *testing.T) {
	h := &HTTPServer{BaseURL: fuzzOrigin, Client: fuzzClient(200, []byte(`{"found":true,"ref":"decoded-marker","data":"aGk="}`))}
	ref, data, found, err := h.Current(context.Background(), "repo", "main")
	if err != nil || !found || ref != "decoded-marker" || string(data) != "hi" {
		t.Fatalf("fuzz harness did not decode seed: %q %q %v %v", data, ref, found, err)
	}
}
