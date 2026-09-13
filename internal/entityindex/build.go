package entityindex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ashtom/entire-brain/internal/factgitmeta"
	"github.com/ashtom/entire-brain/internal/factgitmeta/gitmeta"
)

// gitLogRecordSeparator / gitLogFieldSeparator match the export walker's
// framing so a commit subject or body containing newlines cannot split a
// record.
const (
	gitLogRecordSeparator = "\x1e"
	gitLogFieldSeparator  = "\x00"
	// gitLogFormat frames one commit as sha, first-parent list, COMMITTER date,
	// and full body. The separators are git's %x00/%x1e PLACEHOLDERS, not
	// literal control bytes: an argv string may not contain a NUL, so the
	// literal bytes only ever appear in git's output, where parseCommitLog
	// splits on them.
	//
	// The committer date timestamps each reverse-index entry, so the stored
	// records carry real times instead of write-order counters. It is an
	// honest stamp, NOT a sort key: the exchange format appends, and pushing an
	// entry no newer than the list's newest nudges it forward (gitmeta.Apply
	// does this too), so a backfill converging backwards still lands older
	// commits after newer ones. Chronological ORDER is imposed where the commit
	// dates are actually known — see entityCommitMetadata's join.
	gitLogFormat = "--format=%H%x00%P%x00%cI%x00%B%x1e"

	// checkpointTrailer is the commit trailer the Entire CLI stamps on work it
	// captured. It is what makes a commit a "checkpoint commit".
	checkpointTrailer = "Entire-Checkpoint"

	// maxDeltaEntities caps one commit's stored delta document. A pathological
	// commit (a vendored tree drop, a generated-code refresh) can name tens of
	// thousands of entities; storing that verbatim would bloat every subsequent
	// materialize of the whole git-meta state. Over the cap the document is
	// stored with its entities truncated and a warning is reported — the commit
	// still counts as indexed; a full pass repairs it after a cap increase.
	maxDeltaEntities = 2000
	// Legacy writers capped without recording truncation.
	legacyUnmarkedDeltaCap = 2000

	// maxConsecutiveDiffFailures stops a pass whose provider is simply broken
	// (missing binary, wrong flags) instead of invoking it once per commit in
	// the whole history.
	maxConsecutiveDiffFailures = 5

	// maxWalkCommits is the hard ceiling on any single `git log` this package
	// runs, so an enormous history cannot materialize unboundedly in memory. A
	// BOUNDED pass (Limit > 0) walks only as far as its budget can reach, which
	// is what keeps a freshness tick O(new commits) instead of O(history).
	maxWalkCommits = 20000
)

// projectTarget is the repo-wide git-meta target the reverse index, aliases and
// indexed-window records hang off.
var projectTarget = gitmeta.Target{Type: gitmeta.TargetProject}

// CommitTarget is the git-meta target for one commit's forward delta document.
func CommitTarget(sha string) gitmeta.Target {
	return gitmeta.Target{Type: gitmeta.TargetCommit, Value: sha}
}

// BuildOptions configures one indexing pass.
type BuildOptions struct {
	migrating bool
	// IdentityRevision pins the parser rules used for every stored delta.
	IdentityRevision string
	// RepoDir is the repository whose history is indexed.
	RepoDir string
	// GraphBinary is the Entire CLI binary exposing `graph diff` ("entire").
	GraphBinary string
	// Branch is the ref whose first-parent history is walked. Empty resolves to
	// the checked-out branch, then HEAD.
	Branch string
	// Limit caps how many NEW commits this pass indexes (0 = unlimited). It
	// also bounds the git walk itself: a budgeted pass never lists more history
	// than it could index.
	Limit int
	// CheckpointsOnly restricts indexing to commits carrying an
	// Entire-Checkpoint trailer. It is a FILTERED CONVENIENCE PASS and moves
	// NEITHER window cursor: it deliberately leaves plain commits unindexed, so
	// claiming coverage over the range it walked would be a lie that no later
	// pass could detect. It also walks the whole branch (bounded by
	// maxWalkCommits) rather than a cursor-relative range, because the
	// checkpoint commits it wants can be anywhere in history.
	CheckpointsOnly bool
	// Full ignores the stored window and re-walks the branch from the tip.
	// Already indexed commits are still skipped (the index is idempotent), so
	// this is a repair switch, not a rebuild. The window is RE-DERIVED from
	// what this pass covers, so a bounded --full pass can claim less than the
	// window it replaced; pair it with Limit 0 to repair a whole branch.
	Full bool
	// NonBlocking makes the pass acquire the git-meta write lock WITHOUT
	// waiting. The lock is taken before any indexing work, so on contention the
	// pass does nothing at all — no walk, no provider invocation, no write — and
	// reports LockBusy. Freshness ticks set it: flock(2) cannot be interrupted
	// by a context, so a blocking acquire would outlive the caller's own
	// deadline and hang a watch tick or a session-end hook.
	NonBlocking bool
	// Now supplies the computed_at stamp and list timestamps; nil uses
	// time.Now.
	Now func() time.Time
	// Progress, when set, is called after each commit is diffed with the number
	// of commits processed so far. A pass streams its walk, so the total is not
	// known in advance.
	Progress func(done int)
}

