package factsync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ashtom/entire-brain/internal/factmerge"
)

// The OPEN-PROPOSAL SET is the second hosted surface of the cross-member model.
// Sync RAISES routed proposals (keep-both, nothing overwritten) and Resolve SETTLES
// one against a fact set; between those two moments the proposal has to live
// somewhere every member can see it. That somewhere is the hosted open-proposal set:
// a per-repo/branch list the server stores as an opaque blob and compare-and-swaps,
// exactly like the fact-set head.
//
// The merge still runs on the runner (ADR-P1-E). The server never evaluates a
// proposal: a member lists the open set, computes the resolved fact set locally with
// factmerge.ApplyProposal / factmerge.RejectProposal (through Resolve), and pushes
// BOTH the new facts blob and the shrunken proposal set under CAS. The server is a
// dumb, atomic store on both halves, so 412/409 mean the same thing here as on the
// fact-set head: someone else moved first, re-read and retry.

// ErrProposalNotFound is returned when the requested proposal is not in the branch's
// open set — either it never existed or another member already resolved it. It is
// distinct from ErrProposalNotApplicable (the proposal IS open but its facts have
// moved) so a caller can tell "already settled" from "settle it against a newer head".
var ErrProposalNotFound = errors.New("factsync: no such open proposal")

// ErrAmbiguousProposal is returned when a caller-supplied proposal reference (an id
// prefix) matches more than one open proposal — resolving the wrong conflict is
// unrecoverable, so the reference must be disambiguated by the caller.
var ErrAmbiguousProposal = errors.New("factsync: proposal reference is ambiguous")

// ErrProposalQueueUnsupported is returned when the backend does not serve the
// open-proposal endpoints at all (an entire-api that predates the queue: 404 or
// 501 on the collection). It is the ONLY publish failure a sync may downgrade
// to a warning — every other class (auth, exhausted CAS retries, 5xx) means a
// queue that exists did not receive the raised conflicts.
var ErrProposalQueueUnsupported = errors.New("factsync: backend does not serve the open-proposal queue")

// minProposalRefLen is the fewest DISCRIMINATING characters (beyond the fixed
// "prop-" prefix every derived id shares) accepted in a proposal-id prefix; a
// 1-2 character prefix is far too collision-prone to resolve a conflict by,
// and the shared prefix itself carries zero discriminating information.
const minProposalRefLen = 4

// proposalIDPrefix is the fixed lead-in of every derived proposal id.
const proposalIDPrefix = "prop-"

// OpenProposal is one entry of the hosted open set: the merge-core Proposal plus the
// stable, content-derived id the transport addresses it by. factmerge.Proposal has no
// id of its own (the on-disk single-user format keys by candidate), so the id is
// DERIVED here rather than stored — the same member re-raising the same conflict
// produces the same id (re-publishing is idempotent), while two members raising it
// produce distinct ids because ProposedBy is part of the derivation.
type OpenProposal struct {
	ID       string             `json:"id"`
	Proposal factmerge.Proposal `json:"proposal"`
}

// ProposalSet is a branch's open proposal set at a point in time: the CAS ref it was
// read at (empty when the branch has no set yet) and the open proposals. Found is
// false when the branch has no proposal set stored — an empty set, not an error.
type ProposalSet struct {
	Branch    string         `json:"branch"`
	Ref       string         `json:"ref"`
	Found     bool           `json:"found"`
	Proposals []OpenProposal `json:"proposals"`
}

// ProposalID derives the stable id of a proposal from the fields that identify the
// conflict it represents: the action, both fact ids, the branch, and the member who
// raised it. It is deterministic and collision-resistant, so the same conflict has the
// same id at every member and across re-reads of the set.
func ProposalID(p factmerge.Proposal) string {
	h := sha256.New()
	for _, field := range []string{p.Action, p.CandidateID, p.TargetID, p.Branch, p.ProposedBy} {
		h.Write([]byte(field))
		h.Write([]byte{0})
	}
	return "prop-" + hex.EncodeToString(h.Sum(nil))[:16]
}

// OpenProposals stamps derived ids onto merge-core proposals, turning a Sync result's
// Proposals into the hosted set's entries.
func OpenProposals(proposals []factmerge.Proposal) []OpenProposal {
	if len(proposals) == 0 {
		return nil
	}
	out := make([]OpenProposal, 0, len(proposals))
	for _, p := range proposals {
		out = append(out, OpenProposal{ID: ProposalID(p), Proposal: p})
	}
	return out
}

