package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ashtom/entire-brain/internal/entityindex"
	"github.com/ashtom/entire-brain/internal/factgitmeta"

	"github.com/spf13/cobra"
)

// entities_cmd.go is the entity -> checkpoint index surface: `entities
// backfill` builds the persisted git-meta index from repository history,
// `entities history` answers "which checkpoints/sessions changed entity X"
// without re-parsing anything, and `entities show` prints one commit's stored
// delta document.
//
// Two layers, deliberately:
//
//   - internal/entityindex owns the DURABLE, git-native index (forward delta
//     documents on commit targets, reverse entity -> commit lists on the
//     project target). That is the source of truth and survives a wiped brain.
//   - the cache here is DERIVED and rebuildable: it joins each indexed commit
//     to the checkpoint that produced it (the Entire-Checkpoint trailer) and to
//     that checkpoint's sessions (the brain manifest), which is what a human or
//     an agent actually asked for. It is keyed by the git-meta ref tip, so any
//     write to the index invalidates it and the next query rebuilds.

const (
	entitiesDirName          = "entities"
	entitiesCacheFileName    = "index.json"
	entitiesCachePath        = entitiesDirName + "/" + entitiesCacheFileName
	entityIndexCacheVersion  = 1
	entityIndexCacheMaxKeys  = 50_000
	entityIndexLogChunkSize  = 128
	defaultEntitiesLimit     = 20
	defaultEntitiesBackfill  = 200
	entitiesFreshnessCommits = 10
	// entitiesFreshnessTimeout caps one deterministic freshness pass.
	entitiesFreshnessTimeout = 90 * time.Second
	// entityBranchRevWalkMax bounds the branch-reachability walk behind the
	// optional --branch/branch filter.
	entityBranchRevWalkMax = 20_000
)

// entityIndexCache is the derived, rebuildable checkpoint/session join over the
// git-meta reverse index. MetaTip pins it to the exact git-meta commit it was
// built from: any index write moves the tip and the cache is rebuilt.
type entityIndexCache struct {
	SchemaVersion int                                `json:"schema_version"`
	GeneratedAt   time.Time                          `json:"generated_at"`
	MetaTip       string                             `json:"meta_tip"`
	Truncated     bool                               `json:"truncated,omitempty"`
	Entries       map[string][]entityIndexOccurrence `json:"entries"`
	Aliases       map[string]string                  `json:"aliases,omitempty"`
	// LastTouch is the recency signal SearchKeys needs to resolve a
	// rename-back alias cycle (A -> B -> A) to the spelling actually live in
	// the tree rather than a dead intermediate name — see
	// entityindex.resolveAlias. Additive: an older cache file simply decodes
	// this as nil/empty, which only degrades that one cycle tie-break, not
	// ordinary lookups.
	LastTouch map[string]int64 `json:"last_touch,omitempty"`
}

// entityIndexOccurrence is one commit that changed an entity, joined to the
// checkpoint(s) that captured it and the session(s) those checkpoints belong
// to. CheckpointIDs/SessionIDs are empty for ordinary commits made outside a
// captured agent session — the commit is still the honest answer.
type entityIndexOccurrence struct {
	Commit        string     `json:"commit"`
	CheckpointIDs []string   `json:"checkpoint_ids,omitempty"`
	SessionIDs    []string   `json:"session_ids,omitempty"`
	Subject       string     `json:"subject,omitempty"`
	CommittedAt   *time.Time `json:"committed_at,omitempty"`
}

// entityIndexView is one resolved read of the index: the derived cache, plus
// whether this call had to rebuild it. Forward delta documents deliberately do
// NOT live here — `show` reads them straight from git-meta so the derived cache
// never has to carry every commit's document.
type entityIndexView struct {
	cache   entityIndexCache
	rebuilt bool
	// warning is set when the join could not be rebuilt and a previously
	// persisted (stale) cache is being served instead.
	warning string
}

