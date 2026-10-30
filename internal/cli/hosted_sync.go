package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/entireio/entire-brain/factmerge"
	"github.com/entireio/entire-brain/internal/apiurl"
	"github.com/entireio/entire-brain/internal/cli/command/connect"
	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
	"github.com/entireio/entire-brain/internal/factsync"
)

const (
	// hostedUpsertChunk stays well under brainstore's 5000-record batch cap
	// and its 32 MiB body cap.
	hostedUpsertChunk = 1000
	// hostedFeedPageSize is brainstore's page-size ceiling.
	hostedFeedPageSize = 100
	// maxHostedFeedPages bounds one pull so the never-fails path cannot spin
	// on a pathological feed. ponytail: a delta larger than 20k records
	// finishes on the next trigger — the cursor persists either way.
	maxHostedFeedPages = 200
	// hostedNoAccessBackoff is how long every background trigger skips a repo
	// after the server denied access to the binding (COR-2279). Long enough to
	// stop a stuck client from tripping the server's auth-failure monitor,
	// short enough that a restored grant resumes on its own; `entire brain
	// connect` resumes immediately.
	hostedNoAccessBackoff = 24 * time.Hour
)

// hostedSyncOutcome reports what one hostedFactsSyncAndPull run did.
type hostedSyncOutcome struct {
	Attempted     bool
	PullSucceeded bool
}