// FindProposal resolves a human/agent-supplied reference against an open set. A
// reference matches, in order: an exact proposal id, an exact candidate fact id (the
// key the single-user `facts review` flow uses), or an unambiguous proposal-id prefix
// of at least minProposalRefLen characters. A prefix matching several proposals is
// ErrAmbiguousProposal — never a silent pick.
func FindProposal(set []OpenProposal, ref string) (OpenProposal, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return OpenProposal{}, fmt.Errorf("%w: empty reference", ErrProposalNotFound)
	}
	for _, p := range set {
		if p.ID == ref {
			return p, nil
		}
	}
	var matches []OpenProposal
	for _, p := range set {
		if p.Proposal.CandidateID == ref {
			matches = append(matches, p)
		}
	}
	// Prefix matching counts only characters past the fixed "prop-" lead-in:
	// the ref "prop" (or "prop-") satisfies any raw length minimum while
	// discriminating between zero proposals, and would silently settle
	// whichever single conflict happens to be open.
	if len(matches) == 0 && strings.HasPrefix(ref, proposalIDPrefix) &&
		len(ref) >= len(proposalIDPrefix)+minProposalRefLen {
		for _, p := range set {
			if strings.HasPrefix(p.ID, ref) {
				matches = append(matches, p)
			}
		}
	}
	switch len(matches) {
	case 0:
		return OpenProposal{}, fmt.Errorf("%w: %s", ErrProposalNotFound, ref)
	case 1:
		return matches[0], nil
	default:
		ids := make([]string, 0, len(matches))
		for _, m := range matches {
			ids = append(ids, m.ID)
		}
		return OpenProposal{}, fmt.Errorf("%w: %s matches %s — pass one full proposal id",
			ErrAmbiguousProposal, ref, strings.Join(ids, ", "))
	}
}

// removeProposal returns the set with the given id removed (a copy; the input is not
// mutated), and whether it was present.
func removeProposal(set []OpenProposal, id string) ([]OpenProposal, bool) {
	out := make([]OpenProposal, 0, len(set))
	found := false
	for _, p := range set {
		if p.ID == id {
			found = true
			continue
		}
		out = append(out, p)
	}
	return out, found
}

// String renders a Decision for the wire and for human output.
func (d Decision) String() string {
	switch d {
	case Accept:
		return "accept"
	case Reject:
		return "reject"
	default:
		return "unknown"
	}
}

// ParseDecision parses the wire/CLI spelling of a Decision.
func ParseDecision(s string) (Decision, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "accept", "apply":
		return Accept, nil
	case "reject":
		return Reject, nil
	default:
		return 0, fmt.Errorf("factsync: unknown decision %q (want accept or reject)", s)
	}
}

// ResolveProposalRequest is one runner-computed resolution pushed to the hosted set.
// Facts is the FULL resolved fact set the runner computed with factmerge (the server
// never merges); Remaining is the full open set with the resolved proposal dropped.
// Both old refs are the CAS preconditions the runner read at — a mismatch on either
// is ErrConflict.
type ResolveProposalRequest struct {
	RepoID          string
	Branch          string
	ProposalID      string
	Decision        Decision
	FactsOldRef     string
	Facts           []factmerge.Record
	ProposalsOldRef string
	Remaining       []OpenProposal

	// FactsUnchanged prunes the proposal set WITHOUT writing the fact head, and is
	// the only way an orphaned proposal ever leaves the open set: one whose fact
	// head is gone entirely (both facts retracted and GC'd) cannot be accepted, and
	// rejecting it used to be impossible too — the empty-head guard fired before the
	// reject path, and pushing an empty fact set is refused on purpose, so the entry
	// sat in every member's list forever. Reject changes no facts, so the write it
	// actually needs is the proposal set alone. Set only with Decision == Reject;
	// Facts is then ignored and MUST NOT be interpreted as "empty the fact set".
	FactsUnchanged bool
}

// ResolveProposalResponse reports where the two heads landed.
type ResolveProposalResponse struct {
	FactsRef     string `json:"factsRef"`
	ProposalsRef string `json:"proposalsRef"`
	Changed      bool   `json:"changed"`
}

// ProposalTransport is the open-proposal-set half of the hosted fact-set surface,
// kept as its own seam so the existing Server interface (implemented by
// internal/factgitmeta) is untouched. ListProposals/GetProposal read the set;
// PublishProposals CAS-swaps the whole set (how a sync's raised conflicts become
// visible to other members); ResolveProposal atomically swaps the fact-set head and
// the proposal set together.
type ProposalTransport interface {
	ListProposals(ctx context.Context, repoID, branch string) (ProposalSet, error)
	GetProposal(ctx context.Context, repoID, branch, proposalID string) (OpenProposal, error)
	PublishProposals(ctx context.Context, repoID, branch, oldRef string, proposals []OpenProposal) (string, error)
	ResolveProposal(ctx context.Context, req ResolveProposalRequest) (ResolveProposalResponse, error)
}

