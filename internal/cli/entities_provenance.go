package cli

import (
	"context"
	"path"
	"strings"
	"time"
)

// entities_provenance.go sharpens a distilled fact's anchor.
//
// Before the entity index existed, every fact distilled from a session was
// anchored to that session's LATEST checkpoint — the coarsest possible answer:
// a session with forty checkpoints attributed all forty turns' worth of facts
// to the last one. With the index, a fact whose locus names a code entity can
// be anchored to the checkpoint that actually changed THAT entity.
//
// Four properties this must keep, in order:
//
//  1. Non-breaking. No index, no match, or an ambiguous match falls back to the
//     exact prior behavior (session.LatestCheckpoint). Distill must never fail
//     or slow meaningfully because of this.
//  2. In-session AND on-branch. The sharpened checkpoint must belong to the
//     SAME SESSION the fact was distilled from, and its commit must be
//     reachable from that session's branch. The index is repo-wide, so an
//     unscoped "newest checkpoint that touched this entity" happily pairs one
//     session's id with another session's commit and checkpoint — a provenance
//     triple that never existed and that verification would then have to either
//     reject or launder. Session scoping alone is not enough: history rewriting
//     puts one checkpoint's trailer on two commits, so the newest commit
//     bearing a session's checkpoint can be another branch's copy — live,
//     referenced, and off this fact's branch.
//  3. Consistent. Commit and CheckpointID are overridden together or not at
//     all; a commit from the index paired with a checkpoint from the fallback
//     would be a provenance pair that never existed.
//  4. Honest. A generic locus token ("config", "handler") that names many
//     unrelated entities is NOT provenance; it is dropped rather than used to
//     attach a fact to an arbitrary checkpoint.

const (
	// entityProvenanceMaxKeysPerName drops a symbol name shared by more than
	// this many distinct entities: at that point the name identifies nothing.
	entityProvenanceMaxKeysPerName = 5
	// entityProvenanceMaxLocus bounds per-fact work so distill's cost stays
	// flat regardless of how chatty a fact's locus is.
	entityProvenanceMaxLocus = 16
)

// entityProvenanceResolver answers "which checkpoint of THIS session changed
// this identifier" from the derived entity-index cache. It is built once per
// distill run and is read-only afterwards.
type entityProvenanceResolver struct {
	byName map[string]*entityProvenanceEntry
	byPath map[string]*entityProvenanceEntry
	// branchBySession is the branch each session was exported on, so a fact can
	// refuse a candidate from a same-named session on another branch.
	branchBySession map[string]string
}

// entityProvenanceEntry is what one token resolves to: the newest candidate PER
// SESSION, plus the distinct things that produced the token (the ambiguity
// guard — entity keys for a name, file paths for a path).
type entityProvenanceEntry struct {
	bySession map[string]entityProvenanceCandidate
	distinct  map[string]struct{}
}

// entityProvenanceCandidate is one commit/checkpoint pair that touched a token.
type entityProvenanceCandidate struct {
	commit       string
	checkpointID string
	at           time.Time
}

// newEntityProvenanceResolver builds the resolver from the entity index. It
// returns nil (never an error) when the index is absent or empty: distill's
// existing anchoring is then used unchanged.
func newEntityProvenanceResolver(ctx context.Context, opts Options, repoDir string) *entityProvenanceResolver {
	view, err := loadEntityIndexView(ctx, opts, repoDir)
	if err != nil || view == nil || len(view.cache.Entries) == 0 {
		return nil
	}
	return buildEntityProvenanceResolver(
		view.cache,
		sessionBranchByID(ctx, opts, repoDir),
		branchReachabilityLookup(ctx, opts, repoDir),
	)
}

// provenanceBranchReachability answers "which commits are reachable from this
// branch", or nil when that cannot be determined. It is the same walk
// `entities history` already filters its answers through
// (branchReachableCommits); provenance needs it for the same reason.
type provenanceBranchReachability func(branch string) map[string]struct{}

