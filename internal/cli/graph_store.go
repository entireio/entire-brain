package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/entireio/entire-brain/internal/cli/internal/semstore"
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
	"github.com/spf13/cobra"
)

func fetchGraphStore(ctx context.Context, opts Options, repoDir, binary, profile string) (graphStoreDescriptor, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if binary == "" {
		binary = "entire"
	}
	if profile == "" {
		profile = "full"
	}
	args := []string{"graph", "store", "--repo", repoDir, "--profile", profile}
	ignores, err := semanticProviderIgnoreFiles(repoDir)
	if err != nil {
		return graphStoreDescriptor{}, err
	}
	for _, ignore := range ignores {
		args = append(args, "--ignore-file", ignore)
	}
	ctx, cancel := context.WithTimeout(ctx, graphTaskContextTimeout)
	defer cancel()
	stdout, stderr, err := opts.Runner.Run(ctx, repoDir, binary, args...)
	if err != nil {
		detail := strings.ToLower(string(stderr) + string(stdout))
		if strings.Contains(detail, "unknown command") || strings.Contains(detail, "unknown subcommand") {
			return graphStoreDescriptor{}, errGraphStoreUnsupported
		}
		return graphStoreDescriptor{}, fmt.Errorf("Graph read-only store unavailable: %w: %s", err, firstLine(strings.TrimSpace(string(stderr))))
	}
	var d graphStoreDescriptor
	if err := json.Unmarshal(stdout, &d); err != nil {
		return d, fmt.Errorf("decode Graph store descriptor: %w", err)
	}
	if d.StoreUnavailable {
		return d, fmt.Errorf("%w: %s; run %s", errGraphStoreUnavailable, d.Detail, base.BrainInvocation(os.LookupEnv, "refresh index"))
	}
	// A contract mismatch names the binary that is behind: a NEWER contract than
	// this build supports means Graph has moved on and entire-brain must catch
	// up; an OLDER contract means the installed Graph predates the interface
	// brain needs, so Graph is the one to upgrade. Both are a present, responding
	// Graph reporting an incompatible version -- distinct from errGraphStoreUnsupported,
	// which means Graph never answered the store command at all (effectively
	// absent) and is handled separately above.
	if d.ContractVersion > graphStoreSupportedContract {
		return d, fmt.Errorf("%w (Graph reports store contract %d, this build supports %d)", errGraphStoreContractNewer, d.ContractVersion, graphStoreSupportedContract)
	}
	if d.ContractVersion != graphStoreSupportedContract {
		return d, fmt.Errorf("%w (Graph reports store contract %d, this build requires %d)", errGraphStoreContractOlder, d.ContractVersion, graphStoreSupportedContract)
	}
	if err := semstore.ValidateSchema(d.Header.SchemaVersion); err != nil {
		// semstore.ValidateSchema's own text names what this build reads but not
		// who's asking; naming both Graph (the header's source) and entire-brain
		// (what rejected it) here matches the other version-mismatch messages in
		// this file.
		return d, fmt.Errorf("Graph's semantic schema is unsupported by this entire-brain build: %w", err)
	}
	if !filepath.IsAbs(d.Path) || d.EntryDigest == "" || d.Commit == "" || d.Tree == "" {
		return d, errors.New("Graph returned an unsupported or incomplete store descriptor")
	}
	return d, nil
}

// graphStoreSupportedContract is the one read-only store contract this build
// speaks. It is independent of semstore.SchemaVersion: the contract is the
// shape of graphStoreDescriptor and its SQLite views, while the schema is the
// data model inside them (ADR 0001).
const graphStoreSupportedContract = 1

var errGraphStoreUnsupported = fmt.Errorf("installed Graph lacks the read-only store interface; upgrade Graph to enable code inspection (requires store contract %d)", graphStoreSupportedContract)

// errGraphStoreContractNewer flags a store built by a Graph that has already
// moved past the contract this build understands. The fix is the other
// direction from errGraphStoreUnsupported: upgrade entire-brain, not Graph.
var errGraphStoreContractNewer = errors.New("Graph's read-only store contract is newer than this build of entire-brain supports; upgrade entire-brain")

// errGraphStoreContractOlder flags an installed Graph that DOES answer the
// store command but reports a contract version older than this build
// requires -- a present, responding Graph with an incompatible version. This
// is deliberately a different sentinel from errGraphStoreUnsupported (the
// command didn't exist at all, i.e. Graph is effectively absent): status
// treats "absent" as the ordinary, silent case and must not collapse this
// actionable version mismatch into the same bucket.
var errGraphStoreContractOlder = errors.New("Graph's read-only store contract is older than this build of entire-brain requires; upgrade Graph")

var errGraphStoreUnavailable = errors.New("Graph store unavailable")

func loadGraphManifest(ctx context.Context, opts Options, repoDir, brainDir string) (*exportManifest, error) {
	manifest, err := bmanifest.Load(brainDir)
	if err != nil {
		return nil, err
	}
	if err := checkLegacyBrainLayout(brainDir, manifest); err != nil {
		return nil, err
	}
	if manifest.Sources == nil {
		manifest.Sources = &brainSources{}
	}
	profile := graphStoreProfile(manifest)
	d, err := fetchGraphStore(ctx, opts, repoDir, "", profile)
	if err != nil {
		return nil, err
	}
	manifest.Sources.Semantic = semstore.GraphStoreSource(d, brainDir)
	return manifest, nil
}