// PublishResult reports the outcome of publishing a sync's raised proposals.
// Proposals is the full hosted open set as of the publish (the union that was
// pushed, or the set read when nothing new needed pushing), so a caller
// keeping a local queue can reconcile it against the hosted truth.
type PublishResult struct {
	Published bool           `json:"published"`
	Ref       string         `json:"ref"`
	Attempts  int            `json:"attempts"`
	Open      int            `json:"open"`
	Proposals []OpenProposal `json:"-"`
}

// PublishRaised makes the proposals a Sync raised visible to every member: read the
// current open set, union the raised ones into it (by derived id — re-raising the same
// conflict is idempotent), and CAS the result back, retrying on a losing swap.
//
// It is deliberately NOT part of Sync: Sync is written against the Server seam alone
// (internal/factgitmeta implements it and has no proposal set), so publishing stays an
// opt-in step a hosted caller takes with a ProposalTransport in hand.
func PublishRaised(ctx context.Context, tr ProposalTransport, repoID, branch string, raised []factmerge.Proposal) (PublishResult, error) {
	entries := OpenProposals(raised)
	if len(entries) == 0 {
		return PublishResult{}, nil
	}
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		set, err := tr.ListProposals(ctx, repoID, branch)
		if err != nil {
			return PublishResult{}, err
		}
		merged := append([]OpenProposal(nil), set.Proposals...)
		known := make(map[string]struct{}, len(merged))
		for _, p := range merged {
			known[p.ID] = struct{}{}
		}
		added := 0
		for _, p := range entries {
			if _, dup := known[p.ID]; dup {
				continue
			}
			known[p.ID] = struct{}{}
			merged = append(merged, p)
			added++
		}
		if added == 0 {
			// Every raised conflict is already open — converged, nothing to push.
			return PublishResult{Published: false, Ref: set.Ref, Attempts: attempt, Open: len(merged), Proposals: merged}, nil
		}
		newRef, err := tr.PublishProposals(ctx, repoID, branch, set.Ref, merged)
		switch {
		case err == nil:
			return PublishResult{Published: true, Ref: newRef, Attempts: attempt, Open: len(merged), Proposals: merged}, nil
		case errors.Is(err, ErrNoChange):
			return PublishResult{Published: false, Ref: set.Ref, Attempts: attempt, Open: len(merged), Proposals: merged}, nil
		case errors.Is(err, ErrConflict):
			continue // another member changed the open set first — re-read and re-union
		default:
			return PublishResult{}, err
		}
	}
	return PublishResult{}, ErrExhausted
}

// ProposalServer is a hosted backend that serves BOTH the fact-set head and the open
// proposal set — what ResolveOpen needs to settle a proposal end to end.
type ProposalServer interface {
	Server
	ProposalTransport
}

// ResolveOpenResult reports the outcome of settling one open proposal.
type ResolveOpenResult struct {
	Proposal     OpenProposal `json:"proposal"`
	Decision     string       `json:"decision"`
	FactsRef     string       `json:"facts_ref"`
	ProposalsRef string       `json:"proposals_ref"`
	Attempts     int          `json:"attempts"`
	Remaining    int          `json:"remaining"`
}

