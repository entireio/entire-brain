package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/entireio/entire-brain/internal/apiurl"
	"github.com/entireio/entire-brain/internal/factgitmeta"
	"github.com/entireio/entire-brain/internal/factmerge"
	"github.com/entireio/entire-brain/internal/factsync"

	"github.com/spf13/cobra"
)

const (
	// factsBackendLocalGitmeta is the default: a bare, on-disk git-meta store
	// under the plugin cache; fully local, no network/entiredb/Postgres.
	factsBackendLocalGitmeta = "local-gitmeta"
	// factsBackendHTTP drives the hosted entire-api FactSetStore (factsync.HTTPServer).
	factsBackendHTTP = "http"
	// envFactsBackend selects the backend when --facts-backend is unset.
	envFactsBackend = "ENTIRE_BRAIN_FACTS_BACKEND"
)

// factsSyncOptions bundles the flag state for `facts sync`.
type factsSyncOptions struct {
	branch  string
	backend string
	member  string
	// http backend only (mirror publish's flag/env resolution):
	apiURL string
	token  string
	repoID string

	jsonOut bool
}

func newFactsSyncCommand(opts Options) *cobra.Command {
	var syncOpts factsSyncOptions
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Read-merge-CAS this branch's local facts into the shared fact-set head",
		Long: `sync runs the factsync read-merge-CAS loop for the current branch: pull the
current fact-set head, keep-both merge this member's local facts into it, and
compare-and-swap the result back; retrying on a losing swap.

Backends (--facts-backend, or ` + envFactsBackend + `):
  local-gitmeta  (default) a bare, on-disk git-meta store under the brain cache
                 (<cache>/brain/<repo-key>/gitmeta.git); fully local, no network,
                 entiredb, or Postgres.
  http           the hosted entire-api FactSetStore; opt-in, requires
                 --api-url/ENTIRE_API_URL, --repo-id/ENTIRE_REPO_ID,
                 --token/ENTIRE_API_TOKEN and ENTIRE_BRAIN_ALLOW_HOSTED=1.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runFactsSync(cmd, opts, syncOpts)
		},
	}
	cmd.Flags().StringVar(&syncOpts.branch, "branch", "", "Branch to sync (default: current branch)")
	cmd.Flags().StringVar(&syncOpts.backend, "facts-backend", "", "Fact-set backend: local-gitmeta (default) or http")
	cmd.Flags().StringVar(&syncOpts.member, "member", "", "Member identity stamped on cross-member proposals (default: git user.email)")
	cmd.Flags().StringVar(&syncOpts.repoID, "repo-id", "", "Target repo id for the http backend (overrides ENTIRE_REPO_ID)")
	cmd.Flags().StringVar(&syncOpts.apiURL, "api-url", "", "Entire API base URL for the http backend, https:// (overrides ENTIRE_API_URL)")
	cmd.Flags().StringVar(&syncOpts.token, "token", "", "Entire API bearer token for the http backend (overrides ENTIRE_API_TOKEN)")
	cmd.Flags().BoolVar(&syncOpts.jsonOut, "json", false, "Emit the sync result as JSON")
	return cmd
}

func runFactsSync(cmd *cobra.Command, opts Options, syncOpts factsSyncOptions) error {
	ctx := cmd.Context()

	repoDir, local, err := resolveLocalTargetRepoDir(ctx, opts.Runner, agentSurfaceTarget(opts, nil))
	if err != nil {
		return err
	}
	if !local {
		return fmt.Errorf("facts sync requires a local repository path")
	}
	storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return err
	}

	branch := strings.TrimSpace(syncOpts.branch)
	if branch == "" {
		if current, gitErr := gitScalar(ctx, opts.Runner, repoDir, "branch", "--show-current"); gitErr == nil {
			branch = strings.TrimSpace(current)
		}
	}
	if branch == "" {
		branch = distillDefaultBranch
	}

	facts, err := loadFacts(storage.BrainDir, branch)
	if err != nil {
		return err
	}
	memberID := resolveSyncMemberID(ctx, opts.Runner, repoDir, syncOpts.member)

	srv, repoID, backendLabel, err := buildFactsSyncBackend(opts, resolveFactsBackendName(syncOpts.backend), storage, syncOpts)
	if err != nil {
		return err
	}

	// The merge runs on this runner (ADR-P1-E); the backend only stores the blob
	// and CAS-swaps the head. Sync retries on a losing swap and never drops a
	// member's fact.
	res, err := factsync.Sync(ctx, srv, repoID, branch, memberID, facts, opts.Now().UTC())
	if err != nil {
		return err
	}
	if err := persistFactsSyncProposals(storage.BrainDir, branch, res.Proposals); err != nil {
		return err
	}

	// Publish the WHOLE local review queue — this sync's raised conflicts plus any
	// earlier entries that never reached the shared set (a previously failed
	// publish, or a queue populated before a hosted backend was configured) — so
	// other members can review them with `facts proposals`. Only backends that
	// serve the queue (the hosted http one) can take them; the local git-meta
	// store has no proposal set and is left untouched. After a successful publish
	// the hosted set is the source of truth, and the local queue is reconciled to
	// it so a conflict a teammate settled hosted-side stops being re-listed (and
	// re-appliable) here.
	//
	// Failure discipline: a backend that does not serve the queue at all
	// (entire-api predating the endpoint) is a warning — the sync itself is
	// complete and the queue is a hosted add-on. Any OTHER failure fails the
	// command: the facts already advanced the shared head, so a swallowed
	// publish error would leave conflicts only this member can see.
	var pub factsync.PublishResult
	var pubErr error
	if transport, hasQueue := srv.(factsync.ProposalTransport); hasQueue {
		localQueue, queueErr := loadFactProposals(storage.BrainDir, branch)
		if queueErr != nil {
			return fmt.Errorf("facts sync: load review queue: %w", queueErr)
		}
		ledger, ledgerErr := loadSharedProposalLedger(storage.BrainDir, branch)
		if ledgerErr != nil {
			return fmt.Errorf("facts sync: load shared-proposal ledger: %w", ledgerErr)
		}
		// The post-sync head gates what may be published AND supplies the settlements
		// mirrored into local facts below. Sync already computed it, so take it from
		// the result rather than issuing another Current — that read is a full network
		// round-trip against the hosted backend on every sync. Fall back to a real
		// read only if the result carries nothing (an empty converged head).
		hostedFacts := res.Facts
		if len(hostedFacts) == 0 {
			fetched, headErr := readSharedFactHead(ctx, srv, repoID, branch)
			if headErr != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not read the shared fact head; skipping settled-proposal checks this sync: %v\n", headErr)
			}
			hostedFacts = fetched
		}

		// Publish only entries that are member-attributed AND have never reached
		// the hosted set. Distill's single-user review backlog shares the same
		// local queue but carries no ProposedBy: it is this member's private
		// pending-review state, not a cross-member conflict, and must never
		// egress (its unattributed id derivation would also collide across
		// members). An already-shared entry absent from the hosted set was
		// settled by another member; re-publishing it would resurrect exactly
		// the conflict they resolved, so reconciliation prunes it instead.
		//
		// The ledger alone is not enough: it lives in this checkout, so a SECOND
		// clone of the same member starts with an empty one and would republish
		// conflicts the team already settled. The shared head is the cross-clone
		// source of truth — a settled conflict is no longer applicable to it — so
		// an unapplicable proposal is treated as settled regardless of the ledger.
		toPublish := make([]factProposal, 0, len(localQueue))
		for _, p := range localQueue {
			if strings.TrimSpace(p.ProposedBy) == "" {
				continue
			}
			if _, shared := ledger[factsync.ProposalID(p)]; shared {
				continue
			}
			if len(hostedFacts) > 0 && !proposalStillOpenAgainst(hostedFacts, p) {
				continue
			}
			toPublish = append(toPublish, p)
		}
		if len(toPublish) > 0 {
			pub, pubErr = factsync.PublishRaised(ctx, transport, repoID, branch, toPublish)
			switch {
			case pubErr == nil:
			case errors.Is(pubErr, factsync.ErrProposalQueueUnsupported):
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: backend has no shared review queue; %d proposal(s) stay in the local queue ('facts review'): %v\n", len(localQueue), pubErr)
			default:
				return fmt.Errorf("facts sync: facts head advanced to %s, but sharing %d proposal(s) to the review queue failed: %w (they remain reviewable locally with 'facts review', and the next 'facts sync' republishes them)",
					refOrNone(res.NewRef), len(toPublish), pubErr)
			}
		} else if set, listErr := transport.ListProposals(ctx, repoID, branch); listErr != nil {
			// Nothing needed sharing: this list exists only to reconcile the
			// local queue, so a transient failure must not fail a sync whose
			// head-advance fully succeeded. The next sync reconciles.
			pubErr = listErr
			if !errors.Is(listErr, factsync.ErrProposalQueueUnsupported) {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not read the shared review queue to reconcile the local one; will retry next sync: %v\n", listErr)
			}
		} else {
			pub = factsync.PublishResult{Ref: set.Ref, Open: len(set.Proposals), Proposals: set.Proposals}
			if pub.Proposals == nil {
				pub.Proposals = []factsync.OpenProposal{}
			}
		}
		if pubErr == nil && pub.Proposals != nil {
			// hostedFacts was read above; a failed read only costs this sync's
			// mirroring — the queue entry stays until reconciled, so the next
			// sync retries.
			if err := reconcileLocalProposalQueue(storage.BrainDir, branch, pub.Proposals, hostedFacts, opts.Now().UTC()); err != nil {
				return fmt.Errorf("facts sync: reconcile review queue: %w", err)
			}
		}
	}

	if syncOpts.jsonOut {
		payload := map[string]any{
			"branch":             branch,
			"backend":            backendLabel,
			"member":             memberID,
			"published":          res.Published,
			"ref":                res.NewRef,
			"attempts":           res.Attempts,
			"proposals":          res.Proposals,
			"facts":              len(facts),
			"proposals_shared":   pub.Published,
			"proposals_open":     pub.Open,
			"proposals_head_ref": pub.Ref,
		}
		if pubErr != nil {
			payload["proposals_share_error"] = pubErr.Error()
		}
		return writeJSON(cmd, payload)
	}

	out := cmd.OutOrStdout()
	if res.Published {
		fmt.Fprintf(out, "synced %d local fact(s) on %s via %s: head advanced to %s (%d attempt(s))\n",
			len(facts), branch, backendLabel, res.NewRef, res.Attempts)
	} else {
		fmt.Fprintf(out, "synced %d local fact(s) on %s via %s: no change; head at %s (%d attempt(s))\n",
			len(facts), branch, backendLabel, refOrNone(res.NewRef), res.Attempts)
	}
	if len(res.Proposals) > 0 {
		fmt.Fprintf(out, "%d cross-member conflict(s) queued for review:\n", len(res.Proposals))
		for _, p := range res.Proposals {
			fmt.Fprintf(out, "  %s %s -> %s (by %s, confidence %.2f)\n", p.Action, p.CandidateID, p.TargetID, p.ProposedBy, p.Confidence)
		}
		if pub.Published {
			fmt.Fprintf(out, "shared to the review queue (%d open); resolve with 'facts proposals apply|reject <proposal>'\n", pub.Open)
		}
	}
	return nil
}

// persistFactsSyncProposals appends cross-member conflicts to the same durable
// queue consumed by facts review/status and the recall pending-review guard.
// The brain write lock makes the read-merge-write atomic with other local
// writers, while writeFactProposals supplies stable ordering and deduplication.
func persistFactsSyncProposals(brainDir, branch string, proposals []factProposal) error {
	if len(proposals) == 0 {
		return nil
	}
	return withBrainWriteLock(brainDir, func() error {
		existing, err := loadFactProposals(brainDir, branch)
		if err != nil {
			return fmt.Errorf("facts sync: load review queue: %w", err)
		}
		if err := writeFactProposals(brainDir, branch, append(existing, proposals...)); err != nil {
			return fmt.Errorf("facts sync: persist review queue: %w", err)
		}
		return nil
	})
}

// factsSharedLedgerFileName records, per branch, the derived ids of local
// proposals that have REACHED the hosted open set at some point. Without it a
// settled proposal (published, then resolved by a teammate: gone from the
// hosted set) is indistinguishable from a never-published one, and the next
// sync would resurrect exactly the conflict the teammate settled.
const factsSharedLedgerFileName = "proposals-shared.json"

func factsSharedLedgerRelPath(branch string) string {
	return filepath.ToSlash(filepath.Join(factsBranchRelDir(branch), factsSharedLedgerFileName))
}

func loadSharedProposalLedger(brainDir, branch string) (map[string]struct{}, error) {
	path := filepath.Join(brainDir, filepath.FromSlash(factsSharedLedgerRelPath(branch)))
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]struct{}{}, nil
		}
		return nil, err
	}
	var ids []string
	if err := json.Unmarshal(data, &ids); err != nil {
		return nil, fmt.Errorf("parse %s: %w", factsSharedLedgerRelPath(branch), err)
	}
	out := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		out[id] = struct{}{}
	}
	return out, nil
}

func writeSharedProposalLedger(brainDir, branch string, ids map[string]struct{}) error {
	if len(ids) == 0 {
		return removeBrainRelativeFile(brainDir, factsSharedLedgerRelPath(branch))
	}
	sorted := make([]string, 0, len(ids))
	for id := range ids {
		sorted = append(sorted, id)
	}
	sort.Strings(sorted)
	data, err := json.Marshal(sorted)
	if err != nil {
		return err
	}
	return writeBrainRelativeFileAtomic(brainDir, factsSharedLedgerRelPath(branch), data, 0o600)
}

// reconcileLocalProposalQueue rewrites the local review queue after a successful
// publish so it converges on the hosted open set: an entry the ledger says was
// shared but the hosted set no longer carries was settled by another member and
// is dropped; entries still hosted-open stay; entries never yet shared stay for
// the next publish. The ledger is then rewritten to the hosted set, which by
// definition holds every currently shared proposal.
// readSharedFactHead pulls the shared fact-set head and parses it. An absent head
// yields no records and no error — a branch nobody has synced yet is a normal state,
// not a failure. Only a transport fault or unparseable content is an error, and the
// caller degrades rather than failing a sync whose head-advance already succeeded.
func readSharedFactHead(ctx context.Context, srv factsync.Server, repoID, branch string) ([]factRecord, error) {
	_, plaintext, found, err := srv.Current(ctx, repoID, branch)
	if err != nil {
		return nil, err
	}
	if !found || len(plaintext) == 0 {
		return nil, nil
	}
	parsed, err := factmerge.ParseNDJSON(bytes.NewReader(plaintext))
	if err != nil {
		return nil, fmt.Errorf("parse shared fact head: %w", err)
	}
	// The head is written by every member with push access. Re-derive each
	// record's content id before any of it reaches local state: this read feeds
	// proposal reconciliation, which supersedes and removes LOCAL facts by id.
	if err := factmerge.VerifyIdentities(parsed); err != nil {
		return nil, fmt.Errorf("shared fact head for %s/%s is not trustworthy: %w", repoID, branch, err)
	}
	return parsed, nil
}

// proposalStillOpenAgainst reports whether p is a live conflict against the shared
// head — both of its facts still present, and the target not already superseded by
// the candidate. A settled or stale proposal fails this and must not be republished.
//
// This is what makes settlement stick ACROSS CLONES: the shared-proposal ledger is
// per-checkout, so a second clone of the same member has no record of what was
// published and would resurrect conflicts the team already resolved. The head is
// the same for every clone.
func proposalStillOpenAgainst(hostedFacts []factRecord, p factProposal) bool {
	ci := indexOfFact(hostedFacts, p.CandidateID)
	ti := indexOfFact(hostedFacts, p.TargetID)
	if ci < 0 || ti < 0 {
		return false // one side is gone: merged away, retracted, or GC'd
	}
	if hostedFacts[ti].Status == factStatusSuperseded && hostedFacts[ti].SupersededBy == p.CandidateID {
		return false // already superseded exactly as this proposal asked
	}
	if hostedFacts[ci].Status == factStatusSuperseded && hostedFacts[ci].SupersededBy == p.TargetID {
		return false // settled the other way round
	}
	return true
}

// settledHostedAsAccept reports whether the shared fact-set head shows p as
// ACCEPTED. A settled proposal carries no decision field, so the decision is
// derived from the effect ApplyProposal would have left on the head:
//
//	merge     accept removes the candidate and keeps the target
//	supersede accept flips the target to superseded, pointing at the candidate
//
// Anything else (both ids still present and merely unlinked, or a fact that has
// since moved on) reads as a reject: reject changes no fact status, so mirroring
// it locally is limited to clearing the conflict cross-link.
func settledHostedAsAccept(hostedFacts []factRecord, p factProposal) bool {
	ci := indexOfFact(hostedFacts, p.CandidateID)
	ti := indexOfFact(hostedFacts, p.TargetID)
	if p.Action == factActionMerge {
		return ci < 0 && ti >= 0
	}
	return ti >= 0 && hostedFacts[ti].Status == factStatusSuperseded && hostedFacts[ti].SupersededBy == p.CandidateID
}

// applySettlementLocally folds a settlement another member made on the shared
// head into THIS member's local facts, using the same primitives the hosted
// side ran so the two cannot drift.
//
// Without this the settlement does not stick: the losing member's local copy
// still holds the retired fact, and the next sync's keep-both Promote re-adds it
// and re-raises the same conflict under a fresh id — an unresolvable loop for
// whichever member lost the merge.
func applySettlementLocally(facts []factRecord, p factProposal, accepted bool, now time.Time) ([]factRecord, bool) {
	if !accepted {
		// Reject: clear the local cross-link so the pair stops reading as
		// conflicting. With either side absent there is no link to clear, so report
		// no change rather than rewriting the fact store and bumping the source
		// manifest for nothing.
		if indexOfFact(facts, p.CandidateID) < 0 || indexOfFact(facts, p.TargetID) < 0 {
			return facts, false
		}
		return rejectProposal(facts, p), true
	}
	updated, err := applyProposal(facts, p, now)
	if err != nil {
		// The local copy no longer holds both sides (already retracted, GC'd, or
		// settled here). The hosted decision is still authoritative and already
		// recorded; there is nothing left locally to reconcile.
		return facts, false
	}
	return updated, true
}

func reconcileLocalProposalQueue(brainDir, branch string, hosted []factsync.OpenProposal, hostedFacts []factRecord, now time.Time) error {
	hostedIDs := make(map[string]struct{}, len(hosted))
	for _, p := range hosted {
		hostedIDs[p.ID] = struct{}{}
	}
	return withBrainWriteLock(brainDir, func() error {
		ledger, err := loadSharedProposalLedger(brainDir, branch)
		if err != nil {
			return err
		}
		current, err := loadFactProposals(brainDir, branch)
		if err != nil {
			return err
		}
		// A settled entry may only be pruned once its settlement has been mirrored
		// into local facts. Mirroring needs the shared head, so with no head to read
		// the entry STAYS QUEUED and the next sync retries: pruning it here would
		// drop the only record of what still needs mirroring, leaving the retired
		// fact active locally with nothing left to reconcile it against — the exact
		// defect this function exists to close, reintroduced through the back door.
		canMirror := len(hostedFacts) > 0
		next := make([]factProposal, 0, len(current))
		settled := make([]factProposal, 0, len(current))
		for _, p := range current {
			id := factsync.ProposalID(p)
			_, stillOpen := hostedIDs[id]
			_, wasShared := ledger[id]
			if stillOpen || !wasShared {
				next = append(next, p)
				continue
			}
			if !canMirror {
				next = append(next, p) // settled, but keep it until it can be mirrored
				continue
			}
			// Shared once, gone from the hosted set: another member settled it.
			settled = append(settled, p)
		}
		// Mirror BEFORE dropping the queue entries, so a crash between the two
		// writes leaves the entries in place and the next sync redoes the mirror
		// (idempotent) rather than losing it.
		if len(settled) > 0 {
			facts, factsErr := loadFacts(brainDir, branch)
			if factsErr != nil {
				return factsErr
			}
			changed := false
			for _, p := range settled {
				updated, did := applySettlementLocally(facts, p, settledHostedAsAccept(hostedFacts, p), now)
				facts = updated
				changed = changed || did
			}
			if changed {
				if err := writeFacts(brainDir, branch, facts); err != nil {
					return err
				}
				if err := updateFactSourceManifestLocked(brainDir, now); err != nil {
					return err
				}
			}
		}
		// Queue first, ledger second: a crash between the writes then leaves a
		// stale EXTRA ledger id (harmless — it only suppresses a republish),
		// whereas the reverse order would mark still-queued settled proposals
		// as never-shared and resurrect them on the next sync.
		if len(next) != len(current) {
			if err := writeFactProposals(brainDir, branch, next); err != nil {
				return err
			}
		}
		merged := make(map[string]struct{}, len(hostedIDs)+len(ledger))
		for id := range hostedIDs {
			merged[id] = struct{}{}
		}
		for id := range ledger {
			merged[id] = struct{}{}
		}
		return writeSharedProposalLedger(brainDir, branch, merged)
	})
}

// publishRaisedProposals shares a sync's raised conflicts through the backend's open
// proposal queue when it has one. A backend that only implements the fact-set head
// seam (internal/factgitmeta's local store) is a no-op, so the default local flow is
// unchanged and never reaches the network.
func publishRaisedProposals(ctx context.Context, srv factsync.Server, repoID, branch string, raised []factProposal) (factsync.PublishResult, error) {
	if len(raised) == 0 {
		return factsync.PublishResult{}, nil
	}
	transport, ok := srv.(factsync.ProposalTransport)
	if !ok {
		return factsync.PublishResult{}, nil
	}
	return factsync.PublishRaised(ctx, transport, repoID, branch, raised)
}

// resolveFactsBackendName folds the --facts-backend flag, the env override, and
// the default into a single backend name.
func resolveFactsBackendName(flagVal string) string {
	if v := strings.TrimSpace(flagVal); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv(envFactsBackend)); v != "" {
		return v
	}
	return factsBackendLocalGitmeta
}

// buildFactsSyncBackend constructs the selected Server, returning it, the repoID
// to thread through Sync, and a human label for output. The local-gitmeta path
// is keyed by the repo storage key; the http path mirrors publish's opt-in gates
// and flag/env resolution.
func buildFactsSyncBackend(opts Options, backend string, storage repoStorage, syncOpts factsSyncOptions) (factsync.Server, string, string, error) {
	switch backend {
	case factsBackendLocalGitmeta:
		gitDir, err := gitmetaDirForKey(opts.Env, storage.Key)
		if err != nil {
			return nil, "", "", err
		}
		b, err := factgitmeta.NewLocalBackend(gitDir, storage.Key, opts.Now)
		if err != nil {
			return nil, "", "", err
		}
		return b, storage.Key, "local-gitmeta (" + gitDir + ")", nil

	case factsBackendHTTP:
		// The master local-only switch always wins over the hosted opt-in.
		if brainNoEgressMode() {
			return nil, "", "", fmt.Errorf("no_egress: hosted fact sync is disabled while ENTIRE_BRAIN_NO_EGRESS or ENTIRE_BRAIN_LOCAL_ONLY is set; unset it or use --facts-backend=local-gitmeta")
		}
		// Hosted egress is opt-in and fails closed (mirrors publish).
		if !envBool(envBrainAllowHosted) {
			return nil, "", "", fmt.Errorf("hosted_sync_disabled: the http fact backend is opt-in and off by default; set %s=1 to enable it, or use --facts-backend=local-gitmeta", envBrainAllowHosted)
		}
		repoID := publishFlagOrEnv(syncOpts.repoID, envRepoID)
		if repoID == "" {
			return nil, "", "", fmt.Errorf("facts sync: target repo id is required for the http backend; set --repo-id or %s", envRepoID)
		}
		baseURL := publishFlagOrEnv(syncOpts.apiURL, envAPIBaseURL)
		if baseURL == "" {
			return nil, "", "", fmt.Errorf("facts sync: API base URL is required for the http backend; set --api-url or %s", envAPIBaseURL)
		}
		// The fact-set advance carries this member's facts and the bearer token, so
		// the target must clear the scheme floor before the backend is built.
		baseURL, urlErr := apiurl.Validate(baseURL)
		if urlErr != nil {
			return nil, "", "", fmt.Errorf("facts sync: %w", urlErr)
		}
		token := publishFlagOrEnv(syncOpts.token, envAPIToken)
		if token == "" {
			return nil, "", "", fmt.Errorf("facts sync: API token is required for the http backend; set --token or %s", envAPIToken)
		}
		return &factsync.HTTPServer{BaseURL: baseURL, Token: token}, repoID, "http (" + strings.TrimRight(baseURL, "/") + ")", nil

	default:
		return nil, "", "", fmt.Errorf("unknown --facts-backend %q (want %q or %q)", backend, factsBackendLocalGitmeta, factsBackendHTTP)
	}
}

// resolveSyncMemberID derives the member identity stamped on cross-member
// proposals: the explicit --member, else the repo's git user.email, else
// $USER@<hostname>, else "local". A non-empty id is required (Sync rejects "").
func resolveSyncMemberID(ctx context.Context, runner CommandRunner, repoDir, override string) string {
	if v := strings.TrimSpace(override); v != "" {
		return v
	}
	if email, err := gitScalar(ctx, runner, repoDir, "config", "--get", "user.email"); err == nil {
		if e := strings.TrimSpace(email); e != "" {
			return e
		}
	}
	if u := strings.TrimSpace(os.Getenv("USER")); u != "" {
		if host, err := os.Hostname(); err == nil && host != "" {
			return u + "@" + host
		}
		return u
	}
	return "local"
}

// gitmetaDirForKey resolves the local git-meta store path for a repo key
// (<cache>/brain/<key>/gitmeta.git), mirroring brainDirForKey's key handling but
// rooted in the plugin CACHE tree (the store is a local, rebuildable sync
// staging area, not authoritative brain data).
func gitmetaDirForKey(env EntireEnv, key string) (string, error) {
	if first, _, _ := strings.Cut(key, "/"); first == workspaceDirName {
		return "", fmt.Errorf("repo key uses the reserved %q segment: %s", workspaceDirName, key)
	}
	dirs, err := resolvePluginDirs(env)
	if err != nil {
		return "", err
	}
	root := filepath.Join(dirs.Cache, "brain")
	if err := rejectBrainRootPathSymlinks(root, filepath.FromSlash(key)); err != nil {
		return "", err
	}
	return filepath.Join(root, filepath.FromSlash(key), "gitmeta.git"), nil
}

// refOrNone renders an empty ref as "(none)" for the no-change/empty case.
func refOrNone(ref string) string {
	if ref == "" {
		return "(none)"
	}
	return ref
}
