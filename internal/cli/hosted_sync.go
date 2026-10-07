package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/entireio/entire-brain/factmerge"
	"github.com/entireio/entire-brain/internal/apiurl"
	"github.com/entireio/entire-brain/internal/factsync"
)

// hostEntireBinary is the host CLI the plugin shells out to; PATH comes from
// the caller's environment (the daemon plist bakes PATH at install time).
const hostEntireBinary = "entire"

// hostedFactsSyncAndPull closes the hosted-memory loop for one branch: publish
// the branch's local facts through factsync.Sync (read-merge-CAS), record the
// per-branch hosted target, then merge the settled head — with server-stamped
// authors — back into the local branch store so teammates' facts become
// first-class in brief/recall.
//
// It is a NEVER-FAILS step: every gated or soft failure is one stderr line and
// a nil return, because its callers (daemon tick, session-end hook, connect)
// must not fail on hosted outages. The error return exists for programming
// errors only.
//
// Gate order is fixed: (1) the local-only kill switch always wins; (2) the
// repo binding is the connection state — absent, each run tries to resolve it
// through the host CLI and falls back to local-only, silently, until that
// succeeds; (3) apiurl.Validate before any client exists. A fresh token is
// minted per call via the host CLI's own login and held only in memory.
func hostedFactsSyncAndPull(ctx context.Context, errW io.Writer, opts Options, repoDir string, storage repoStorage, branch string) error {
	if brainNoEgressMode() {
		return nil
	}
	binding, present, err := readHostedRepoBinding(storage.BrainDir)
	if err != nil {
		fmt.Fprintf(errW, "hosted sync: skipped: %v\n", err)
		return nil
	}
	if !present {
		// Auto-connect: placement comes from the host CLI (whereami), which
		// fails for a logged-out user, an unregistered repo, or a CLI without
		// the command — all of which mean "stay local and retry next run".
		// ponytail: an unresolvable repo re-runs whereami on every throttled
		// sync attempt; cache a negative result if that ever gets expensive.
		resolved, whereamiErr := resolveWhereami(ctx, opts.Runner, repoDir)
		if whereamiErr != nil {
			return nil
		}
		binding, err = bindHostedRepo(storage.BrainDir, resolved.RepoID, resolved.APIURL)
		if err != nil {
			fmt.Fprintf(errW, "hosted sync: auto-connect skipped: %v\n", err)
			return nil
		}
	}
	baseURL, err := apiurl.Validate(binding.BaseURL)
	if err != nil {
		fmt.Fprintf(errW, "hosted sync: skipped: %v\n", err)
		return nil
	}
	token, err := mintHostedToken(ctx, opts.Runner, repoDir, hostEntireBinary)
	if err != nil {
		fmt.Fprintf(errW, "hosted sync: skipped: %v\n", err)
		return nil
	}

	facts, err := loadFacts(storage.BrainDir, branch)
	if err != nil {
		fmt.Fprintf(errW, "hosted sync: branch %s: %v\n", branch, err)
		return nil
	}
	srv := &factsync.HTTPServer{BaseURL: baseURL, Token: token}
	memberID := resolveSyncMemberID(ctx, opts.Runner, repoDir, "")
	res, err := factsync.Sync(ctx, srv, binding.RepoID, branch, memberID, facts, opts.Now().UTC())
	if err != nil {
		fmt.Fprintf(errW, "hosted sync: branch %s failed (local brain unaffected): %v\n", branch, err)
		return nil
	}
	// The per-branch marker records that this branch has synced against this
	// target; the blind-spot note keys off its absence.
	if err := writeHostedFactsBinding(storage.BrainDir, branch, binding.RepoID, baseURL); err != nil {
		fmt.Fprintf(errW, "hosted sync: record hosted target: %v\n", err)
	}
	// Promote raises a keep-both conflict proposal only on FIRST contact (both
	// facts are in the head afterwards), so dropping res.Proposals here would
	// lose the review signal forever. Persist to the local review queue like
	// the manual sync; v1 cuts the hosted queue endpoints, so no publish.
	if err := persistFactsSyncProposals(storage.BrainDir, branch, res.Proposals); err != nil {
		fmt.Fprintf(errW, "hosted sync: persist raised conflicts: %v\n", err)
	}

	// Best-effort attribution: the authors map is additive on the GET response,
	// so a second read is the only way to fetch it. Failure costs display
	// attribution, never the pull.
	var authors map[string]string
	if _, _, a, found, authorsErr := srv.CurrentWithAuthors(ctx, binding.RepoID, branch); authorsErr == nil && found {
		authors = a
	}
	// The server map is the only attribution source: it overwrites whatever
	// the blob carried (SanitizeForEgress clears Author on upload, but an old
	// or hostile peer's value must not survive either). Records absent from
	// the map arrive unattributed, and Upsert keeps any handle already stored.
	merged := append([]factmerge.Record(nil), res.Facts...)
	for i := range merged {
		merged[i].Author = authors[merged[i].ID]
	}

	// The import write composition: everything under one write lock, manifest
	// refresh included; a manifest failure reports alongside the successful
	// pull instead of masking it. upsertFact never moves a local status, so a
	// locally retracted fact stays retracted whatever the head says.
	var manifestErr error
	if err := withBrainWriteLock(storage.BrainDir, func() error {
		existing, loadErr := loadFacts(storage.BrainDir, branch)
		if loadErr != nil {
			return loadErr
		}
		for _, fact := range merged {
			existing = upsertFact(existing, fact)
		}
		if err := writeFacts(storage.BrainDir, branch, existing); err != nil {
			return err
		}
		manifestErr = updateFactSourceManifestLocked(storage.BrainDir, opts.Now().UTC())
		return nil
	}); err != nil {
		fmt.Fprintf(errW, "hosted sync: branch %s pull failed (local store unchanged): %v\n", branch, err)
		return nil
	}
	if manifestErr != nil {
		fmt.Fprintf(errW, "hosted sync: facts updated, but the source manifest could not be refreshed: %v\n", manifestErr)
	}
	fmt.Fprintf(errW, "hosted sync: branch %s published=%v attempts=%d facts=%d\n", branch, res.Published, res.Attempts, len(res.Facts))
	return nil
}

// hostedSyncBranch resolves the branch a hosted sync step should cover: the
// repo's current branch, else the default branch.
func hostedSyncBranch(ctx context.Context, runner CommandRunner, repoDir string) string {
	if current, err := gitScalar(ctx, runner, repoDir, "branch", "--show-current"); err == nil {
		if branch := strings.TrimSpace(current); branch != "" {
			return branch
		}
	}
	return distillDefaultBranch
}