// BuildResult reports what one pass did.
type BuildResult struct {
	Branch  string `json:"branch"`
	Tip     string `json:"tip"`
	Scanned int    `json:"scanned"`
	Indexed int    `json:"indexed"`
	Skipped int    `json:"skipped"`
	Failed  int    `json:"failed"`
	// Entities counts entity changes recorded by this pass.
	Entities int `json:"entities"`
	// Window is the branch's indexed range AFTER this pass.
	Window IndexWindow `json:"window"`
	// LockBusy reports that another writer held the git-meta lock and this
	// non-blocking pass wrote nothing at all.
	LockBusy   bool     `json:"lock_busy,omitempty"`
	MetaTip    string   `json:"meta_tip,omitempty"`
	Warnings   []string `json:"warnings,omitempty"`
	NewCommits []string `json:"new_commits,omitempty"`
}

// commitMutations groups every git-meta record one commit contributes, so the
// pre-CAS idempotence re-check can drop a commit's records as a unit.
type commitMutations struct {
	sha  string
	muts []gitmeta.Mutation
}

// commitInfo is one walked commit: its sha, first parent, committer date, and
// message.
type commitInfo struct {
	SHA         string
	Parent      string
	CommittedAt time.Time
	Message     string
}

// Build indexes commits on a branch into the git-meta store: one forward delta
// document per commit, one reverse list append per changed entity, one alias
// per rename/move, and the branch's indexed window. It is idempotent — a commit
// that already carries a forward document is skipped without invoking the
// provider — and it writes AT MOST ONE git-meta commit per pass. The git-meta
// write lock is held for the WHOLE pass, not just the write, so a contended
// pass never pays for work it would have to discard.
//
// The window is extended from both ends. Ticks push the TIP forward over what
// landed since (walking only tip..head, so an unchanged branch costs one empty
// `git log`), and bounded backfill chunks pull the FLOOR backward until it
// reaches the root. Either end stops at the first commit the provider could not
// diff, so the range never claims to cover a hole. A tip that is no longer on
// the head's FIRST-PARENT chain — still an ancestor, but only through some
// merge's second parent — is dropped and the range re-covered from the head:
// `tip..head` omits exactly the commits such a window would go on to claim.
func Build(ctx context.Context, runner Runner, store *factgitmeta.MetaStore, opts BuildOptions) (BuildResult, error) {
	if runner == nil {
		return BuildResult{}, errors.New("entityindex: command runner is required")
	}
	if store == nil {
		return BuildResult{}, errors.New("entityindex: git-meta store is required")
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}

	branch, err := resolveBranch(ctx, runner, opts.RepoDir, opts.Branch)
	if err != nil {
		return BuildResult{}, err
	}
	head, err := revParse(ctx, runner, opts.RepoDir, branch)
	if err != nil {
		return BuildResult{}, fmt.Errorf("entityindex: resolve %s: %w", branch, err)
	}
	result := BuildResult{Branch: branch, Tip: head}

	window := IndexWindow{}
	if !opts.Full || opts.CheckpointsOnly {
		window, err = readRevisionWindow(ctx, runner, store, opts.RepoDir, branch, head, opts.IdentityRevision)
		if err != nil {
			return result, err
		}
	}
	result.Window = window

	// Take the write lock BEFORE any indexing work, and hold it across the whole
	// pass. The work is not cheap — one `entire graph diff` subprocess per new
	// commit — and acquiring the lock only at the write meant a contended pass
	// ran every one of those diffs and then discarded the lot. A non-blocking
	// pass now learns about contention in one syscall instead of after 90
	// seconds of provider invocations.
	//
	// The cost is that a long blocking pass (`entities backfill`) holds the
	// shared git-meta lock for its duration rather than only for its commit.
	// That is the honest trade: the pass is one logical unit, and the writer it
	// blocks would have blocked it right back at the write.
	unlock, err := lockStore(store, opts.NonBlocking)
	if errors.Is(err, factgitmeta.ErrLockBusy) {
		// Nothing was read, nothing was written, nothing was done. Best-effort
		// freshness: the next pass finds the same work waiting.
		result.LockBusy = true
		result.Warnings = append(result.Warnings, "another writer holds the git-meta lock; skipped this pass")
		if tipHash, tipErr := store.Tip(); tipErr == nil {
			result.MetaTip = tipHash
		}
		return result, nil
	}
	if err != nil {
		return result, err
	}
	defer unlock()

	b := &builder{
		ctx:         ctx,
		runner:      runner,
		opts:        opts,
		now:         now,
		result:      &result,
		store:       store,
		budget:      opts.Limit,
		baseMS:      now().UTC().UnixMilli(),
		indexed:     map[string]bool{},
		invalidated: map[string]string{},
	}

	nextWindow := window
	if opts.CheckpointsOnly {
		b.indexCheckpointCommits(head)
	} else {
		nextWindow = b.extendWindow(window, head)
	}
	result.Window = nextWindow

	var windowMut []gitmeta.Mutation
	if !opts.CheckpointsOnly {
		key := RevisionKey(WindowKey(branch), opts.IdentityRevision)
		raw, present, readErr := store.ReadString(projectTarget, key)
		if readErr != nil {
			return result, readErr
		}
		if nextWindow.Empty() {
			if present {
				windowMut = append(windowMut, gitmeta.Mutation{Op: gitmeta.OpRemoveKey, Target: projectTarget, Key: key})
			}
		} else if !present || raw != EncodeWindow(nextWindow) {
			windowMut = append(windowMut, gitmeta.Mutation{Op: gitmeta.OpSetString, Target: projectTarget, Key: key, Value: EncodeWindow(nextWindow)})
		}
	}
	if len(b.groups) == 0 && len(windowMut) == 0 && len(b.invalidated) == 0 {
		tipHash, tipErr := store.Tip()
		if tipErr != nil {
			return result, tipErr
		}
		result.MetaTip = tipHash
		return result, nil
	}

	metaTip, err := store.UpdateLocked(b.mutCount+len(windowMut)+len(b.invalidated), func(current gitmeta.State) (gitmeta.State, error) {
		commitWindowMut := append([]gitmeta.Mutation(nil), windowMut...)
		// Re-check idempotence against the state actually held under the lock: a
		// concurrent writer may have indexed the same commits between our read
		// and the CAS. A commit's records move together, so the WHOLE group is
		// dropped — replaying only its reverse appends would duplicate entries.
		//
		// The one exception is a commit whose CURRENTLY stored document is
		// itself a stale truncation (written under a lower maxDeltaEntities cap
		// than is in effect now): that document does not satisfy today's cap
		// either way, so it is not the "someone already indexed this" case the
		// guard exists for, and the group is let through to replace it. A
		// concurrent writer that already landed a fully-repaired document in
		// the meantime still wins the drop, exactly as before.
		var cleanup []gitmeta.Mutation
		for sha, expected := range b.invalidated {
			key := RevisionKey(ForwardKey, opts.IdentityRevision)
			if raw, ok := current.CurrentString(CommitTarget(sha), key); !ok || raw != expected {
				continue
			}
			cleanup = append(cleanup, gitmeta.Mutation{Op: gitmeta.OpRemoveKey, Target: CommitTarget(sha), Key: key})
			for _, list := range current.Lists {
				original, sameRevision := originalRevisionKey(list.Key, opts.IdentityRevision)
				if list.Target != projectTarget || !sameRevision || !strings.HasPrefix(original, reverseKeyPrefix) {
					continue
				}
				for _, entry := range list.Entries {
					if entry.Value == sha {
						cleanup = append(cleanup, gitmeta.Mutation{Op: gitmeta.OpListRemove, Target: projectTarget, Key: list.Key, Value: sha})
						break
					}
				}
			}
		}
		if len(cleanup) > 0 {
			// Other branches may share this poisoned forward record. Retract
			// their coverage too; each branch can safely re-establish its window.
			for _, value := range current.Strings {
				original, sameRevision := originalRevisionKey(value.Key, opts.IdentityRevision)
				if value.Target == projectTarget && sameRevision && strings.HasPrefix(original, windowKeyPrefix) {
					cleanup = append(cleanup, gitmeta.Mutation{Op: gitmeta.OpRemoveKey, Target: projectTarget, Key: value.Key})
				}
			}
			current = applyBatch(current, cleanup)
			if !nextWindow.Empty() && !opts.CheckpointsOnly {
				commitWindowMut = []gitmeta.Mutation{{Op: gitmeta.OpSetString, Target: projectTarget, Key: RevisionKey(WindowKey(branch), opts.IdentityRevision), Value: EncodeWindow(nextWindow)}}
			}
		}
		// Index memberships once, rather than scanning the full store for
		// every repaired entity.
		existingEntries := map[stateKey]map[string]bool{}
		for _, list := range current.Lists {
			values := map[string]bool{}
			for _, entry := range list.Entries {
				values[entry.Value] = true
			}
			existingEntries[stateKey{list.Target, list.Key}] = values
		}
		pending := make([]gitmeta.Mutation, 0, b.mutCount+len(windowMut))
		for _, group := range b.groups {
			forwardKey := RevisionKey(ForwardKey, opts.IdentityRevision)
			if current.HasKey(CommitTarget(group.sha), forwardKey) {
				// HasKey, not CurrentString, is the presence test: it also sees a
				// list or set value, so a group is never let through to clobber a
				// non-string value. The stored STRING is then consulted only to
				// spot the stale-truncation exception above.
				raw, isString := current.CurrentString(CommitTarget(group.sha), forwardKey)
				if !isString || !deltaNeedsRepair(raw) {
					continue
				}
			}
			// Keep existing reverse timestamps and later alias decisions when
			// extending a truncated document. Only newly discovered entities
			// should add history entries.
			oldRaw, _ := current.CurrentString(CommitTarget(group.sha), forwardKey)
			var old Delta
			_ = json.Unmarshal([]byte(oldRaw), &old)
			oldAliases := map[string]bool{}
			for _, e := range old.Entities {
				if k, ok := e.OldKey(); ok {
					oldAliases[RevisionKey(AliasRecordKey(k), opts.IdentityRevision)] = true
				}
			}
			for _, m := range group.muts {
				if m.Op == gitmeta.OpListPush && existingEntries[stateKey{m.Target, m.Key}][m.Value] {
					continue
				}
				if m.Op == gitmeta.OpSetString && m.Target == projectTarget && oldAliases[m.Key] {
					continue
				}
				pending = append(pending, m)
			}
		}
		pending = append(pending, commitWindowMut...)

		if len(pending) == 0 && len(cleanup) == 0 {
			return gitmeta.State{}, factgitmeta.ErrNoUpdate
		}
		return applyBatch(current, pending), nil
	})
	if err != nil {
		return result, err
	}
	result.MetaTip = metaTip
	return result, nil
}

