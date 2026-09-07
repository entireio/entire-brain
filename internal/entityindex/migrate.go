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

// Migrate recomputes exactly the already indexed commits, across all branches.
// No history window is widened and no authored memory is removed. All provider
// work must succeed before one atomic git-meta update replaces the derived keys.
func Migrate(ctx context.Context, runner Runner, store *factgitmeta.MetaStore, opts BuildOptions) (BuildResult, error) {
	result := BuildResult{}
	opts.migrating = true
	if runner == nil || store == nil {
		return result, fmt.Errorf("migration requires runner and store")
	}
	unlock, err := store.Lock()
	if err != nil {
		return result, err
	}
	defer unlock()
	state, err := store.State()
	if err != nil {
		return result, err
	}
	var commits []commitInfo
	for _, v := range state.Strings {
		if v.Target.Type != gitmeta.TargetCommit || v.Key != ForwardKey {
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
		if len(commits) > maxWalkCommits {
			return result, fmt.Errorf("migration exceeds %d stored commits; no records changed", maxWalkCommits)
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
	result.MetaTip, err = store.UpdateLocked(b.mutCount+1, func(current gitmeta.State) (gitmeta.State, error) {
		next := clearDerivedEntityKeys(current)
		var muts []gitmeta.Mutation
		for _, g := range b.groups {
			muts = append(muts, g.muts...)
		}
		muts = append(muts, gitmeta.Mutation{Op: gitmeta.OpSetString, Target: projectTarget, Key: IdentityKey, Value: opts.IdentityRevision})
		return applyBatch(next, muts), nil
	})
	return result, err
}

// Retain forward documents until overwritten, windows, and every unrelated
// namespace. Tombstone superseded reverse/alias keys so sync cannot revive them.
func clearDerivedEntityKeys(st gitmeta.State) gitmeta.State {
	out := gitmeta.State{Tombstones: append([]gitmeta.Tombstone(nil), st.Tombstones...)}
	removed := map[stateKey]bool{}
	drop := func(target gitmeta.Target, key string) bool {
		if target != projectTarget || (!strings.HasPrefix(key, reverseKeyPrefix) && !strings.HasPrefix(key, aliasKeyPrefix)) {
			return false
		}
		id := stateKey{target, key}
		if !removed[id] {
			out.Tombstones = append(out.Tombstones, gitmeta.Tombstone{Target: target, Key: key})
			removed[id] = true
		}
		return true
	}
	for _, v := range st.Strings {
		if !drop(v.Target, v.Key) {
			out.Strings = append(out.Strings, v)
		}
	}
	for _, v := range st.Lists {
		if !drop(v.Target, v.Key) {
			out.Lists = append(out.Lists, v)
		}
	}
	for _, v := range st.Sets {
		if !drop(v.Target, v.Key) {
			out.Sets = append(out.Sets, v)
		}
	}
	return out
}