// hostedFactsSyncAndPull closes the hosted-memory loop for one branch on the
// per-fact wire (COR-2126): batch-upsert this machine's branch store to
// brainstore, pull the repo-wide delta feed into the repo-level cache, and
// rebuild the branch store as the pull filter's output plus this machine's
// own facts the feed has not seen. No CAS, no branch heads, no proposals —
// per-fact storage keeps conflicting facts side by side and resolution stays
// deferred to the async pipeline.
//
// It is a NEVER-FAILS step: every gated or soft failure is one stderr line and
// a nil return, because its callers (daemon tick, session-end hook, connect)
// must not fail on hosted outages. The error return exists for programming
// errors only.
//
// Gate order is fixed: (1) the local-only kill switch always wins; (2) a
// recorded opt-out (`entire brain disconnect`) stops sync until `connect`
// clears it; (3) the repo binding is the consent AND the connection state —
// only `entire brain connect` (or the future onboarding flow) writes it, so an
// absent binding means local-only, silently, every run. Background triggers
// never connect a repo. (4) a recorded no-access pause (COR-2279) skips the
// repo until it expires or connect clears it. (5) apiurl.Validate before any
// client exists. A fresh token is minted per call via the host CLI's own login
// and held only in memory.
//
// The outcome reports what actually happened: Attempted is whether the gates
// passed and the server was contacted (callers that log success must not
// report a gated no-op as a sync that ran); PullSucceeded is whether the pull
// half completed — the half that proves the exchange (the per-branch marker
// keys off it). Callers that promise a sync to the user (connect) key their
// report off PullSucceeded; background callers keep ignoring it.
func hostedFactsSyncAndPull(ctx context.Context, errW io.Writer, opts Options, repoDir string, storage repoStorage, branch string) (hostedSyncOutcome, error) {
	if base.BrainNoEgressMode() {
		return hostedSyncOutcome{}, nil
	}
	binding, present, err := brainstore.ReadHostedRepoBinding(storage.BrainDir)
	if err != nil {
		// NO unreadable binding may fall through to auto-connect: the file may
		// hide a recorded opt-out (privacy) or a custom target, and neither
		// survives a rewrite. Skip loudly; the remedy is an explicit
		// `entire brain connect`.
		fmt.Fprintf(errW, "hosted sync: skipped: %v\n", err)
		return hostedSyncOutcome{}, nil
	}
	if !present || binding.OptedOut {
		return hostedSyncOutcome{}, nil
	}
	// A recorded access denial pauses the repo (COR-2279): retrying the same
	// 403 every trigger only feeds the server's auth-failure monitor. Silent
	// like the opt-out gate; `entire brain status` reports the pause, and the
	// line printed when it was recorded named the remedy.
	if binding.NoAccessUntil.After(opts.Now().UTC()) {
		return hostedSyncOutcome{}, nil
	}
	baseURL, err := apiurl.Validate(binding.BaseURL)
	if err != nil {
		fmt.Fprintf(errW, "hosted sync: skipped: %v\n", err)
		return hostedSyncOutcome{}, nil
	}
	token, err := mintHostedToken(ctx, opts.Runner, repoDir, base.HostEntireBinary)
	if err != nil {
		fmt.Fprintf(errW, "hosted sync: skipped: %v\n", err)
		return hostedSyncOutcome{}, nil
	}
	srv := &factsync.HTTPServer{BaseURL: baseURL, Token: token, Version: opts.Version}

	rebound := false
	for {
		counts, skipped, pushErr := pushHostedFacts(ctx, storage, srv, binding.RepoID, branch)
		if skipped > 0 {
			fmt.Fprintf(errW, "hosted sync: branch %s: skipped %d unpublishable record(s) whose content id does not re-derive (kept locally, not uploaded)\n", branch, skipped)
		}
		pushed := pushErr == nil
		if pushErr != nil {
			// A push denial alone still degrades to pull-only: a read-only
			// grant is a real state (push 403, pull 200 — the COR-2030 staging
			// smoke hit exactly this) and the team's memories must keep
			// landing through it.
			fmt.Fprintf(errW, "hosted sync: branch %s publish failed (local brain unaffected): %v; pull-only\n", branch, pushErr)
		}
		kept, total, manifestErr, pullErr := pullHostedFacts(ctx, opts, repoDir, storage, srv, binding.RepoID, baseURL, token, branch)
		if errors.Is(pullErr, factsync.ErrNoAccess) {
			// The pull is the half every caller is granted on a healthy
			// binding, so its denial means the binding itself is stale: the
			// repo was deleted or this caller lost access (brainstore
			// deliberately serves both as 403). Retrying cannot help —
			// re-resolve placement once, and pause the repo when that does
			// not produce a different repo to talk to.
			if !rebound {
				if fresh, ok := rebindStaleHostedRepo(ctx, errW, opts, repoDir, storage.BrainDir, binding); ok {
					binding = fresh
					rebound = true
					continue
				}
			}
			pauseHostedSyncNoAccess(errW, opts, storage.BrainDir, binding)
			return hostedSyncOutcome{Attempted: true}, nil
		}
		if pullErr != nil {
			fmt.Fprintf(errW, "hosted sync: branch %s pull failed (local brain unaffected): %v\n", branch, pullErr)
			return hostedSyncOutcome{Attempted: true}, nil
		}
		if pushed {
			// The per-branch marker records that this branch completed a sync
			// against this target; the blind-spot note keys off its absence. It
			// needs BOTH halves: a pull-only run leaves it unwritten, and so does
			// a failed pull — the empty-store push "succeeds" without contacting
			// the server, so the pull is what proves the exchange happened.
			if err := writeHostedFactsBinding(storage.BrainDir, branch, binding.RepoID, baseURL); err != nil {
				fmt.Fprintf(errW, "hosted sync: record hosted target: %v\n", err)
			}
		}
		if manifestErr != nil {
			fmt.Fprintf(errW, "hosted sync: facts updated, but the source manifest could not be refreshed: %v\n", manifestErr)
		}
		if !binding.NoAccessUntil.IsZero() {
			// This run started past an expired pause and the pull succeeded:
			// access is back, so retire the pause instead of leaving status
			// reporting it forever. Guarded like the pause write — a binding
			// another writer replaced meanwhile is not touched.
			if err := clearHostedSyncNoAccess(storage.BrainDir, binding); err != nil {
				fmt.Fprintf(errW, "hosted sync: clear expired no-access pause: %v\n", err)
			}
		}
		fmt.Fprintf(errW, "hosted sync: branch %s pushed added=%d changed=%d unchanged=%d; feed facts=%d kept=%d\n",
			branch, counts.Added, counts.Changed, counts.Unchanged, total, kept)
		return hostedSyncOutcome{Attempted: true, PullSucceeded: true}, nil
	}
}