func newEntitiesCommand(opts Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "entities",
		Short: "Query the persisted entity -> checkpoint index (which checkpoints changed a symbol)",
		Long: `entities queries a persisted, git-native index mapping every code entity
(function, method, class, type) to the commits, checkpoints, and sessions that
changed it.

The index lives as git-meta records beside the brain, is built once by
` + "`entities backfill`" + `, and is kept current by the deterministic (token-free)
refresh the watch loop and the session-end hook already run.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newEntitiesBackfillCommand(opts))
	cmd.AddCommand(newEntitiesHistoryCommand(opts))
	cmd.AddCommand(newEntitiesShowCommand(opts))
	return cmd
}

type entitiesBackfillOptions struct {
	branch          string
	limit           int
	graphBinary     string
	checkpointsOnly bool
	full            bool
	jsonOut         bool
}

func newEntitiesBackfillCommand(opts Options) *cobra.Command {
	backfillOpts := entitiesBackfillOptions{limit: defaultEntitiesBackfill, graphBinary: "entire"}
	cmd := &cobra.Command{
		Use:   "backfill [path]",
		Short: "Index repository history into the entity -> checkpoint index",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runEntitiesBackfill(cmd.Context(), cmd, opts, backfillOpts, entitiesTarget(opts, args))
		},
	}
	cmd.Flags().StringVar(&backfillOpts.branch, "branch", "", "Branch whose first-parent history is indexed (default: the checked-out branch)")
	cmd.Flags().IntVar(&backfillOpts.limit, "limit", defaultEntitiesBackfill, "Maximum NEW commits to index this pass (0 = all)")
	cmd.Flags().StringVar(&backfillOpts.graphBinary, "graph-binary", "entire", "Entire CLI binary that exposes `graph diff`")
	cmd.Flags().BoolVar(&backfillOpts.checkpointsOnly, "checkpoints-only", false, "Index only commits carrying an Entire-Checkpoint trailer (a filtered pass: it does NOT advance the indexed window)")
	cmd.Flags().BoolVar(&backfillOpts.full, "full", false, "Ignore the stored indexed window and re-walk the branch from the tip; the window is re-derived from this pass, so pair it with --limit 0 to repair a whole branch")
	cmd.Flags().BoolVar(&backfillOpts.jsonOut, "json", false, "Emit the build result as JSON")
	return cmd
}

func newEntitiesHistoryCommand(opts Options) *cobra.Command {
	var (
		limit   int
		branch  string
		jsonOut bool
	)
	cmd := &cobra.Command{
		Use:   "history <query>",
		Short: "List the checkpoints and sessions that changed a code entity",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runEntitiesHistory(cmd.Context(), cmd, opts, strings.Join(args, " "), branch, limit, jsonOut, entitiesTarget(opts, nil))
		},
	}
	cmd.Flags().IntVar(&limit, "limit", defaultEntitiesLimit, "Maximum matching entities")
	cmd.Flags().StringVar(&branch, "branch", "", "Only report commits reachable from this branch (default: every indexed commit)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit matches as JSON")
	return cmd
}

func newEntitiesShowCommand(opts Options) *cobra.Command {
	var summary bool
	cmd := &cobra.Command{
		Use:   "show <commit|checkpoint-id>",
		Short: "Print the stored entity delta document for a commit or checkpoint",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runEntitiesShow(cmd.Context(), cmd, opts, args[0], summary, entitiesTarget(opts, nil))
		},
	}
	cmd.Flags().BoolVar(&summary, "summary", false, "Render a human summary instead of the stored delta document")
	return cmd
}

// entitiesTarget resolves the repository path a subcommand operates on.
func entitiesTarget(opts Options, args []string) string {
	if len(args) == 1 && strings.TrimSpace(args[0]) != "" {
		return args[0]
	}
	if opts.Env.RepoRoot != "" {
		return opts.Env.RepoRoot
	}
	return "."
}

func runEntitiesBackfill(ctx context.Context, cmd *cobra.Command, opts Options, backfillOpts entitiesBackfillOptions, target string) error {
	repoDir, storage, store, err := openEntityIndexStore(ctx, opts, target)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	result, err := entityindex.Build(ctx, opts.Runner, store, entityindex.BuildOptions{
		RepoDir:         repoDir,
		GraphBinary:     backfillOpts.graphBinary,
		Branch:          backfillOpts.branch,
		Limit:           backfillOpts.limit,
		CheckpointsOnly: backfillOpts.checkpointsOnly,
		Full:            backfillOpts.full,
		Now:             opts.Now,
	})
	if err != nil {
		return err
	}
	// The index moved, so the derived join is stale by construction. Rebuild it
	// now (the expensive part is the provider work that already happened) so
	// the first query after a backfill is instant.
	if result.Indexed > 0 {
		if _, viewErr := loadEntityIndexView(ctx, opts, repoDir); viewErr != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: entity index cache rebuild failed: %v\n", viewErr)
		}
	}
	if backfillOpts.jsonOut {
		return writeIndentedJSON(out, result)
	}
	fmt.Fprintf(out, "entity index: %d commit(s) indexed, %d skipped, %d failed, %d entity change(s) on %s\n",
		result.Indexed, result.Skipped, result.Failed, result.Entities, result.Branch)
	if !result.Window.Empty() {
		fmt.Fprintf(out, "  indexed window: %s..%s (brain: %s)\n", shortSHA(result.Window.Floor), shortSHA(result.Window.Tip), storage.Key)
	}
	if backfillOpts.checkpointsOnly {
		fmt.Fprintln(out, "  --checkpoints-only is a filtered pass: it indexes checkpoint commits without claiming coverage, so the indexed window is unchanged")
	}
	for _, warning := range result.Warnings {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", warning)
	}
	return nil
}

// entityHistoryResult is the JSON contract of `entities history` and of the
// brain_entity_history MCP tool.
type entityHistoryResult struct {
	Query     string                `json:"query"`
	Branch    string                `json:"branch,omitempty"`
	Matches   []entityHistoryMatch  `json:"matches"`
	Truncated bool                  `json:"truncated,omitempty"`
	Note      string                `json:"note,omitempty"`
	Warnings  []string              `json:"warnings,omitempty"`
	Index     entityHistoryIndexRef `json:"index"`
}

type entityHistoryIndexRef struct {
	MetaTip  string    `json:"meta_tip,omitempty"`
	Entities int       `json:"entities"`
	BuiltAt  time.Time `json:"built_at,omitzero"`
}

type entityHistoryMatch struct {
	EntityKey   string                  `json:"entity_key"`
	Path        string                  `json:"path"`
	Kind        string                  `json:"kind,omitempty"`
	Name        string                  `json:"name"`
	AliasOf     string                  `json:"alias_of,omitempty"`
	Occurrences []entityIndexOccurrence `json:"occurrences"`
}

func runEntitiesHistory(ctx context.Context, cmd *cobra.Command, opts Options, query, branch string, limit int, jsonOut bool, target string) error {
	repoDir, _, _, err := openEntityIndexStore(ctx, opts, target)
	if err != nil {
		return err
	}
	result, err := entityHistory(ctx, opts, repoDir, query, branch, limit)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	for _, warning := range result.Warnings {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", warning)
	}
	if jsonOut {
		return writeIndentedJSON(out, result)
	}
	if len(result.Matches) == 0 {
		fmt.Fprintf(out, "no indexed entity matches %q\n", query)
		if result.Note != "" {
			fmt.Fprintln(out, result.Note)
		}
		return nil
	}
	for _, match := range result.Matches {
		header := match.EntityKey
		if match.AliasOf != "" {
			header += "  (was " + match.AliasOf + ")"
		}
		fmt.Fprintln(out, header)
		for _, occ := range match.Occurrences {
			line := "  " + shortSHA(occ.Commit)
			if len(occ.CheckpointIDs) > 0 {
				line += "  checkpoint=" + strings.Join(occ.CheckpointIDs, ",")
			}
			if len(occ.SessionIDs) > 0 {
				line += "  session=" + strings.Join(occ.SessionIDs, ",")
			}
			if occ.Subject != "" {
				line += "  " + truncateString(occ.Subject, 72)
			}
			fmt.Fprintln(out, line)
		}
	}
	return nil
}

// entityHistory is the single query path behind `entities history` and the
// brain_entity_history MCP tool, so the CLI and the agent surface can never
// answer the same question differently.
func entityHistory(ctx context.Context, opts Options, repoDir, query, branch string, limit int) (entityHistoryResult, error) {
	if limit <= 0 {
		limit = defaultEntitiesLimit
	}
	view, err := loadEntityIndexView(ctx, opts, repoDir)
	if err != nil {
		return entityHistoryResult{}, err
	}
	result := entityHistoryResult{
		Query:   query,
		Branch:  strings.TrimSpace(branch),
		Matches: []entityHistoryMatch{},
		Index: entityHistoryIndexRef{
			MetaTip:  view.cache.MetaTip,
			Entities: len(view.cache.Entries),
			BuiltAt:  view.cache.GeneratedAt,
		},
		Truncated: view.cache.Truncated,
	}
	if view.warning != "" {
		result.Warnings = append(result.Warnings, view.warning)
	}
	if result.Truncated {
		// Route through Warnings rather than a bespoke print: the caller
		// already prints every entry in Warnings to stderr BEFORE branching on
		// --json (see runEntitiesHistory), so this reaches a human reader the
		// same way a JSON consumer already saw it in the "truncated" field.
		// Before this, a human running the plain-text path had no way to know
		// the answer was a clipped subset of the index — a silent partial
		// answer identical in shape to a complete one.
		result.Warnings = append(result.Warnings, fmt.Sprintf(
			"the entity index cache is truncated at %d keys (entityIndexCacheMaxKeys); results may omit entities outside that cap",
			entityIndexCacheMaxKeys))
	}
	if len(view.cache.Entries) == 0 {
		result.Note = "the entity index is empty; run `entire brain entities backfill`"
		return result, nil
	}
	reachable := branchReachableCommits(ctx, opts.Runner, repoDir, result.Branch)
	keys := make([]string, 0, len(view.cache.Entries))
	for key := range view.cache.Entries {
		keys = append(keys, key)
	}
	// Over-fetch when filtering by branch so a match whose occurrences are all
	// off-branch cannot silently consume a result slot.
	searchLimit := limit
	if reachable != nil {
		searchLimit = limit * 4
	}
	for _, hit := range entityindex.SearchKeys(keys, view.cache.Aliases, view.cache.LastTouch, query, searchLimit) {
		lookup := hit.EntityKey
		if hit.AliasOf != "" {
			lookup = hit.AliasOf
		}
		// Union the whole rename/move family — the older spellings that resolve
		// into this key as well as the chain forward from it — so a symbol's
		// history survives the names it has been through no matter which of
		// them the query happened to match.
		var occurrences []entityIndexOccurrence
		for _, key := range entityindex.AliasFamily(view.cache.Aliases, lookup) {
			occurrences = mergeEntityOccurrences(occurrences, view.cache.Entries[key])
		}
		occurrences = filterOccurrencesToBranch(occurrences, reachable)
		if len(occurrences) == 0 {
			continue
		}
		result.Matches = append(result.Matches, entityHistoryMatch{
			EntityKey:   hit.EntityKey,
			Path:        hit.Path,
			Kind:        hit.Kind,
			Name:        hit.Name,
			AliasOf:     hit.AliasOf,
			Occurrences: occurrences,
		})
		if len(result.Matches) >= limit {
			break
		}
	}
	return result, nil
}

// branchReachableCommits returns the set of commits reachable from branch, or
// nil when no branch filter applies (or the branch cannot be resolved — an
// unfiltered answer beats a wrongly empty one).
func branchReachableCommits(ctx context.Context, runner CommandRunner, repoDir, branch string) map[string]struct{} {
	if strings.TrimSpace(branch) == "" || runner == nil {
		return nil
	}
	stdout, _, err := runner.Run(ctx, repoDir, "git", "rev-list", fmt.Sprintf("--max-count=%d", entityBranchRevWalkMax), branch)
	if err != nil {
		return nil
	}
	out := map[string]struct{}{}
	for _, line := range strings.Fields(string(stdout)) {
		out[line] = struct{}{}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func filterOccurrencesToBranch(occurrences []entityIndexOccurrence, reachable map[string]struct{}) []entityIndexOccurrence {
	if reachable == nil {
		return occurrences
	}
	out := make([]entityIndexOccurrence, 0, len(occurrences))
	for _, occ := range occurrences {
		if _, ok := reachable[occ.Commit]; ok {
			out = append(out, occ)
		}
	}
	return out
}

// mergeEntityOccurrences concatenates a pre-rename entity's occurrences with
// its post-rename ones, deduplicating by commit and preserving order.
func mergeEntityOccurrences(older, newer []entityIndexOccurrence) []entityIndexOccurrence {
	out := make([]entityIndexOccurrence, 0, len(older)+len(newer))
	seen := map[string]struct{}{}
	for _, group := range [][]entityIndexOccurrence{older, newer} {
		for _, occ := range group {
			if _, dup := seen[occ.Commit]; dup {
				continue
			}
			seen[occ.Commit] = struct{}{}
			out = append(out, occ)
		}
	}
	return out
}

func runEntitiesShow(ctx context.Context, cmd *cobra.Command, opts Options, ref string, summary bool, target string) error {
	repoDir, _, store, err := openEntityIndexStore(ctx, opts, target)
	if err != nil {
		return err
	}
	state, err := store.State()
	if err != nil {
		return err
	}
	snapshot := entityindex.Load(state)
	commit := strings.TrimSpace(ref)
	if isCheckpointID(commit) {
		view, viewErr := loadEntityIndexView(ctx, opts, repoDir)
		if viewErr != nil {
			return viewErr
		}
		resolved, ok := commitForCheckpoint(view.cache, commit)
		if !ok {
			return fmt.Errorf("no indexed commit carries checkpoint %s", commit)
		}
		commit = resolved
	} else if resolved, resolveErr := gitScalar(ctx, opts.Runner, repoDir, "rev-parse", "--verify", commit+"^{commit}"); resolveErr == nil && resolved != "" {
		commit = resolved
	}
	raw, ok := snapshot.RawDelta(commit)
	if !ok {
		return fmt.Errorf("commit %s is not indexed; run `entire brain entities backfill`", shortSHA(commit))
	}
	out := cmd.OutOrStdout()
	if summary {
		delta, decoded := snapshot.Delta(commit)
		if !decoded {
			fmt.Fprintln(out, raw)
			return nil
		}
		fmt.Fprintf(out, "%s (base %s) computed %s\n", shortSHA(delta.Head), shortSHA(delta.Base), delta.ComputedAt)
		for _, entity := range delta.Entities {
			fmt.Fprintf(out, "  %-9s %s %s (%s:%d)\n", entity.Change, entity.Kind, entity.Name, entity.Path, entity.StartLine)
		}
		return nil
	}
	// Print the stored bytes verbatim: `show` is an inspection surface for what
	// is actually in git, not a re-serialization of it.
	fmt.Fprintln(out, raw)
	return nil
}

// commitForCheckpoint finds the newest indexed commit carrying a checkpoint id.
func commitForCheckpoint(cache entityIndexCache, checkpointID string) (string, bool) {
	best := ""
	var bestAt time.Time
	for _, occurrences := range cache.Entries {
		for _, occ := range occurrences {
			for _, id := range occ.CheckpointIDs {
				if id != checkpointID {
					continue
				}
				at := time.Time{}
				if occ.CommittedAt != nil {
					at = *occ.CommittedAt
				}
				if best == "" || at.After(bestAt) {
					best, bestAt = occ.Commit, at
				}
			}
		}
	}
	return best, best != ""
}

// openEntityIndexStore resolves the repository, its brain storage, and the
// shared local git-meta store the index lives in.
func openEntityIndexStore(ctx context.Context, opts Options, target string) (string, repoStorage, *factgitmeta.MetaStore, error) {
	repoDir, local, err := resolveLocalTargetRepoDir(ctx, opts.Runner, target)
	if err != nil {
		return "", repoStorage{}, nil, err
	}
	if !local {
		return "", repoStorage{}, nil, fmt.Errorf("the entity index requires a local repository path: %s", target)
	}
	storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return "", repoStorage{}, nil, err
	}
	gitDir, err := gitmetaDirForKey(opts.Env, storage.Key)
	if err != nil {
		return "", repoStorage{}, nil, err
	}
	store, err := factgitmeta.OpenMetaStore(gitDir, opts.Now)
	if err != nil {
		return "", repoStorage{}, nil, err
	}
	return repoDir, storage, store, nil
}

// loadEntityIndexView returns the derived cache, rebuilding it from git-meta
// when it is absent, unreadable, or pinned to a stale git-meta tip.
func loadEntityIndexView(ctx context.Context, opts Options, repoDir string) (*entityIndexView, error) {
	storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return nil, err
	}
	gitDir, err := gitmetaDirForKey(opts.Env, storage.Key)
	if err != nil {
		return nil, err
	}
	store, err := factgitmeta.OpenMetaStore(gitDir, opts.Now)
	if err != nil {
		return nil, err
	}
	tip, err := store.Tip()
	if err != nil {
		return nil, err
	}
	view := &entityIndexView{}
	previous, hasPrevious := loadEntityIndexCache(storage.BrainDir)
	if hasPrevious && previous.MetaTip == tip && tip != "" {
		view.cache = previous
		return view, nil
	}
	cache, err := rebuildEntityIndexCache(ctx, opts, repoDir, storage.BrainDir, store, tip)
	if err != nil {
		// The join could not be computed. NEVER persist a partial one: it would
		// be pinned to the current git-meta tip and then served forever, so a
		// single failed `git log` would silently answer "this entity was never
		// touched" for the life of the index. Serve the previous cache if there
		// is one and leave the rebuild to the next call.
		if hasPrevious {
			view.cache = previous
			view.warning = fmt.Sprintf("entity index join could not be rebuilt (%v); serving the previous cache", err)
			return view, nil
		}
		return nil, err
	}
	view.cache = cache
	view.rebuilt = true
	// Persisting is an optimization, never a correctness requirement: a
	// read-only brain dir must still answer queries.
	if err := saveEntityIndexCache(storage.BrainDir, cache); err != nil {
		return view, nil //nolint:nilerr // a cache we could not persist is still a valid answer
	}
	return view, nil
}

// rebuildEntityIndexCache materializes the git-meta index and joins each
// indexed commit to its checkpoint trailer and the sessions those checkpoints
// belong to.
func rebuildEntityIndexCache(ctx context.Context, opts Options, repoDir, brainDir string, store *factgitmeta.MetaStore, tip string) (entityIndexCache, error) {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	cache := entityIndexCache{
		SchemaVersion: entityIndexCacheVersion,
		GeneratedAt:   now().UTC(),
		MetaTip:       tip,
		Entries:       map[string][]entityIndexOccurrence{},
		Aliases:       map[string]string{},
		LastTouch:     map[string]int64{},
	}
	state, err := store.State()
	if err != nil {
		return cache, err
	}
	snapshot := entityindex.Load(state)
	if snapshot.Empty() {
		return cache, nil
	}
	cache.Aliases = snapshot.Aliases()
	cache.LastTouch = snapshot.LastTouch()

	commitMeta, err := entityCommitMetadata(ctx, opts.Runner, repoDir, snapshot.IndexedCommits())
	if err != nil {
		return entityIndexCache{}, err
	}
	sessionsByCheckpoint := sessionIDsByCheckpoint(ctx, opts, repoDir, brainDir)

	keys := snapshot.Keys()
	if len(keys) > entityIndexCacheMaxKeys {
		// Bound the derived artifact rather than writing an unbounded JSON blob
		// beside the brain. The durable index is untouched; only this join is
		// clipped, and the flag says so.
		keys = keys[:entityIndexCacheMaxKeys]
		cache.Truncated = true
	}
	for _, key := range keys {
		commits := snapshot.Commits(key)
		occurrences := make([]entityIndexOccurrence, 0, len(commits))
		for _, sha := range commits {
			meta := commitMeta[sha]
			occ := entityIndexOccurrence{Commit: sha, Subject: meta.subject, CheckpointIDs: meta.checkpointIDs}
			if !meta.committedAt.IsZero() {
				at := meta.committedAt
				occ.CommittedAt = &at
			}
			occ.SessionIDs = sessionIDsFor(sessionsByCheckpoint, meta.checkpointIDs)
			occurrences = append(occurrences, occ)
		}
		// Oldest first. The durable reverse list preserves APPEND order (the
		// exchange format nudges a late-arriving older entry after the newest
		// one rather than re-sorting), and backfill converges backwards — so
		// the stored order alone would answer newest-first on a repository
		// indexed in chunks and oldest-first on one indexed in a single pass.
		// This join is the first place the commit dates are known, so it is
		// where chronology gets imposed.
		sortOccurrencesByCommitDate(occurrences)
		cache.Entries[key] = occurrences
	}
	return cache, nil
}

// sortOccurrencesByCommitDate orders occurrences oldest first, stably, leaving
// undated ones (a commit git could not describe) in their index order at the end.
func sortOccurrencesByCommitDate(occurrences []entityIndexOccurrence) {
	sort.SliceStable(occurrences, func(i, j int) bool {
		left, right := occurrences[i].CommittedAt, occurrences[j].CommittedAt
		if left == nil || right == nil {
			return left != nil && right == nil
		}
		return left.Before(*right)
	})
}

// entityCommitFacts is the per-commit metadata the join needs.
type entityCommitFacts struct {
	subject       string
	committedAt   time.Time
	checkpointIDs []string
}

// entityCommitLogFormat frames one commit as sha, COMMITTER date, subject, and
// full body. The committer date is the one that orders work as it landed: a
// rebase, a cherry-pick, or a patch applied days after it was written keeps its
// author date, so ordering provenance by %aI can attribute a fact to the wrong
// checkpoint. git's %x00/%x1e are placeholders, not literal control bytes: an
// argv string may not contain a NUL, so they only appear in the output.
const entityCommitLogFormat = "--format=%H%x00%cI%x00%s%x00%B%x1e"

// entityCommitMetadata reads subject, committer date, and Entire-Checkpoint
// trailers for the indexed commits, in bounded batches so a large index does
// not build one enormous argv.
//
// It distinguishes the two reasons a batch can fail. A commit that is genuinely
// GONE (pruned, unreachable, a shallow clone's boundary) is not a query failure
// — the index still knows the entity changed there, we just cannot decorate it,
// so it is dropped and the rest of the batch is re-read. Anything else is a git
// failure, and it is returned as an error: swallowing it produced an EMPTY join
// that then got persisted against the current git-meta tip and served forever.
func entityCommitMetadata(ctx context.Context, runner CommandRunner, repoDir string, commits []string) (map[string]entityCommitFacts, error) {
	out := make(map[string]entityCommitFacts, len(commits))
	if runner == nil {
		return out, nil
	}
	for start := 0; start < len(commits); start += entityIndexLogChunkSize {
		end := start + entityIndexLogChunkSize
		if end > len(commits) {
			end = len(commits)
		}
		batch := commits[start:end]
		stdout, err := entityCommitLogBatch(ctx, runner, repoDir, batch)
		if err != nil {
			present, absent := partitionPresentCommits(ctx, runner, repoDir, batch)
			if len(absent) == 0 || len(present) == 0 {
				// Nothing in the batch was missing (so git itself failed), or
				// git claims none of them exist (so the probe failed too).
				// Either way this is not a decoration gap; refuse to build a
				// join that silently omits every one of these commits.
				return nil, fmt.Errorf("read commit metadata for %d commit(s): %w", len(batch), err)
			}
			if stdout, err = entityCommitLogBatch(ctx, runner, repoDir, present); err != nil {
				return nil, fmt.Errorf("read commit metadata for %d retained commit(s): %w", len(present), err)
			}
		}
		for _, record := range strings.Split(string(stdout), gitLogRecordSeparator) {
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
			at, _ := time.Parse(time.RFC3339, strings.TrimSpace(parts[1])) //nolint:errcheck // decoration only
			out[sha] = entityCommitFacts{
				subject:       strings.TrimSpace(parts[2]),
				committedAt:   at.UTC(),
				checkpointIDs: checkpointIDsFromCommitMessageText(parts[3]),
			}
		}
	}
	return out, nil
}

func entityCommitLogBatch(ctx context.Context, runner CommandRunner, repoDir string, commits []string) ([]byte, error) {
	if len(commits) == 0 {
		return nil, nil
	}
	args := append([]string{"log", "--no-walk", entityCommitLogFormat}, commits...)
	stdout, _, err := runner.Run(ctx, repoDir, "git", args...)
	return stdout, err
}

// partitionPresentCommits splits a batch into the commits git can still resolve
// locally and the ones it cannot. It runs only on the failure path.
func partitionPresentCommits(ctx context.Context, runner CommandRunner, repoDir string, commits []string) (present, absent []string) {
	for _, sha := range commits {
		if _, _, err := runner.Run(ctx, repoDir, "git", "cat-file", "-e", sha+"^{commit}"); err != nil {
			absent = append(absent, sha)
			continue
		}
		present = append(present, sha)
	}
	return present, absent
}

// sessionIDsByCheckpoint maps a checkpoint id to the sessions it belongs to.
//
// Two sources, in this order:
//
//  1. The LOCAL CHECKPOINT STORE, which knows the session behind EVERY
//     checkpoint. This is the one that matters: the brain manifest records only
//     each session's LATEST checkpoint, and "the session's last checkpoint" is
//     exactly the coarse answer this index exists to sharpen. Without it a
//     mid-session checkpoint carries no session at all and provenance could
//     never move off the last one.
//  2. The brain manifest, which still covers sessions whose checkpoints are no
//     longer readable locally.
//
// The checkpoint store is best-effort: an unreadable or absent one leaves the
// manifest join exactly as it was.
func sessionIDsByCheckpoint(ctx context.Context, opts Options, repoDir, brainDir string) map[string][]string {
	out := map[string][]string{}
	add := func(checkpoint, id string) {
		checkpoint, id = strings.TrimSpace(checkpoint), strings.TrimSpace(id)
		if checkpoint == "" || id == "" {
			return
		}
		if !containsString(out[checkpoint], id) {
			out[checkpoint] = append(out[checkpoint], id)
		}
	}
	if snapshot, _, err := loadLocalCheckpointSnapshotForVerify(ctx, opts.Runner, repoDir); err == nil && snapshot != nil {
		for _, member := range snapshot.SessionCheckpoints {
			add(member.CheckpointID, member.SessionID)
		}
	}
	if manifest, err := loadBrainManifest(brainDir); err == nil && manifest != nil {
		sessions := manifest.Sessions
		if manifest.Sources != nil && manifest.Sources.Sessions != nil && len(manifest.Sources.Sessions.Sessions) > 0 {
			sessions = manifest.Sources.Sessions.Sessions
		}
		for _, session := range sessions {
			add(session.LatestCheckpoint, session.SessionID)
		}
	}
	for key := range out {
		sort.Strings(out[key])
	}
	return out
}

func sessionIDsFor(byCheckpoint map[string][]string, checkpointIDs []string) []string {
	var out []string
	for _, checkpoint := range checkpointIDs {
		for _, id := range byCheckpoint[checkpoint] {
			if !containsString(out, id) {
				out = append(out, id)
			}
		}
	}
	return out
}

func containsString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

func loadEntityIndexCache(brainDir string) (entityIndexCache, bool) {
	data, err := safeReadFile(filepath.Join(brainDir, filepath.FromSlash(entitiesCachePath)), defaultMaxReadBytes)
	if err != nil {
		return entityIndexCache{}, false
	}
	var cache entityIndexCache
	if err := json.Unmarshal(data, &cache); err != nil {
		return entityIndexCache{}, false
	}
	if cache.SchemaVersion != entityIndexCacheVersion || cache.Entries == nil {
		return entityIndexCache{}, false
	}
	if cache.Aliases == nil {
		cache.Aliases = map[string]string{}
	}
	if cache.LastTouch == nil {
		// A cache written before LastTouch existed: SearchKeys degrades
		// gracefully (nil just means the rename-back-cycle tie-break can't
		// fire), so this is a compatibility default, not a forced rebuild.
		cache.LastTouch = map[string]int64{}
	}
	return cache, true
}

func saveEntityIndexCache(brainDir string, cache entityIndexCache) error {
	if strings.TrimSpace(brainDir) == "" {
		return errors.New("entity index cache: brain dir is empty")
	}
	if err := os.MkdirAll(filepath.Join(brainDir, entitiesDirName), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(cache)
	if err != nil {
		return err
	}
	return withBrainWriteLock(brainDir, func() error {
		return writeBrainRelativeFileAtomic(brainDir, entitiesCachePath, append(data, '\n'), 0o600)
	})
}

// refreshEntityIndexQuietly is the deterministic, zero-token freshness step: it
// extends the branch's indexed window over whatever landed since. It is bounded
// and failure-silent by design — it rides the same tick as the other free
// refresh steps, and a missing `entire graph` provider must never fail a watch
// tick or a session-end hook.
//
// It is also strictly NON-BLOCKING on the git-meta lock. The deadline below
// bounds the git and provider work, but a context cannot interrupt flock(2), so
// a blocking write against a contended store would hang the caller for as long
// as the other writer runs. Contention costs a skipped tick instead.
func refreshEntityIndexQuietly(ctx context.Context, opts Options, repoDir string) error {
	// A hard deadline, not just a commit budget: the provider runs once per new
	// commit, and a watch tick must stay a tick even on a repository where the
	// semantic diff is slow.
	ctx, cancel := context.WithTimeout(ctx, entitiesFreshnessTimeout)
	defer cancel()
	_, _, store, err := openEntityIndexStore(ctx, opts, repoDir)
	if err != nil {
		return err
	}
	result, err := entityindex.Build(ctx, opts.Runner, store, entityindex.BuildOptions{
		RepoDir:     repoDir,
		GraphBinary: "entire",
		Limit:       entitiesFreshnessCommits,
		NonBlocking: true,
		Now:         opts.Now,
	})
	if err != nil {
		return err
	}
	if result.Indexed == 0 {
		return nil
	}
	// Refresh the derived join so the next query does not pay for it.
	_, err = loadEntityIndexView(ctx, opts, repoDir)
	return err
}

func writeIndentedJSON(out io.Writer, value any) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