// lockStore takes the git-meta write lock the way this pass asked for it: a
// non-blocking pass reports contention immediately, a blocking one waits.
func lockStore(store *factgitmeta.MetaStore, nonBlocking bool) (func(), error) {
	if nonBlocking {
		return store.TryLock()
	}
	return store.Lock()
}

// readWindow loads the branch's stored window and discards it when it no longer
// describes this branch (a rebase, a reset, a pruned tip). Reading the cursor is
// ONE blob, not a materialization of the whole index: a tick that finds nothing
// new must not pay for every record in the store.
func readWindow(ctx context.Context, runner Runner, store *factgitmeta.MetaStore, repoDir, branch, head string) (IndexWindow, error) {
	return readRevisionWindow(ctx, runner, store, repoDir, branch, head, "")
}

func readRevisionWindow(ctx context.Context, runner Runner, store *factgitmeta.MetaStore, repoDir, branch, head, revision string) (IndexWindow, error) {
	raw, ok, err := store.ReadString(projectTarget, RevisionKey(WindowKey(branch), revision))
	if err != nil {
		return IndexWindow{}, err
	}
	if !ok {
		return IndexWindow{}, nil
	}
	window, decoded := DecodeWindow(raw)
	if !decoded {
		return IndexWindow{}, nil
	}
	// The window claims "floor..tip is indexed ON THIS BRANCH". If the tip is no
	// longer an ancestor of the head, the claim is about history this branch no
	// longer has, and keeping it would leave everything after the divergence
	// permanently unindexed. Drop it and re-cover from the head.
	if !isAncestor(ctx, runner, repoDir, window.Tip, head) {
		return IndexWindow{}, nil
	}
	return window, nil
}