// rebindStaleHostedRepo handles the deleted-and-reonboarded case behind a
// pull denial: if the origin now resolves to a DIFFERENT repo id in the SAME
// cell, the old binding points at a dead repo and the consent it records
// carries over to its replacement. The cell must match so an explicit
// non-default binding (--repo-id/--api-url to another cell or staging) is
// never silently retargeted to the resolved placement. The write re-checks
// the binding under the lock: a concurrent disconnect or connect wins.
func rebindStaleHostedRepo(ctx context.Context, errW io.Writer, opts Options, repoDir, brainDir string, stale brainstore.HostedRepoBinding) (brainstore.HostedRepoBinding, bool) {
	fresh, err := connect.ResolveBinding(ctx, opts.Runner, repoDir)
	if err != nil || fresh.RepoID == "" || fresh.RepoID == stale.RepoID || fresh.BaseURL != stale.BaseURL {
		return brainstore.HostedRepoBinding{}, false
	}
	written := false
	err = brainstore.MutateHostedRepoBinding(brainDir, func(current brainstore.HostedRepoBinding) (brainstore.HostedRepoBinding, bool) {
		if current.OptedOut || current.RepoID != stale.RepoID || current.BaseURL != stale.BaseURL {
			return current, false
		}
		written = true
		return fresh, true
	})
	if err != nil || !written {
		return brainstore.HostedRepoBinding{}, false
	}
	fmt.Fprintf(errW, "hosted sync: repo was re-onboarded on Entire; rebound %s -> %s\n", stale.RepoID, fresh.RepoID)
	return fresh, true
}

// clearHostedSyncNoAccess retires a pause whose repo answered a pull again.
func clearHostedSyncNoAccess(brainDir string, stale brainstore.HostedRepoBinding) error {
	return brainstore.MutateHostedRepoBinding(brainDir, func(current brainstore.HostedRepoBinding) (brainstore.HostedRepoBinding, bool) {
		if current.OptedOut || current.RepoID != stale.RepoID || current.BaseURL != stale.BaseURL || current.NoAccessUntil.IsZero() {
			return current, false
		}
		current.NoAccessUntil = time.Time{}
		return current, true
	})
}

// pauseHostedSyncNoAccess records the no-access pause next to the binding and
// says so once. The write re-checks the binding under the lock so a
// concurrent connect or disconnect is never stamped with a stale denial. The
// "paused" line prints only when the pause was actually recorded: a declined
// write means another writer owns the binding now (its state speaks for
// itself), and a failed write means the next trigger will try again.
func pauseHostedSyncNoAccess(errW io.Writer, opts Options, brainDir string, stale brainstore.HostedRepoBinding) {
	until := opts.Now().UTC().Add(hostedNoAccessBackoff)
	written := false
	err := brainstore.MutateHostedRepoBinding(brainDir, func(current brainstore.HostedRepoBinding) (brainstore.HostedRepoBinding, bool) {
		if current.OptedOut || current.RepoID != stale.RepoID || current.BaseURL != stale.BaseURL {
			return current, false
		}
		current.NoAccessUntil = until
		written = true
		return current, true
	})
	if err != nil {
		fmt.Fprintf(errW, "hosted sync: no access to repo %s on Entire, but recording the pause failed: %v\n", stale.RepoID, err)
		return
	}
	if !written {
		return
	}
	fmt.Fprintf(errW, "hosted sync: no access to repo %s on Entire (deleted, or this account lost access); sync paused until %s — run '%s' once access is granted\n",
		stale.RepoID, until.Format(time.RFC3339), base.BrainInvocation(os.LookupEnv, "connect"))
}

