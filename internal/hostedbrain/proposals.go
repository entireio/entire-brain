package hostedbrain

import (
	"context"
	"time"

	"github.com/ashtom/entire-brain/internal/factmerge"
	"github.com/ashtom/entire-brain/internal/factsync"
)

// Cross-member review from the hosted brain client. The MCP surface in client.go is
// READ-ONLY (search/get/status); settling a cross-member conflict writes, so it runs
// over the same REST fact-set transport the sync runner uses (internal/factsync's
// HTTPServer), not over MCP.
//
// Every call goes through the same egress chokepoint as the MCP calls: the no-egress
// gate (ENTIRE_BRAIN_NO_EGRESS / ENTIRE_BRAIN_LOCAL_ONLY) refuses the request BEFORE
// any connection is made, so a local-only workspace can neither read nor settle a
// hosted proposal. The merge itself still runs here on the runner (ADR-P1-E) — the
// server only compare-and-swaps the two heads.

// factsyncServer builds the REST transport for this client's repo target, reusing the
// client's base URL, bearer token, and HTTP client so both surfaces share one config.
func (c *Client) factsyncServer() *factsync.HTTPServer {
	return &factsync.HTTPServer{BaseURL: c.BaseURL, Token: c.Token, Client: c.HTTP}
}

// ListProposals returns the branch's OPEN cross-member proposals — the conflicts a
// member's sync kept both sides of and routed for review. An empty list is a normal
// outcome (no open conflict), not an error. branch "" means the server's default.
func (c *Client) ListProposals(ctx context.Context, repoID, branch string) ([]factsync.OpenProposal, error) {
	if noEgress() {
		return nil, ErrNoEgress
	}
	set, err := c.factsyncServer().ListProposals(ctx, repoID, branch)
	if err != nil {
		return nil, err
	}
	return set.Proposals, nil
}

// GetProposal fetches one open proposal by its id. A proposal another member already
// settled is factsync.ErrProposalNotFound.
func (c *Client) GetProposal(ctx context.Context, repoID, branch, proposalID string) (factsync.OpenProposal, error) {
	if noEgress() {
		return factsync.OpenProposal{}, ErrNoEgress
	}
	return c.factsyncServer().GetProposal(ctx, repoID, branch, proposalID)
}

// ApplyProposal ACCEPTS a proposal: the candidate consolidates into (merge) or
// supersedes (supersede) the target, the losing fact is retained rather than deleted,
// and the resolved fact set plus the shrunken open set are pushed under CAS. ref is a
// proposal id, an unambiguous id prefix, or the candidate fact id.
func (c *Client) ApplyProposal(ctx context.Context, repoID, branch, ref string, now time.Time) (factsync.ResolveOpenResult, error) {
	return c.resolve(ctx, repoID, branch, ref, factsync.Accept, now)
}

// RejectProposal REJECTS a proposal: both facts stay active and the conflict
// cross-link is cleared — the two statements are judged to coexist. Nothing is
// dropped.
func (c *Client) RejectProposal(ctx context.Context, repoID, branch, ref string, now time.Time) (factsync.ResolveOpenResult, error) {
	return c.resolve(ctx, repoID, branch, ref, factsync.Reject, now)
}

// PublishProposals makes a sync's raised conflicts visible to every other member by
// unioning them into the branch's hosted open set (idempotent: re-publishing the same
// conflict is a no-op).
func (c *Client) PublishProposals(ctx context.Context, repoID, branch string, raised []factsync.OpenProposal) (factsync.PublishResult, error) {
	if noEgress() {
		return factsync.PublishResult{}, ErrNoEgress
	}
	merge := make([]factmerge.Proposal, 0, len(raised))
	for _, p := range raised {
		merge = append(merge, p.Proposal)
	}
	return factsync.PublishRaised(ctx, c.factsyncServer(), repoID, branch, merge)
}

func (c *Client) resolve(ctx context.Context, repoID, branch, ref string, decision factsync.Decision, now time.Time) (factsync.ResolveOpenResult, error) {
	if noEgress() {
		return factsync.ResolveOpenResult{}, ErrNoEgress
	}
	return factsync.ResolveOpen(ctx, c.factsyncServer(), repoID, branch, ref, decision, now)
}