// builder carries one pass's accumulators.
type builder struct {
	ctx    context.Context
	runner Runner
	opts   BuildOptions
	now    func() time.Time
	result *BuildResult
	store  *factgitmeta.MetaStore

	groups   []commitMutations
	mutCount int
	baseMS   int64

	// budget is the remaining NEW-commit allowance; <= 0 with opts.Limit > 0
	// means exhausted, and opts.Limit == 0 means unlimited.
	budget              int
	done                int
	consecutiveFailures int

	// indexed memoizes per-commit forward-document presence, so a commit at a
	// window edge is probed at most once per pass.
	indexed     map[string]bool
	invalidated map[string]string
}

// isGraftedBoundary reports whether a commit git described as PARENTLESS is
// really a shallow clone's boundary rather than a root.
//
// A graft truncates `git log --format=%P` to nothing, so a boundary commit is
// indistinguishable from a root in the walk — and a root is diffed against the
// empty tree, which would record every entity in the whole tree as ADDED by
// that one commit. The raw object still carries its parent lines, so
// `cat-file commit` tells the two apart.
func (b *builder) isGraftedBoundary(sha string) bool {
	stdout, _, err := b.runner.Run(b.ctx, b.opts.RepoDir, "git", "--no-replace-objects", "cat-file", "commit", sha)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(stdout), "\n") {
		if strings.TrimSpace(line) == "" {
			break // the header ends at the blank line before the message
		}
		if strings.HasPrefix(line, "parent ") {
			return true
		}
	}
	return false
}

