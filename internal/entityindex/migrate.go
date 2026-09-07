package entityindex

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ashtom/entire-brain/internal/factgitmeta"
	"github.com/ashtom/entire-brain/internal/factgitmeta/gitmeta"
)

// Migrate carries indexed commits into the selected parser namespace. Provider
// work runs without the write lock. Publication checks the captured metadata
// tip under the lock and aborts on drift; no older parser record is rewritten or
// tombstoned. Older peers continue to read and extend their original history.
func Migrate(ctx context.Context, runner Runner, store *factgitmeta.MetaStore, opts BuildOptions) (BuildResult, error) {
	return migrate(ctx, runner, store, opts, maxWalkCommits)
}

func migrate(ctx context.Context, runner Runner, store *factgitmeta.MetaStore, opts BuildOptions, maxPendingCommits int) (BuildResult, error) {
	result := BuildResult{}
	if ctx == nil {
		ctx = context.Background()
	}
	opts.migrating = true
	if runner == nil || store == nil {
		return result, fmt.Errorf("migration requires runner and store")
	}
	baseline, err := store.Tip()
	if err != nil {
		return result, err
	}
	state, err := store.State()
	if err != nil {
		return result, err
	}
	observed, err := store.Tip()
	if err != nil {
		return result, err
	}
	if observed != baseline {
		return result, fmt.Errorf("entity history changed while reading migration input; retry migration")
	}
	existing := map[stateKey]bool{}
	for _, v := range state.Strings {
		existing[stateKey{v.Target, v.Key}] = true
	}
	var commits []commitInfo
	seen := map[string]bool{}
	var windows []gitmeta.Mutation
	// Copy a coverage window only when the destination has none. All source
	// forward records are included below, so the copied range has no holes.
	for _, v := range state.Strings {
		key := anyRevisionKey(v.Key)
		if v.Target != projectTarget {
			continue
		}
		if _, ok := DecodeWindowBranch(key); !ok {
			continue
		}
		dest := RevisionKey(key, opts.IdentityRevision)
		if existing[stateKey{projectTarget, dest}] {
			continue
		}
		if _, ok := DecodeWindow(v.Value); ok {
			existing[stateKey{projectTarget, dest}] = true
			windows = append(windows, gitmeta.Mutation{Op: gitmeta.OpSetString, Target: projectTarget, Key: dest, Value: v.Value})
		}
	}

	for _, v := range state.Strings {
		if v.Target.Type != gitmeta.TargetCommit || anyRevisionKey(v.Key) != ForwardKey {
			continue
		}
		if seen[v.Target.Value] {
			continue
		}
		seen[v.Target.Value] = true
		if existing[stateKey{v.Target, RevisionKey(ForwardKey, opts.IdentityRevision)}] {
			result.Skipped++
			continue
		}
		var old Delta
		if err := json.Unmarshal([]byte(v.Value), &old); err != nil || old.SchemaVersion != SchemaVersion || old.Head != v.Target.Value {
			return result, fmt.Errorf("cannot migrate invalid delta for %s", v.Target.Value)
		}
		// Read metadata from Git, preserving the original source commit and its
		// checkpoint trailers; never synthesize replacement source commits.
		out, _, err := runner.Run(ctx, opts.RepoDir, "git", "log", "-1", gitLogFormat, v.Target.Value)
		if err != nil {
			return result, fmt.Errorf("read migration commit %s: %w", v.Target.Value, err)
		}
		parsed := parseCommitLog(out)
		if len(parsed) != 1 || parsed[0].SHA != v.Target.Value {
			return result, fmt.Errorf("missing migration commit %s", v.Target.Value)
		}
		commits = append(commits, parsed[0])
		if len(commits) > maxPendingCommits {
			return result, fmt.Errorf("migration exceeds %d pending commits; no records changed", maxPendingCommits)
		}
	}
	sort.Slice(commits, func(i, j int) bool {
		if commits[i].CommittedAt.Equal(commits[j].CommittedAt) {
			return commits[i].SHA < commits[j].SHA
		}
		return commits[i].CommittedAt.Before(commits[j].CommittedAt)
	})
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	b := builder{ctx: ctx, runner: runner, opts: opts, now: now, result: &result, store: store, indexed: map[string]bool{}, baseMS: now().UnixMilli()}
	for _, c := range commits {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		result.Scanned++
		ok, _ := b.indexCommit(c)
		if result.Entities > 200000 {
			return result, fmt.Errorf("migration exceeds 200000 entity changes; no records changed")
		}
		if !ok || len(result.Warnings) != 0 {
			return result, fmt.Errorf("migration aborted; no records changed: %s", strings.Join(result.Warnings, "; "))
		}
	}
	// A provider replacement during the run cannot stamp mixed output as current.
	revision, err := ProviderIdentity(ctx, runner, opts.RepoDir, opts.GraphBinary)
	if err != nil {
		return result, err
	}
	if revision != opts.IdentityRevision {
		return result, fmt.Errorf("provider identity changed during migration; no records changed")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	// Only publication takes the lock. Do not wait behind an unbounded writer.
	unlock, err := store.TryLock()
	if err != nil {
		return result, fmt.Errorf("migration publish lock unavailable; retry migration: %w", err)
	}
	defer unlock()
	result.MetaTip, err = store.UpdateLocked(b.mutCount+len(windows), func(current gitmeta.State) (gitmeta.State, error) {
		tip, err := store.Tip()
		if err != nil {
			return gitmeta.State{}, err
		}
		if tip != baseline {
			return gitmeta.State{}, fmt.Errorf("entity history changed during migration; no migration records published; retry migration")
		}
		if err := ctx.Err(); err != nil {
			return gitmeta.State{}, err
		}
		var muts []gitmeta.Mutation
		for _, g := range b.groups {
			muts = append(muts, g.muts...)
		}
		muts = append(muts, windows...)
		if len(muts) == 0 {
			return gitmeta.State{}, factgitmeta.ErrNoUpdate
		}
		return applyBatch(current, muts), nil
	})
	return result, err
}