// branchReachabilityLookup memoizes branchReachableCommits per branch, so one
// distill run pays for one rev-list per branch it distills rather than one per
// candidate.
func branchReachabilityLookup(ctx context.Context, opts Options, repoDir string) provenanceBranchReachability {
	cache := map[string]map[string]struct{}{}
	return func(branch string) map[string]struct{} {
		branch = strings.TrimSpace(branch)
		if branch == "" {
			return nil
		}
		if set, ok := cache[branch]; ok {
			return set
		}
		set := branchReachableCommits(ctx, opts.Runner, repoDir, branch)
		if len(set) >= entityBranchRevWalkMax {
			// The walk hit its bound, so the answer is the branch's NEWEST
			// commits, not its commits. Reporting that as a reachability set
			// would call every older indexed commit off-branch and silently
			// stop sharpening the older half of a deep repository. A truncated
			// walk is unknown reachability, which refuses nothing.
			set = nil
		}
		cache[branch] = set
		return set
	}
}

// provenanceCommitOnBranch reports whether a candidate commit may anchor work
// distilled on branch.
//
// UNKNOWN reachability is not a refusal. A branch that is gone locally, a walk
// git refused, or a history deeper than the walk bound all return nil, and
// treating that as "nothing is on this branch" would silently downgrade every
// fact in the run to the coarse anchor. A KNOWN set that does not contain the
// commit is a refusal: the commit is real, but it is not on this branch.
func provenanceCommitOnBranch(reachable provenanceBranchReachability, branch, commit string) bool {
	if reachable == nil || strings.TrimSpace(branch) == "" || strings.TrimSpace(commit) == "" {
		return true
	}
	set := reachable(branch)
	if set == nil {
		return true
	}
	_, ok := set[commit]
	return ok
}

// buildEntityProvenanceResolver folds a derived cache into the resolver. It is
// the pure half of newEntityProvenanceResolver, so the resolution order can be
// tested against a cache built by hand.
func buildEntityProvenanceResolver(cache entityIndexCache, branchBySession map[string]string, reachable provenanceBranchReachability) *entityProvenanceResolver {
	resolver := &entityProvenanceResolver{
		byName:          map[string]*entityProvenanceEntry{},
		byPath:          map[string]*entityProvenanceEntry{},
		branchBySession: branchBySession,
	}
	// The iteration below is over a Go map, whose order is randomized per run,
	// and one token routinely collects candidates from several entity keys. That
	// is safe ONLY because betterProvenance is a TOTAL order: every (token,
	// session) cell keeps the same winner no matter what order the cache is
	// walked in. Nothing here may fall back to "first seen".
	for key, occurrences := range cache.Entries {
		entityPath, _, name, ok := splitEntityIndexKey(key)
		if !ok {
			continue
		}
		for _, occ := range occurrences {
			// Only checkpoint-bearing occurrences can sharpen an anchor: the
			// whole point is to name a checkpoint, and a commit made outside a
			// captured session has none. More than one checkpoint on a commit
			// leaves the (checkpoint, session) pairing ambiguous, and a guessed
			// pairing is exactly the invented anchor this must not produce.
			if len(occ.CheckpointIDs) != 1 || len(occ.SessionIDs) == 0 {
				continue
			}
			at := time.Time{}
			if occ.CommittedAt != nil {
				at = *occ.CommittedAt
			}
			candidate := entityProvenanceCandidate{
				commit:       occ.Commit,
				checkpointID: occ.CheckpointIDs[0],
				at:           at,
			}
			for _, sessionID := range occ.SessionIDs {
				if strings.TrimSpace(sessionID) == "" {
					continue
				}
				// The reverse index is PROJECT-scoped: it lists every commit
				// that touched the entity on every branch, including cherry-
				// picked and rebase-copied commits carrying the SAME
				// Entire-Checkpoint trailer. One checkpoint id therefore
				// resolves to more than one commit, and the newest committer
				// date is routinely the copy that landed on another branch. A
				// commit that is not on the session's branch is not this fact's
				// evidence: `git log <branch>` will never show it, and verify
				// passes it — verifyCommitUncached (verify_cmd.go:392) accepts
				// reachability from ANY local ref, so a live commit on another
				// branch verifies clean. (A commit no ref contains is a
				// different case: verify does catch that one as orphaned. This
				// guard still helps there, by keeping the honest coarse anchor
				// instead of a dangling one.) Drop it here, so the winner is
				// the newest ON-BRANCH candidate rather than the newest
				// candidate anywhere.
				if !provenanceCommitOnBranch(reachable, branchBySession[sessionID], candidate.commit) {
					continue
				}
				if name != "" {
					resolver.observe(resolver.byName, strings.ToLower(name), key, sessionID, candidate)
				}
				if entityPath != "" {
					lowerPath := strings.ToLower(entityPath)
					// A path token is ambiguous when it names many FILES, not
					// many entities: one file legitimately holds hundreds of
					// symbols, while a bare basename ("utils.go") can name a
					// dozen unrelated files.
					resolver.observe(resolver.byPath, lowerPath, lowerPath, sessionID, candidate)
					if base := path.Base(lowerPath); base != "" && base != lowerPath {
						resolver.observe(resolver.byPath, base, lowerPath, sessionID, candidate)
					}
				}
			}
		}
	}
	if len(resolver.byName) == 0 && len(resolver.byPath) == 0 {
		return nil
	}
	return resolver
}