func (b *builder) exhausted() bool { return b.opts.Limit > 0 && b.budget <= 0 }

// walkBudget bounds a git walk to what this pass could still index. It is the
// difference between a tick that reads ten commits and one that reads twenty
// thousand every time it runs.
func (b *builder) walkBudget() int {
	if b.opts.Limit <= 0 || b.budget > maxWalkCommits {
		return maxWalkCommits
	}
	return b.budget
}

// extendWindow pushes the tip forward over commits that landed since, then
// pulls the floor backward over older history, and returns the resulting range.
func (b *builder) extendWindow(window IndexWindow, head string) IndexWindow {
	// --- forward: cover what landed since the tip ---------------------------
	var forward []commitInfo
	if !window.Empty() && window.Tip != head {
		// Only tip..head: O(new commits), and empty (one instant `git log`)
		// when the branch has not moved. Bounded by what this pass can index,
		// so a Limit:10 tick after a 5000-commit fast-forward materializes ten
		// commit bodies rather than five thousand.
		commits, err := b.walkForward(window.Tip+".."+head, b.walkBudget())
		// Oldest-first, so the tip advances over a contiguous run.
		for i, j := 0, len(commits)-1; i < j; i, j = i+1, j-1 {
			commits[i], commits[j] = commits[j], commits[i]
		}
		if err != nil {
			b.result.Warnings = append(b.result.Warnings, err.Error())
		} else if !contiguousWithTip(commits, window.Tip) {
			// The range does not sit DIRECTLY on top of the tip, so the tip is
			// not on the head's first-parent chain — it is reachable only
			// through some merge's second parent. `A..B` excludes everything
			// reachable from A by ANY parent, so the first-parent commits
			// between the tip and the head were never listed and are NOT
			// indexed; advancing the tip over them would make the window claim
			// coverage of a hole no later pass could ever detect. Re-cover from
			// the head instead, exactly as a diverged tip does.
			b.result.Warnings = append(b.result.Warnings, fmt.Sprintf(
				"indexed window tip %s is not on the first-parent chain of %s; re-covering from the head",
				short(window.Tip), short(head)))
			window = IndexWindow{}
			commits = nil
		}
		forward = commits
	}

	next := window
	for _, commit := range forward {
		if b.exhausted() {
			break
		}
		b.result.Scanned++
		if b.alreadyIndexed(commit) {
			b.result.Skipped++
			next.Tip = commit.SHA
			continue
		}
		ok, stop := b.indexCommit(commit)
		if !ok {
			break // a hole the tip must not step over
		}
		next.Tip = commit.SHA
		if stop {
			break
		}
	}

	// --- backward: cover older history --------------------------------------
	if b.exhausted() {
		return next
	}
	var (
		commits []commitInfo
		err     error
	)
	if window.Empty() {
		// Nothing covered yet: start at the head itself.
		commits, err = b.walk(head, b.walkBudget(), 0)
	} else {
		// Ancestors of the floor. `git log <floor>` includes the floor, so ask
		// for one extra and drop it; `<floor>^` would fail on a root commit.
		commits, err = b.walk(window.Floor, b.walkBudget()+1, 0)
		if len(commits) > 0 && commits[0].SHA == window.Floor {
			commits = commits[1:]
		}
	}
	if err != nil {
		b.result.Warnings = append(b.result.Warnings, err.Error())
		return next
	}

	// The walk is newest-first, but index OLDEST-first: the reverse-index list
	// entries a commit contributes are timestamped from the commit, and writing
	// them in commit order keeps the stored order stable under applyBatch's
	// collision nudge. The walk is already bounded by the remaining budget, so
	// every candidate here is one this pass can afford.
	covered := make(map[string]bool, len(commits))
	for i := len(commits) - 1; i >= 0; i-- {
		commit := commits[i]
		b.result.Scanned++
		if b.alreadyIndexed(commit) {
			b.result.Skipped++
			covered[commit.SHA] = true
			continue
		}
		if b.exhausted() {
			continue
		}
		ok, stop := b.indexCommit(commit)
		covered[commit.SHA] = ok
		if stop {
			break
		}
	}

	// The floor may only descend over an unbroken run below the current floor.
	//
	// The one exception is ESTABLISHING a window from nothing (tip == ""): the
	// range then has no anchor yet, and the head is only its first candidate. An
	// undiffable head would otherwise leave the window unestablished forever —
	// covered[head] stays false, nothing is recorded, and every later tick
	// re-walks the whole budget to rediscover the same hole. Anchoring at the
	// newest commit that actually diffed demotes it to an ordinary forward-path
	// hole, which costs one probe per tick and converges.
	floor, tip := window.Floor, next.Tip
	for _, commit := range commits { // newest-first: adjacent to the floor first
		if !covered[commit.SHA] {
			if tip == "" {
				continue // still looking for the newest diffable commit
			}
			break
		}
		floor = commit.SHA
		if tip == "" {
			tip = commit.SHA
		}
	}
	return IndexWindow{Floor: floor, Tip: tip}
}