func graphStoreProfile(manifest *exportManifest) string {
	if manifest != nil && manifest.Sources != nil && manifest.Sources.Semantic != nil && manifest.Sources.Semantic.Profile != "" {
		return manifest.Sources.Semantic.Profile
	}
	return "full"
}

func runGraphOwnedIndex(ctx context.Context, cmd *cobra.Command, opts Options, indexOpts semanticIndexOptions, target string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if indexOpts.worktree {
		return errors.New("Graph owns the committed-tree index; use entire graph query without --head to inspect uncommitted code")
	}
	repoDir, local, err := base.ResolveLocalTargetRepoDir(ctx, opts.Runner, target)
	if err != nil {
		return err
	}
	if !local {
		return fmt.Errorf("index requires a local repository: %s", target)
	}
	if err = enforceIndexContainment(indexOpts.containRoot, repoDir); err != nil {
		return err
	}
	if _, err := base.GitScalar(ctx, opts.Runner, repoDir, "rev-parse", "HEAD"); err != nil {
		return nameDegenerateRepoFailure(ctx, opts.Runner, repoDir, fmt.Errorf("resolve HEAD for Graph index: %w", err))
	}
	storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return err
	}
	if indexOpts.outputExplicit {
		storage.BrainDir, err = filepath.Abs(indexOpts.outputDir)
		if err != nil {
			return err
		}
	}
	if err := requireCurrentBrainLayout(storage.BrainDir); err != nil {
		return err
	}
	binary := indexOpts.graphBinary
	if binary == "" {
		binary = indexOpts.semBinary
	}
	if binary == "" {
		binary = "entire"
	}
	if indexOpts.skipGraph || indexOpts.skipSem {
		return semanticProviderAbsentError{err: errors.New("Graph was explicitly disabled")}
	}
	doctor, warnings := runSemanticDoctor(ctx, opts.Runner, repoDir, binary)
	if !doctor.NoEgress {
		return semanticDoctorFailureError(binary, warnings)
	}
	profile := indexOpts.profile
	if profile == "" {
		profile = "full"
	}
	args := []string{"graph", "index", "--repo", repoDir, "--head", "--profile", profile, "--format", "json"}
	if indexOpts.force {
		args = append(args, "--force")
	}
	ignores, err := semanticProviderIgnoreFiles(repoDir)
	if err != nil {
		return err
	}
	for _, ignore := range ignores {
		args = append(args, "--ignore-file", ignore)
	}
	runner, ok := opts.Runner.(EnvironmentCommandRunner)
	if !ok {
		return errors.New("Graph indexing requires a runner supporting per-command environment")
	}
	// Fail fast with the clear, specific message instead of letting an
	// incompatible Graph run the real index command and surface a muddier
	// binary-sourced error. Covers both directions reported by an installed
	// Graph that actually answered (errGraphStoreContractOlder) and the case
	// where it didn't answer at all (errGraphStoreUnsupported, Graph absent).
	// errGraphStoreContractNewer is deliberately not fail-fasted here: Graph
	// is ahead, not broken, and the real index command is Graph's own concern.
	if _, probeErr := fetchGraphStore(ctx, opts, repoDir, binary, profile); errors.Is(probeErr, errGraphStoreUnsupported) || errors.Is(probeErr, errGraphStoreContractOlder) {
		return probeErr
	}
	timeout := indexOpts.timeout
	if timeout <= 0 {
		timeout = semanticSnapshotTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	indexOpts.reportPhase("building Graph-owned store")
	stdout, stderr, err := runner.RunWithEnv(ctx, repoDir, map[string]string{"ENTIRE_GRAPH_SEARCH_STORE": "1"}, binary, args...)
	if err != nil {
		return fmt.Errorf("Graph index: %w: %s", err, firstLine(strings.TrimSpace(string(stderr))))
	}
	d, err := fetchGraphStore(ctx, opts, repoDir, binary, profile)
	if err != nil {
		return err
	}
	unlock, err := acquireSemanticIndexLock(storage.BrainDir)
	if err != nil {
		return err
	}
	defer unlock()
	// Recheck after taking the writer lock before publishing a new source.
	if err = requireCurrentBrainLayout(storage.BrainDir); err != nil {
		return err
	}
	source := semstore.GraphStoreSource(d, storage.BrainDir)
	source.StorePath = ""
	source.GeneratedAt = opts.Now().UTC()
	source.Profile = profile
	source.GraphRepoRoot = repoDir
	if err = writeBrainSemanticSource(storage.BrainDir, storage.Key, source); err != nil {
		return err
	}

	var indexed struct {
		CacheHit bool `json:"index_cache_hit"`
	}
	_ = json.Unmarshal(stdout, &indexed)
	label := "Graph-owned index"
	if indexed.CacheHit {
		label += " already current"
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s: %d files, %d symbols, %d relations\n", label, d.Files, d.Symbols, d.Relations)
	return nil
}