// observe records a candidate under a token for one session, keeping that
// session's newest occurrence and counting how ambiguous the token is.
func (r *entityProvenanceResolver) observe(into map[string]*entityProvenanceEntry, token, distinct, sessionID string, candidate entityProvenanceCandidate) {
	entry, ok := into[token]
	if !ok {
		entry = &entityProvenanceEntry{
			bySession: map[string]entityProvenanceCandidate{},
			distinct:  map[string]struct{}{},
		}
		into[token] = entry
	}
	entry.distinct[distinct] = struct{}{}
	current, seen := entry.bySession[sessionID]
	if !seen || betterProvenance(current, candidate) {
		entry.bySession[sessionID] = candidate
	}
}

// betterProvenance reports whether candidate must displace current under a
// TOTAL order over candidates:
//
//  1. a candidate that names a commit beats one that does not (an anchor
//     without a commit cannot sharpen anything);
//  2. then the newer committer date;
//  3. then the larger commit sha;
//  4. then the larger checkpoint id.
//
// Tiers 3 and 4 exist because `%cI` resolves to one SECOND: two commits made in
// the same second are a routine tie, and breaking it by arrival order made the
// anchor depend on Go's randomized map iteration and on the order the index
// happened to be written in — identical inputs, different provenance, run to
// run. Two candidates that tie all four ways are the same candidate.
func betterProvenance(current, candidate entityProvenanceCandidate) bool {
	if (current.commit == "") != (candidate.commit == "") {
		return current.commit == ""
	}
	if !candidate.at.Equal(current.at) {
		return candidate.at.After(current.at)
	}
	if candidate.commit != current.commit {
		return candidate.commit > current.commit
	}
	return candidate.checkpointID > current.checkpointID
}

// candidateFor returns the token's candidate for one session, honoring the
// ambiguity guard.
func (e *entityProvenanceEntry) candidateFor(sessionID string) (entityProvenanceCandidate, bool) {
	if e == nil || len(e.distinct) > entityProvenanceMaxKeysPerName {
		return entityProvenanceCandidate{}, false
	}
	candidate, ok := e.bySession[sessionID]
	return candidate, ok
}