// indexCheckpointCommits is the --checkpoints-only pass: index unindexed
// checkpoint commits anywhere in the branch's history and move no cursor.
func (b *builder) indexCheckpointCommits(head string) {
	commits, err := b.walk(head, maxWalkCommits, 0)
	if err != nil {
		b.result.Warnings = append(b.result.Warnings, err.Error())
		return
	}
	for _, commit := range commits {
		if b.exhausted() {
			break
		}
		b.result.Scanned++
		if len(CheckpointIDs(commit.Message)) == 0 {
			b.result.Skipped++
			continue
		}
		if b.alreadyIndexed(commit) {
			b.result.Skipped++
			continue
		}
		if _, stop := b.indexCommit(commit); stop {
			break
		}
	}
}

// alreadyIndexed reports whether a commit already carries a forward document
// that fully reflects the CURRENT maxDeltaEntities cap. It reads ONE record
// rather than materializing the whole state, and is only ever asked about
// commits OUTSIDE the covered window — everything inside it is indexed by the
// window's own definition.
//
// A commit whose stored document was truncated wrote EXACTLY maxDeltaEntities
// entities at the time it was indexed (that is what truncation means). If the
// cap has since risen, that stored length is now smaller than the current cap,
// which is the one signal available without a second provider invocation that
// re-diffing would recover more of the commit. Such a commit is reported as
// NOT indexed so the ordinary re-diff path (idempotent; see indexCommit)
// refreshes it instead of leaving it stuck at the old, lower cap forever. A
// commit truncated at the SAME cap it still stores (no cap change) is left
// alone: re-diffing it would only reproduce the identical truncation.
func (b *builder) alreadyIndexed(commit commitInfo) bool {
	sha := commit.SHA
	if known, ok := b.indexed[sha]; ok {
		return known
	}
	raw, present, err := b.store.ReadString(CommitTarget(sha), RevisionKey(ForwardKey, b.opts.IdentityRevision))
	if err != nil {
		// Treat an unreadable probe as "not indexed": re-indexing is idempotent
		// (the pre-CAS re-check drops the duplicate), skipping is not.
		present = false
	}
	if present && b.opts.Full && !b.opts.CheckpointsOnly {
		var delta Delta
		if json.Unmarshal([]byte(raw), &delta) == nil && delta.Base == EmptyTreeSHA &&
			(commit.Parent != "" || b.isGraftedBoundary(sha)) {
			b.invalidated[sha] = raw
			present = false
			b.result.Warnings = append(b.result.Warnings, "retracting a previously fabricated root delta for commit "+short(sha))
		}
	}
	if present && deltaNeedsRepair(raw) {
		present = false
	}
	b.indexed[sha] = present
	return present
}

// deltaNeedsRepair reports whether a stored forward-document JSON was
// truncated under a LOWER maxDeltaEntities cap than is in effect now. A
// truncated document always stores exactly the cap's worth of entities at the
// time it was written, so a stored length below today's cap is the signal
// that re-diffing could recover more of the commit; a stored length equal to
// today's cap means the cap has not moved since, and re-diffing would only
// reproduce the identical truncation. An undecodable document is treated as
// NOT needing repair — an unknown shape is not evidence of a stale cap.
func deltaNeedsRepair(raw string) bool {
	var delta Delta
	if json.Unmarshal([]byte(raw), &delta) != nil {
		return false
	}
	// Legacy writers silently capped at 2000. An unmarked document at
	// that limit is ambiguous and needs one verification pass. New writers
	// record EntityCount for truncated or cap-sized documents.
	return (delta.Truncated && len(delta.Entities) < maxDeltaEntities) ||
		(!delta.Truncated && delta.EntityCount == 0 && len(delta.Entities) == legacyUnmarkedDeltaCap)
}

