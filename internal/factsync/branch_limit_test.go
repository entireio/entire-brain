package factsync

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The hosted fact-set endpoints cap a branch name at maxBranchBytes and 400 anything
// longer. Learning that from the server costs a round trip that carries the member's
// bearer token and, on the write paths, the member's whole fact set — for a request
// that cannot succeed. The client knows the branch before it opens a socket, so it
// refuses first, with the same numbers in the message.
//
// This is the one cap the client pre-validates. See the note on maxBranchBytes for
// why the other new server caps are surfaced rather than mirrored.

func TestOverlongBranchIsRefusedBeforeAnyRequest(t *testing.T) {
	branch := strings.Repeat("b", maxBranchBytes+1)
	for name, call := range branchCalls {
		t.Run(name, func(t *testing.T) {
			srv, sent := repoIDCountingServer(t, `{"found":false,"proposals":[],"changed":true}`)
			h := &HTTPServer{BaseURL: srv.URL, Token: "secret"}
			err := call(context.Background(), h, branch)
			if err == nil {
				t.Fatalf("%s accepted a %d-byte branch", name, len(branch))
			}
			if !errors.Is(err, ErrBranchTooLong) {
				t.Fatalf("%s: error = %v; want it to wrap ErrBranchTooLong", name, err)
			}
			// The message has to be actionable on its own: both the size that was
			// sent and the limit it broke, the same two numbers the server states.
			for _, want := range []string{"513", "512"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("%s: error = %q; want it to name %q", name, err, want)
				}
			}
			if *sent != 0 {
				t.Fatalf("%s: %d request(s) reached the server; the refusal must cost zero egress", name, *sent)
			}
		})
	}
}

// The check is an upper bound, not a shape rule: a branch exactly at the cap is a
// branch the server accepts, so the client must not refuse it.
func TestBranchAtTheLimitIsStillSent(t *testing.T) {
	branch := strings.Repeat("b", maxBranchBytes)
	for name, call := range branchCalls {
		t.Run(name, func(t *testing.T) {
			srv, sent := repoIDCountingServer(t, `{"found":false,"proposals":[],"changed":true,"ref":"r","newRef":"r","factsRef":"r","proposalsRef":"r"}`)
			h := &HTTPServer{BaseURL: srv.URL, Token: "secret"}
			if err := call(context.Background(), h, branch); errors.Is(err, ErrBranchTooLong) {
				t.Fatalf("%s refused a branch exactly at the %d-byte limit", name, maxBranchBytes)
			}
			if *sent == 0 {
				t.Fatalf("%s sent no request for a branch at the limit", name)
			}
		})
	}
}

// The cap is on BYTES, matching the server, not on runes: a 300-character branch of
// 3-byte runes is 900 bytes on the wire and the server refuses it.
func TestBranchLimitCountsBytesNotRunes(t *testing.T) {
	branch := strings.Repeat("é", maxBranchBytes) // 2 bytes each
	srv, sent := repoIDCountingServer(t, `{"found":false}`)
	h := &HTTPServer{BaseURL: srv.URL, Token: "secret"}
	if _, _, _, err := h.Current(context.Background(), "repo", branch); !errors.Is(err, ErrBranchTooLong) {
		t.Fatalf("Current(%d-byte branch) = %v; want ErrBranchTooLong", len(branch), err)
	}
	if *sent != 0 {
		t.Fatalf("%d request(s) reached the server", *sent)
	}
}

// branchCalls is every HTTPServer method that puts a branch on the wire.
var branchCalls = map[string]func(context.Context, *HTTPServer, string) error{
	"Current": func(ctx context.Context, h *HTTPServer, branch string) error {
		_, _, _, err := h.Current(ctx, "repo", branch)
		return err
	},
	"Advance": func(ctx context.Context, h *HTTPServer, branch string) error {
		_, err := h.Advance(ctx, "repo", branch, "old", []byte("fact:a\n"))
		return err
	},
	"ListProposals": func(ctx context.Context, h *HTTPServer, branch string) error {
		_, err := h.ListProposals(ctx, "repo", branch)
		return err
	},
	"GetProposal": func(ctx context.Context, h *HTTPServer, branch string) error {
		_, err := h.GetProposal(ctx, "repo", branch, "prop-1234567890abcdef")
		return err
	},
	"PublishProposals": func(ctx context.Context, h *HTTPServer, branch string) error {
		_, err := h.PublishProposals(ctx, "repo", branch, "old", nil)
		return err
	},
	"ResolveProposal": func(ctx context.Context, h *HTTPServer, branch string) error {
		_, err := h.ResolveProposal(ctx, ResolveProposalRequest{
			RepoID: "repo", Branch: branch, ProposalID: "prop-1234567890abcdef",
			Decision: Reject, FactsUnchanged: true,
		})
		return err
	},
}