// pushHostedFacts batch-upserts the branch store. Idempotent server-side, so
// every trigger can fire it without coordination. It returns the upsert counts
// and how many unverifiable records stayed home; a non-nil error means the
// publish did not complete and the branch must not be marked as synced. The
// caller owns presentation: background sync degrades to pull-only with one
// stderr line, manual sync fails the command (COR-2202 honesty).
func pushHostedFacts(ctx context.Context, storage repoStorage, srv *factsync.HTTPServer, repoID, branch string) (factsync.UpsertCounts, int, error) {
	facts, err := factstore.Load(storage.BrainDir, branch)
	if err != nil {
		return factsync.UpsertCounts{}, 0, err
	}
	if len(facts) == 0 {
		// Nothing to publish counts as a successful (empty) publish; the
		// server rejects an empty body, so don't send one.
		return factsync.UpsertCounts{}, 0, nil
	}
	// Redact local-only provenance coordinates before these facts leave, and
	// verify this member's own facts BEFORE publishing them — the server
	// rejects a whole batch for one malformed record, so the check runs here,
	// per record: one corrupted line must not hold the rest of the store
	// hostage. Unverifiable records stay home with a note; everything else
	// uploads.
	egress := factsync.SanitizeForEgress(facts)
	publishable := egress[:0]
	skipped := 0
	for _, rec := range egress {
		if err := factmerge.VerifyIdentity(rec); err != nil {
			skipped++
			continue
		}
		publishable = append(publishable, rec)
	}
	egress = publishable
	if len(egress) == 0 {
		// Everything was unpublishable: nothing was shared, so this is a
		// failed publish, not a vacuous success.
		return factsync.UpsertCounts{}, skipped, errors.New("no publishable records")
	}
	var total factsync.UpsertCounts
	for start := 0; start < len(egress); start += hostedUpsertChunk {
		end := min(start+hostedUpsertChunk, len(egress))
		var buf bytes.Buffer
		if err := factmerge.WriteNDJSON(&buf, egress[start:end]); err != nil {
			return total, skipped, err
		}
		counts, err := srv.UpsertFacts(ctx, repoID, buf.Bytes())
		if err != nil {
			return total, skipped, err
		}
		total.Added += counts.Added
		total.Changed += counts.Changed
		total.Unchanged += counts.Unchanged
	}
	return total, skipped, nil
}