// indexCommit diffs one commit and stages its records. indexed=false means the
// commit was NOT recorded (so no cursor may step past it); stop=true means the
// whole pass must end.
func (b *builder) indexCommit(commit commitInfo) (indexed bool, stop bool) {
	if commit.Parent == "" && b.isGraftedBoundary(commit.SHA) {
		// A shallow clone's boundary is NOT a root: its parent exists, it is
		// just not in this clone, so the commit's delta cannot be computed.
		// Diffing it against the empty tree the way a real root is diffed would
		// record every entity in the whole tree as ADDED by this one commit —
		// a durable, git-native, silently fabricated answer to "which commit
		// introduced this symbol". Treat it as the hole it is, so no cursor
		// steps over it and nothing is written.
		b.consecutiveFailures = 0
		b.result.Failed++
		b.result.Warnings = append(b.result.Warnings, fmt.Sprintf(
			"commit %s has a shallow or grafted boundary; its original parent is hidden, so its delta cannot be computed; fetch complete history and run entities backfill --full",
			short(commit.SHA)))
		return false, false
	}
	b.done++
	if b.opts.Progress != nil {
		b.opts.Progress(b.done)
	}
	delta, err := diffCommit(b.ctx, b.runner, b.opts.RepoDir, b.opts.GraphBinary, commit.Parent, commit.SHA, b.now(), b.opts.migrating, b.opts.IdentityRevision)
	if err != nil {
		b.result.Failed++
		b.result.Warnings = append(b.result.Warnings, err.Error())
		b.consecutiveFailures++
		if b.consecutiveFailures >= maxConsecutiveDiffFailures {
			b.result.Warnings = append(b.result.Warnings, fmt.Sprintf("stopped after %d consecutive provider failures", b.consecutiveFailures))
			return false, true
		}
		return false, false
	}
	b.consecutiveFailures = 0
	if len(delta.Entities) >= maxDeltaEntities || len(delta.Entities) == legacyUnmarkedDeltaCap {
		delta.EntityCount = len(delta.Entities)
	}
	if len(delta.Entities) > maxDeltaEntities {
		b.result.Warnings = append(b.result.Warnings, fmt.Sprintf("commit %s changed %d entities; stored the first %d", short(commit.SHA), len(delta.Entities), maxDeltaEntities))
		delta.Truncated = true
		delta.Entities = delta.Entities[:maxDeltaEntities]
	}
	delta.IdentityRevision = b.opts.IdentityRevision
	encoded, err := json.Marshal(delta)
	if err != nil {
		b.result.Failed++
		b.result.Warnings = append(b.result.Warnings, fmt.Sprintf("commit %s: encode delta: %v", short(commit.SHA), err))
		return false, false
	}
	group := commitMutations{sha: commit.SHA, muts: []gitmeta.Mutation{{
		Op:     gitmeta.OpSetString,
		Target: CommitTarget(commit.SHA),
		Key:    RevisionKey(ForwardKey, b.opts.IdentityRevision),
		Value:  string(encoded),
	}}}
	b.mutCount++
	// One timestamp for every entry this commit contributes, taken from the
	// commit itself.
	entryMS := b.baseMS + int64(b.mutCount)
	if !commit.CommittedAt.IsZero() {
		entryMS = commit.CommittedAt.UnixMilli()
	}
	for _, entity := range delta.Entities {
		group.muts = append(group.muts, gitmeta.Mutation{
			Op:     gitmeta.OpListPush,
			Target: projectTarget,
			Key:    RevisionKey(EntityRecordKey(entity.Key()), b.opts.IdentityRevision),
			Value:  commit.SHA,
			NowMS:  entryMS,
		})
		b.mutCount++
		if oldKey, ok := entity.OldKey(); ok {
			group.muts = append(group.muts, gitmeta.Mutation{
				Op:     gitmeta.OpSetString,
				Target: projectTarget,
				Key:    RevisionKey(AliasRecordKey(oldKey), b.opts.IdentityRevision),
				Value:  entity.Key(),
			})
			b.mutCount++
		}
	}
	b.groups = append(b.groups, group)
	b.indexed[commit.SHA] = true
	b.budget--
	b.result.Indexed++
	b.result.Entities += len(delta.Entities)
	b.result.NewCommits = append(b.result.NewCommits, commit.SHA)
	return true, false
}

// walkForward lists the OLDEST maxCount commits of a tip..head range, newest
// first.
//
// `git log --max-count=N` keeps the NEWEST N, which is the wrong end here: the
// tip may only advance over a run contiguous with the current tip, so a
// budgeted walk that took the newest end would either step the cursor over the
// commits it never listed or make no progress at all on every future tick. The
// range is counted first (`rev-list --count` reads no commit bodies) and the
// walk skips past everything the budget cannot reach.
func (b *builder) walkForward(spec string, maxCount int) ([]commitInfo, error) {
	if maxCount <= 0 || maxCount > maxWalkCommits {
		maxCount = maxWalkCommits
	}
	total, err := b.countCommits(spec)
	if err != nil {
		return nil, err
	}
	skip := 0
	if total > maxCount {
		skip = total - maxCount
	}
	return b.walk(spec, maxCount, skip)
}

// contiguousWithTip reports whether a forward range walk actually starts one
// commit above the window tip. `git log --first-parent <tip>..<head>` lists the
// head's first-parent chain MINUS everything reachable from the tip by any
// parent, so when the tip is only a merge's SECOND parent the range silently
// omits the first-parent commits between them and the walk cannot be used to
// advance the cursor. The walk is bounded from the NEWEST end (walkForward
// skips past what the budget cannot reach), so its oldest element is always the
// oldest commit of the range and its first parent is always the tip when the
// range really is contiguous.
//
// commits must already be oldest-first. An empty range with tip != head means
// the same thing: if the tip were on the head's first-parent chain there would
// be at least one commit above it, and no commit above the tip is reachable
// from it.
func contiguousWithTip(commits []commitInfo, tip string) bool {
	if len(commits) == 0 {
		return false
	}
	return commits[0].Parent == tip
}