// anchorFor returns a sharpened anchor for a record, and ok=false when the
// index cannot improve on the caller's fallback.
func (r *entityProvenanceResolver) anchorFor(record factRecord, fallback factAnchor) (factAnchor, bool) {
	if r == nil {
		return fallback, false
	}
	// Scope: the fact's OWN session. Without a session there is nothing to
	// scope to, so the coarse anchor stands.
	sessionID := strings.TrimSpace(fallback.SessionID)
	if sessionID == "" {
		return fallback, false
	}
	if branch := strings.TrimSpace(record.Branch); branch != "" {
		if sessionBranch, known := r.branchBySession[sessionID]; known && sessionBranch != "" && sessionBranch != branch {
			return fallback, false
		}
	}
	locus := factLocusOf(record)
	if len(locus) > entityProvenanceMaxLocus {
		locus = locus[:entityProvenanceMaxLocus]
	}
	var best *entityProvenanceCandidate
	for _, token := range locus {
		token = strings.ToLower(strings.TrimSpace(token))
		if token == "" {
			continue
		}
		if candidate, ok := r.byName[token].candidateFor(sessionID); ok {
			best = newerProvenance(best, &candidate)
			continue
		}
		if candidate, ok := r.byPath[token].candidateFor(sessionID); ok {
			best = newerProvenance(best, &candidate)
		}
	}
	if best == nil || best.checkpointID == "" {
		return fallback, false
	}
	anchor := fallback
	anchor.Commit = best.commit
	anchor.CheckpointID = best.checkpointID
	return anchor, true
}

// newerProvenance folds one token's candidate into the running best under the
// same total order observe uses, so which of two equally-timed loci won is not
// decided by the order the locus happened to list them in.
func newerProvenance(current, candidate *entityProvenanceCandidate) *entityProvenanceCandidate {
	if current == nil {
		return candidate
	}
	if candidate == nil {
		return current
	}
	if betterProvenance(*current, *candidate) {
		return candidate
	}
	return current
}

// sessionBranchByID maps a session id to the branch the brain exported it on.
func sessionBranchByID(ctx context.Context, opts Options, repoDir string) map[string]string {
	out := map[string]string{}
	storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return out
	}
	manifest, err := loadBrainManifest(storage.BrainDir)
	if err != nil || manifest == nil {
		return out
	}
	sessions := manifest.Sessions
	if manifest.Sources != nil && manifest.Sources.Sessions != nil && len(manifest.Sources.Sessions.Sessions) > 0 {
		sessions = manifest.Sources.Sessions.Sessions
	}
	for _, session := range sessions {
		id := strings.TrimSpace(session.SessionID)
		if id == "" {
			continue
		}
		if branch := strings.TrimSpace(resolveDistillBranch(manifest, session)); branch != "" {
			out[id] = branch
		}
	}
	return out
}

// applyEntityProvenance rewrites each record's single distill-time anchor with
// the entity-resolved one where the index has an answer. Records the index
// cannot place keep the anchor distill built, byte for byte.
func applyEntityProvenance(records []factRecord, resolver *entityProvenanceResolver) []factRecord {
	if resolver == nil || len(records) == 0 {
		return records
	}
	for i := range records {
		if len(records[i].Provenance) != 1 {
			// Distill always emits exactly one anchor per new record. Anything
			// else is a merged/authored record this pass must not rewrite.
			continue
		}
		anchor, ok := resolver.anchorFor(records[i], records[i].Provenance[0])
		if !ok {
			continue
		}
		records[i].Provenance[0] = anchor
	}
	return records
}

// splitEntityIndexKey decomposes a "<path>#<kind>#<name>" index key. It is a
// thin wrapper so this file does not need the entityindex import for one call.
func splitEntityIndexKey(key string) (entityPath, kind, name string, ok bool) {
	idx := strings.LastIndex(key, "#")
	if idx < 0 {
		return "", "", "", false
	}
	name = key[idx+1:]
	rest := key[:idx]
	idx = strings.LastIndex(rest, "#")
	if idx < 0 {
		return "", "", "", false
	}
	kind = rest[idx+1:]
	entityPath = rest[:idx]
	return entityPath, kind, name, true
}
