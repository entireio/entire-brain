// Package factsync is the runner-side read-merge-CAS loop that syncs a member's
// durable facts into the hosted, shared fact-set head. It is where the two P1.M2
// halves meet: the deterministic merge engine (internal/factmerge) and the hosted
// atomic head (entire-api's FactSetStore, reached here through the Server seam).
//
// Per ADR-P1-E the MERGE runs on the runner (this package), not in the API server:
// the server only stores encrypted blobs and compare-and-swaps the branch head, so
// it never needs the cgo/SQLite-heavy brain merge core. Sync pulls the current head
// in plaintext, keep-both promotes the member's local facts into it via factmerge,
// and pushes the result back under CAS — retrying on a losing swap.
package factsync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ashtom/entire-brain/internal/factmerge"
)

// ErrConflict is what a Server.Advance returns when the head moved under it (a
// concurrent member advanced first) — the signal for Sync to re-read and re-merge.
var ErrConflict = errors.New("factsync: fact-set head changed (CAS conflict)")

// ErrNoChange is what a Server.Advance returns when the merged blob is byte-identical
// to the current head — the member's facts were all already present. Sync treats it as
// "converged, nothing to publish", not an error.
var ErrNoChange = errors.New("factsync: fact-set unchanged by merge")

// ErrExhausted is returned when Sync cannot land its merge within maxAttempts CAS
// tries — sustained contention the caller should retry later or escalate.
var ErrExhausted = errors.New("factsync: exceeded CAS retries")

// maxAttempts bounds the read-merge-CAS loop so pathological contention fails loudly
// instead of spinning forever.
const maxAttempts = 32

// Server is the runner's view of the hosted fact-set head (entire-api's FactSetStore
// over HTTP). Current returns the branch head's content ref and the DECRYPTED facts
// blob (found=false, nil blob when the branch has no head yet — merge into empty).
// Advance seals+stores the merged plaintext and compare-and-swaps the head from oldRef
// onto its content ref, returning ErrConflict on a losing swap and ErrNoChange when the
// merge produced exactly the current content.
type Server interface {
	Current(ctx context.Context, repoID, branch string) (ref string, plaintext []byte, found bool, err error)
	Advance(ctx context.Context, repoID, branch, oldRef string, plaintext []byte) (newRef string, err error)
}

// Result reports the outcome of a Sync: whether the head advanced (Published), the new
// content ref, how many CAS attempts it took, and any review Proposals the keep-both
// merge queued (cross-member conflicts a human must resolve — the M2.4 surface).
type Result struct {
	Published bool
	NewRef    string
	Attempts  int
	Proposals []factmerge.Proposal

	// Facts is the fact set this sync settled on: the merged content when the CAS
	// won, or the head as read when it converged with no change. The runner already
	// computed it, so a caller needing the post-sync head (to check which proposals
	// are still live, or to mirror settlements into local facts) can use this
	// instead of issuing another Current — one fewer network round-trip per sync.
	Facts []factmerge.Record
}

// Sync runs the read-merge-CAS loop: pull the head, keep-both promote local into it via
// factmerge, push under CAS, retry on ErrConflict. `now` is injected for determinism.
// memberID identifies the syncing member; it is stamped onto every review Proposal the
// merge raises (ProposedBy) so a cross-member conflict is attributable and routable.
// A keep-both merge NEVER silently drops a member's fact (ADR-P1-G): identical facts
// (same content id) union their provenance; genuine conflicts are kept and queued as
// routed Proposals in the Result (resolve them with Resolve).
func Sync(ctx context.Context, srv Server, repoID, branch, memberID string, local []factmerge.Record, now time.Time) (Result, error) {
	if memberID == "" {
		// A blank memberID would stamp ProposedBy="" (dropped by omitempty), making a
		// cross-member proposal indistinguishable from a local one — routing lost. Reject.
		return Result{}, errors.New("factsync: memberID is required (proposal routing)")
	}
	// Redact local-only provenance coordinates before these facts leave for the shared
	// head — no member's local filesystem path leaks cross-member (opaque ids are kept,
	// so provenance still unions). Copies; the caller's local facts are not mutated.
	local = SanitizeForEgress(local)
	// Verify this member's own facts BEFORE publishing them. The head check below
	// fails closed, so one malformed record reaching the shared head would break
	// every other member's sync — a failure the member that produced it should
	// take, and take here, rather than exporting it.
	if err := factmerge.VerifyIdentities(local); err != nil {
		return Result{}, fmt.Errorf("factsync: local fact set for %s/%s is not publishable: %w", repoID, branch, err)
	}
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		ref, plaintext, found, err := srv.Current(ctx, repoID, branch)
		if err != nil {
			return Result{}, err
		}
		var head []factmerge.Record
		if found && len(plaintext) > 0 {
			head, err = factmerge.ParseNDJSON(bytes.NewReader(plaintext))
			if err != nil {
				return Result{}, err
			}
			// The head is written by every member with push access, so nothing about
			// it is authenticated by having arrived over the transport. Re-derive each
			// record's content id before merging: without this, a record carrying
			// ANOTHER member's id with attacker-chosen text makes Promote's id match
			// read as "already present, union the provenance", silently discarding the
			// victim's real statement and keeping the forgery — with the victim's own
			// anchors attached to it. Fail closed rather than merge a head one record
			// of which is provably not what it claims to be.
			if err := factmerge.VerifyIdentities(head); err != nil {
				return Result{}, fmt.Errorf("factsync: shared fact-set head for %s/%s is not trustworthy: %w", repoID, branch, err)
			}
		}

		// Cross-member merge is structurally cross-branch promote: the member's local
		// facts are the "source" promoted (keep-both) into the current head "target".
		merged, proposals, _ := factmerge.Promote(local, head, "keep-both", branch, now)
		// Stamp routing: every conflict this member's sync raised is attributed to it.
		for i := range proposals {
			proposals[i].ProposedBy = memberID
		}

		var buf bytes.Buffer
		if err := factmerge.WriteNDJSON(&buf, merged); err != nil {
			return Result{}, err
		}
		if buf.Len() == 0 {
			// The merge produced an EMPTY fact-set (an empty head and no active local
			// facts to promote). There is nothing to publish — a head always points at
			// content, and Advance rejects empty plaintext — so report converged rather
			// than push an empty blob. (A non-empty head can never merge to empty: promote
			// only grows the target.)
			return Result{Published: false, NewRef: ref, Attempts: attempt, Proposals: proposals, Facts: merged}, nil
		}

		newRef, err := srv.Advance(ctx, repoID, branch, ref, buf.Bytes())
		switch {
		case err == nil:
			return Result{Published: true, NewRef: newRef, Attempts: attempt, Proposals: proposals, Facts: merged}, nil
		case errors.Is(err, ErrNoChange):
			return Result{Published: false, NewRef: ref, Attempts: attempt, Proposals: proposals, Facts: merged}, nil
		case errors.Is(err, ErrConflict):
			continue // a concurrent member advanced first — re-read the newer head and re-merge
		default:
			return Result{}, err
		}
	}
	return Result{}, ErrExhausted
}