// pullHostedFacts fetches the repo delta feed since the stored cursor into
// the repo-level cache, filters the WHOLE cache by local visibility
// (keepHostedFact), and rebuilds the branch store from the result. Returns
// (kept, total cached) for the caller's summary line.
//
// Everything runs under the brain write lock: the cache and the branch store
// must move together, and concurrent triggers (tick, hook, push hint) must
// not interleave their rebuilds.
func pullHostedFacts(ctx context.Context, opts Options, repoDir string, storage repoStorage, srv *factsync.HTTPServer, repoID, baseURL, token, branch string) (kept, total int, manifestErr, err error) {
	feedDir, err := hostedFeedDirForKey(opts.Env, storage.Key)
	if err != nil {
		return 0, 0, nil, err
	}
	err = brainstore.WithBrainWriteLock(storage.BrainDir, func() error {
		state, items, err := loadHostedFeedCache(feedDir)
		if err != nil {
			return err
		}
		// A different hosted target invalidates everything: cached facts from
		// another repo (or another cell) must never rebuild this branch store.
		if state.RepoID != repoID || state.BaseURL != baseURL {
			state = hostedFeedState{RepoID: repoID, BaseURL: baseURL}
			items = make(map[string]factsync.FeedItem)
		}
		cursor := state.Cursor
		cursorReset := false
		for page := 0; page < maxHostedFeedPages; page++ {
			feedPage, err := srv.FactsFeedPage(ctx, repoID, cursor, hostedFeedPageSize)
			if errors.Is(err, factsync.ErrInvalidPageToken) && !cursorReset && cursor != "" {
				// The server's cursor epoch moved: the stored position is
				// meaningless, so refetch from the start. The cache rebuild is
				// idempotent (keyed by fact id).
				cursorReset = true
				cursor = ""
				items = make(map[string]factsync.FeedItem)
				continue
			}
			if err != nil {
				return err
			}
			if len(feedPage.Items) == 0 {
				break
			}
			for _, item := range feedPage.Items {
				if item.Record.ID != "" {
					items[item.Record.ID] = item
				}
			}
			if feedPage.NextPageToken == nil {
				break
			}
			cursor = *feedPage.NextPageToken
		}

		member := jwtSubClaim(token)
		reachable := newCommitReachability(ctx, opts.Runner, repoDir)
		keptIDs := make(map[string]bool)
		var keptRecords []factRecord
		for id, item := range items {
			if !keepHostedFact(item, member, reachable) {
				continue
			}
			rec := item.Record
			// Server attribution is the only author source: it overwrites
			// whatever the wire record carried (an old or hostile peer's value
			// must not survive).
			rec.Author = item.CreatedByHandle
			keptRecords = append(keptRecords, rec)
			keptIDs[id] = true
		}
		// Same fail-closed rule as the old head pull: a record whose content id
		// does not re-derive is a forgery, and nothing from this feed — cache
		// and cursor included — may land until the server stops serving it.
		if err := factmerge.VerifyIdentities(keptRecords); err != nil {
			return fmt.Errorf("refused untrustworthy feed: %w", err)
		}
		sort.Slice(keptRecords, func(i, j int) bool { return keptRecords[i].ID < keptRecords[j].ID })

		if err := writeHostedFeedCache(feedDir, hostedFeedState{RepoID: repoID, BaseURL: baseURL, Cursor: cursor}, items); err != nil {
			return err
		}

		existing, err := factstore.Load(storage.BrainDir, branch)
		if err != nil {
			return err
		}
		for _, fact := range keptRecords {
			// upsertFact never moves a local status, so a locally retracted
			// fact stays retracted whatever the feed says. The feed is
			// authoritative for RETIREMENTS, though: lifecycle moves one way —
			// active to retired, never a revival.
			existing = upsertFact(existing, fact)
			if fact.Status == factStatusActive {
				continue
			}
			if i := indexOfFact(existing, fact.ID); i >= 0 && existing[i].Status == factStatusActive {
				existing[i].Status = fact.Status
				existing[i].SupersededBy = fact.SupersededBy
				existing[i].UpdatedAt = opts.Now().UTC()
			}
		}
		// The branch store is the filter output plus this machine's own facts
		// the feed has not seen (unsynced or push-denied). A record the feed
		// knows but the filter rejected is another branch's — or a recycled
		// name's — fact: drop it. This is what replaces rename/recycle
		// detection (lineage.json is cancelled). Three stand-downs guard
		// against evicting this machine's own knowledge when the feed's copy
		// fails the filter (first-writer attribution can belong to a teammate
		// who pushed the same statement first, carrying only THEIR anchors):
		// a transcript anchor is proof the fact was distilled here
		// (SanitizeForEgress strips it on push, so a pulled copy never
		// carries one); the local copy's OWN anchors get the same keep filter
		// (judged memberless — being in this store is not proof of
		// authorship), so a locally reachable commit or an anchor-less pool
		// fact survives; and with no member identity the filter cannot
		// recognize this member's facts at all, so removal is disabled
		// rather than evicting them wholesale.
		rebuilt := existing[:0]
		for _, rec := range existing {
			if _, inFeed := items[rec.ID]; member != "" && inFeed && !keptIDs[rec.ID] &&
				!hasLocalProvenance(rec) && !keepHostedFact(factsync.FeedItem{Record: rec}, "", reachable) {
				continue
			}
			rebuilt = append(rebuilt, rec)
		}
		if err := factstore.Write(storage.BrainDir, branch, rebuilt); err != nil {
			return err
		}
		// A manifest failure reports alongside the successful pull instead of
		// masking it.
		manifestErr = updateFactSourceManifestLocked(storage.BrainDir, opts.Now().UTC())
		kept, total = len(keptRecords), len(items)
		return nil
	})
	return kept, total, manifestErr, err
}

// hostedSyncBranch resolves the branch a hosted sync step should cover: the
// repo's current branch, else the default branch.
func hostedSyncBranch(ctx context.Context, runner CommandRunner, repoDir string) string {
	if current, err := base.GitScalar(ctx, runner, repoDir, "branch", "--show-current"); err == nil {
		if branch := strings.TrimSpace(current); branch != "" {
			return branch
		}
	}
	return distillDefaultBranch
}