// countCommits counts the first-parent commits a revision spec names, without
// reading a single commit body.
func (b *builder) countCommits(spec string) (int, error) {
	stdout, _, err := b.runner.Run(b.ctx, b.opts.RepoDir, "git", "rev-list", "--count", "--first-parent", spec)
	if err != nil {
		return 0, fmt.Errorf("entityindex: count %s: %w", spec, err)
	}
	count, err := strconv.Atoi(strings.TrimSpace(string(stdout)))
	if err != nil {
		return 0, fmt.Errorf("entityindex: count %s: %w", spec, err)
	}
	return count, nil
}

// walk lists first-parent commits for a revision spec, newest first, bounded by
// maxCount and starting skip commits down from the newest. Merge commits
// contribute only their first-parent diff, so a merge is recorded as the change
// it introduced to the branch rather than as every commit it brought along.
func (b *builder) walk(spec string, maxCount int, skip int) ([]commitInfo, error) {
	if maxCount <= 0 || maxCount > maxWalkCommits {
		maxCount = maxWalkCommits
	}
	args := []string{"log", "--first-parent", gitLogFormat, fmt.Sprintf("--max-count=%d", maxCount)}
	if skip > 0 {
		args = append(args, fmt.Sprintf("--skip=%d", skip))
	}
	args = append(args, spec)
	stdout, _, err := b.runner.Run(b.ctx, b.opts.RepoDir, "git", args...)
	if err != nil {
		return nil, fmt.Errorf("entityindex: walk %s: %w", spec, err)
	}
	return parseCommitLog(stdout), nil
}

// isAncestor reports whether `ancestor` is reachable from `descendant` (a commit
// is its own ancestor). A failed probe answers false: the caller then re-covers
// from the head, which is the safe direction.
func isAncestor(ctx context.Context, runner Runner, repoDir, ancestor, descendant string) bool {
	if ancestor == "" || descendant == "" {
		return false
	}
	if ancestor == descendant {
		return true
	}
	_, _, err := runner.Run(ctx, repoDir, "git", "merge-base", "--is-ancestor", ancestor, descendant)
	return err == nil
}

// CheckpointIDs extracts the Entire-Checkpoint trailer values from a commit
// message, in first-seen order and deduplicated.
func CheckpointIDs(message string) []string {
	var ids []string
	seen := map[string]struct{}{}
	for _, line := range strings.Split(message, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), checkpointTrailer+":")
		if !ok {
			continue
		}
		id := strings.TrimSpace(rest)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids
}

// resolveBranch folds the requested branch, the checked-out branch, and HEAD.
func resolveBranch(ctx context.Context, runner Runner, repoDir, requested string) (string, error) {
	if b := strings.TrimSpace(requested); b != "" {
		return b, nil
	}
	stdout, _, err := runner.Run(ctx, repoDir, "git", "branch", "--show-current")
	if err == nil {
		if b := strings.TrimSpace(string(stdout)); b != "" {
			return b, nil
		}
	}
	return "HEAD", nil
}

func revParse(ctx context.Context, runner Runner, repoDir, rev string) (string, error) {
	stdout, _, err := runner.Run(ctx, repoDir, "git", "rev-parse", "--verify", rev+"^{commit}")
	if err != nil {
		return "", err
	}
	sha := strings.TrimSpace(string(stdout))
	if sha == "" {
		return "", fmt.Errorf("empty rev-parse output for %q", rev)
	}
	return sha, nil
}

// parseCommitLog decodes the framed `git log` output into commits.
func parseCommitLog(data []byte) []commitInfo {
	var out []commitInfo
	for _, record := range strings.Split(string(data), gitLogRecordSeparator) {
		record = strings.TrimLeft(record, "\n")
		if strings.TrimSpace(record) == "" {
			continue
		}
		parts := strings.SplitN(record, gitLogFieldSeparator, 4)
		if len(parts) != 4 {
			continue
		}
		sha := strings.TrimSpace(parts[0])
		if sha == "" {
			continue
		}
		parent := ""
		if fields := strings.Fields(parts[1]); len(fields) > 0 {
			parent = fields[0]
		}
		committedAt, _ := time.Parse(time.RFC3339, strings.TrimSpace(parts[2])) //nolint:errcheck // ordering hint only
		out = append(out, commitInfo{SHA: sha, Parent: parent, CommittedAt: committedAt.UTC(), Message: parts[3]})
	}
	return out
}