// ResolveOpen settles one open proposal end to end against the hosted heads: list the
// open set, locate the proposal, read the fact-set head, apply the member's Decision
// with the merge core (Resolve → factmerge.ApplyProposal / RejectProposal), and push
// the resolved facts plus the shrunken proposal set under CAS — retrying the whole
// read-resolve-CAS loop when either head moved underneath (ErrConflict), exactly like
// Sync.
//
// The proposal is addressed by ref (id, id prefix, or candidate fact id; see
// FindProposal). A proposal another member already settled is ErrProposalNotFound; a
// proposal whose facts have vanished is ErrProposalNotApplicable. Egress redaction is
// applied by the transport on the pushed facts, so no member's local transcript path
// leaves this machine.
func ResolveOpen(ctx context.Context, srv ProposalServer, repoID, branch, ref string, decision Decision, now time.Time) (ResolveOpenResult, error) {
	if decision != Accept && decision != Reject {
		return ResolveOpenResult{}, errors.New("factsync: unknown review decision")
	}
	// The human-supplied ref is resolved to a proposal exactly once. On a CAS
	// retry the open set has changed by definition, and a candidate-id or
	// prefix ref could rebind to a DIFFERENT proposal that entered the set
	// meanwhile — silently settling a conflict the reviewer never saw. After
	// the first resolution the loop addresses the pinned id only; a pinned
	// proposal that vanished between attempts was settled by someone else.
	pinnedID := ""
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		set, err := srv.ListProposals(ctx, repoID, branch)
		if err != nil {
			return ResolveOpenResult{}, err
		}
		var target OpenProposal
		if pinnedID == "" {
			target, err = FindProposal(set.Proposals, ref)
			if err != nil {
				return ResolveOpenResult{}, err
			}
			pinnedID = target.ID
		} else {
			target, err = FindProposal(set.Proposals, pinnedID)
			if err != nil {
				return ResolveOpenResult{}, err
			}
		}

		headRef, plaintext, found, err := srv.Current(ctx, repoID, branch)
		if err != nil {
			return ResolveOpenResult{}, err
		}
		headGone := !found || len(plaintext) == 0
		if headGone && decision == Accept {
			// Accept needs both facts to still exist; with no head there is nothing
			// to merge into. Fail loudly rather than pushing an empty head.
			return ResolveOpenResult{}, ErrProposalNotApplicable
		}
		if headGone {
			// Reject an orphan: the fact head is gone, so the only write required is
			// pruning the proposal set. Without this the entry is unremovable — it
			// cannot be accepted, and the old guard blocked reject too, so it stayed
			// in every member's open list forever.
			remaining, _ := removeProposal(set.Proposals, target.ID)
			resp, resolveErr := srv.ResolveProposal(ctx, ResolveProposalRequest{
				RepoID:          repoID,
				Branch:          branch,
				ProposalID:      target.ID,
				Decision:        decision,
				FactsOldRef:     headRef,
				FactsUnchanged:  true,
				ProposalsOldRef: set.Ref,
				Remaining:       remaining,
			})
			switch {
			case resolveErr == nil, errors.Is(resolveErr, ErrNoChange):
				return ResolveOpenResult{
					Proposal:     target,
					Decision:     decision.String(),
					FactsRef:     headRef,
					ProposalsRef: resp.ProposalsRef,
					Attempts:     attempt,
					Remaining:    len(remaining),
				}, nil
			case errors.Is(resolveErr, ErrConflict):
				continue // a concurrent member moved the proposal set — re-read
			default:
				return ResolveOpenResult{}, resolveErr
			}
		}
		facts, err := factmerge.ParseNDJSON(bytes.NewReader(plaintext))
		if err != nil {
			return ResolveOpenResult{}, err
		}
		// Same trust boundary as Sync: the head is member-writable, so a record's
		// id is only meaningful once it has been re-derived from its own content.
		// Settling a proposal rewrites fact status, so an unverified head here
		// would let a forged record decide which statement survives.
		if err := factmerge.VerifyIdentities(facts); err != nil {
			return ResolveOpenResult{}, fmt.Errorf("factsync: shared fact-set head for %s/%s is not trustworthy: %w", repoID, branch, err)
		}

		resolved, err := Resolve(facts, target.Proposal, decision, now)
		if err != nil {
			// A proposal whose facts are no longer both in the head cannot be
			// APPLIED — but it can be rejected: reject changes no facts, and
			// removing the entry is the only way such a proposal ever leaves
			// the open set (nothing else prunes it, so it would otherwise sit
			// in every member's list forever). Accept keeps failing loudly.
			if decision == Reject && errors.Is(err, ErrProposalNotApplicable) {
				resolved = facts
			} else {
				return ResolveOpenResult{}, err
			}
		}
		remaining, _ := removeProposal(set.Proposals, target.ID)

		resp, err := srv.ResolveProposal(ctx, ResolveProposalRequest{
			RepoID:          repoID,
			Branch:          branch,
			ProposalID:      target.ID,
			Decision:        decision,
			FactsOldRef:     headRef,
			Facts:           resolved,
			ProposalsOldRef: set.Ref,
			Remaining:       remaining,
		})
		switch {
		case err == nil, errors.Is(err, ErrNoChange):
			factsRef := resp.FactsRef
			if factsRef == "" {
				factsRef = headRef
			}
			return ResolveOpenResult{
				Proposal:     target,
				Decision:     decision.String(),
				FactsRef:     factsRef,
				ProposalsRef: resp.ProposalsRef,
				Attempts:     attempt,
				Remaining:    len(remaining),
			}, nil
		case errors.Is(err, ErrConflict):
			continue // a concurrent member moved a head — re-read both and re-resolve
		default:
			return ResolveOpenResult{}, err
		}
	}
	return ResolveOpenResult{}, ErrExhausted
}
