package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const (
	distillQualitySnapshotContractV1  = "distill_paired_private_source_snapshot_v1"
	distillQualitySnapshotMaxSourceV1 = int64(1 << 30)
)

type distillQualitySnapshotArmV1 struct {
	Name      string `json:"name"`
	ConfigDir string `json:"config_dir"`
	DataDir   string `json:"data_dir"`
	StateDir  string `json:"state_dir"`
	CacheDir  string `json:"cache_dir"`
	BrainDir  string `json:"brain_dir"`
}

type distillQualitySnapshotManifestV1 struct {
	Contract                string                        `json:"contract"`
	SchemaVersion           int                           `json:"schema_version"`
	GeneratedAt             string                        `json:"generated_at"`
	RepositoryKey           string                        `json:"repository_key"`
	SourceScopeDigest       string                        `json:"source_scope_digest"`
	TaxonomyDigest          string                        `json:"taxonomy_digest"`
	TaxonomySnapshotDigest  string                        `json:"taxonomy_snapshot_digest"`
	SourceManifestVersion   int                           `json:"source_manifest_version"`
	NormalizerVersion       string                        `json:"normalizer_version"`
	SelectedSessions        int                           `json:"selected_sessions"`
	ZeroCandidateSessions   int                           `json:"zero_candidate_sessions"`
	SourceBytes             int64                         `json:"source_bytes"`
	PrivateSourceSnapshot   bool                          `json:"private_source_snapshot"`
	ReferenceLabelsComplete bool                          `json:"reference_labels_complete"`
	ReleaseGateEvidence     bool                          `json:"release_gate_evidence"`
	Arms                    []distillQualitySnapshotArmV1 `json:"arms"`
}

type distillQualitySnapshotSourceV1 struct {
	session exportSession
	branch  string
	data    []byte
	digest  string
}

type distillQualitySnapshotSourceIdentityV1 struct {
	Session       exportSession `json:"session"`
	ContentDigest string        `json:"content_digest"`
}

func distillQualitySnapshotSourceIdentityBytesV1(session exportSession, resolvedBranch, contentDigest string) ([]byte, error) {
	session.Branch = resolvedBranch
	return json.Marshal(distillQualitySnapshotSourceIdentityV1{Session: session, ContentDigest: contentDigest})
}

func buildDistillQualitySnapshotV1(ctx context.Context, opts Options, target, outDir, branch, session string, now time.Time) (distillQualitySnapshotManifestV1, error) {
	return buildDistillQualitySnapshotLimitedV1(ctx, opts, target, outDir, branch, session, now, distillQualitySnapshotMaxSourceV1)
}

func buildDistillQualitySnapshotLimitedV1(ctx context.Context, opts Options, target, outDir, branch, session string, now time.Time, maxSourceBytes int64) (distillQualitySnapshotManifestV1, error) {
	if maxSourceBytes <= 0 {
		return distillQualitySnapshotManifestV1{}, errors.New("snapshot aggregate source-byte bound must be positive")
	}
	repoDir, local, err := resolveLocalTargetRepoDir(ctx, opts.Runner, target)
	if err != nil {
		return distillQualitySnapshotManifestV1{}, err
	}
	if !local {
		return distillQualitySnapshotManifestV1{}, errors.New("distill-quality snapshot requires a local repository")
	}
	storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return distillQualitySnapshotManifestV1{}, err
	}
	outDir, err = validateFreshDistillQualityOutputDirV1(outDir, repoDir, storage.BrainDir)
	if err != nil {
		return distillQualitySnapshotManifestV1{}, err
	}
	privacyUnlock, err := acquireBrainPrivacySideEffectLock(storage.BrainDir)
	if err != nil {
		return distillQualitySnapshotManifestV1{}, err
	}
	defer privacyUnlock()

	manifest, err := loadBrainManifest(storage.BrainDir)
	if err != nil {
		return distillQualitySnapshotManifestV1{}, err
	}
	if manifest.Sources == nil || manifest.Sources.Sessions == nil {
		return distillQualitySnapshotManifestV1{}, errors.New("no exported sessions; run `entire brain refresh` first")
	}
	selection := distillCommandOptions{pipeline: distillPipelineCandidates, branch: strings.TrimSpace(branch), session: strings.TrimSpace(session)}
	sessions, err := filterTombstonedSessions(storage.BrainDir, append([]exportSession(nil), manifest.Sources.Sessions.Sessions...))
	if err != nil {
		return distillQualitySnapshotManifestV1{}, err
	}
	sessions, err = coalesceCandidateDistillSessions(ctx, storage.BrainDir, sessions, func(s exportSession) string { return resolveDistillBranch(manifest, s) }, func(s exportSession) bool { return distillSessionSelected(manifest, s, selection) })
	if err != nil {
		return distillQualitySnapshotManifestV1{}, err
	}
	selected := sessions[:0]
	for _, s := range sessions {
		if distillSessionSelected(manifest, s, selection) {
			selected = append(selected, s)
		}
	}
	sessions = selected
	sortDistillSessionsDeterministically(sessions, func(s exportSession) string { return resolveDistillBranch(manifest, s) })
	if len(sessions) == 0 {
		return distillQualitySnapshotManifestV1{}, errors.New("snapshot scope contains no selected sessions")
	}

	taxonomy, err := loadFactTaxonomy(storage.BrainDir, now)
	if err != nil {
		return distillQualitySnapshotManifestV1{}, err
	}
	taxonomyBytes, err := json.MarshalIndent(taxonomy, "", "  ")
	if err != nil {
		return distillQualitySnapshotManifestV1{}, err
	}
	taxonomyBytes = append(taxonomyBytes, '\n')
	var sources []distillQualitySnapshotSourceV1
	scopeHash := sha256.New()
	var sourceBytes int64
	zeroCandidates := 0
	for _, s := range sessions {
		data, readErr := readCanonicalHistoryTranscriptBounded(ctx, storage.BrainDir, s.TranscriptPath, distillCandidateMaxRawBytes)
		if readErr != nil {
			return distillQualitySnapshotManifestV1{}, readErr
		}
		if int64(len(data)) > maxSourceBytes-sourceBytes {
			return distillQualitySnapshotManifestV1{}, fmt.Errorf("snapshot selected source bytes exceed %d-byte aggregate bound; narrow the scope with --branch or --session", maxSourceBytes)
		}
		resolvedBranch := resolveDistillBranch(manifest, s)
		normalized := normalizeDistillTranscriptV1(s, resolvedBranch, string(data))
		if normalized.Overflow || normalized.Unsupported || !normalized.Recognized {
			return distillQualitySnapshotManifestV1{}, fmt.Errorf("snapshot cannot completely normalize session %q", s.SessionID)
		}
		cards, overflow := selectDistillCandidateCardsLimitedV1(normalized, distillCandidateMaxCards)
		if overflow {
			return distillQualitySnapshotManifestV1{}, fmt.Errorf("snapshot session %q exceeds candidate bound", s.SessionID)
		}
		if len(cards) == 0 {
			zeroCandidates++
		}
		digest := distillQualitySHA256V1(data)
		identityBytes, identityErr := distillQualitySnapshotSourceIdentityBytesV1(s, resolvedBranch, digest)
		if identityErr != nil {
			return distillQualitySnapshotManifestV1{}, identityErr
		}
		scopeHash.Write(identityBytes)
		scopeHash.Write([]byte{'\n'})
		sourceBytes += int64(len(data))
		sources = append(sources, distillQualitySnapshotSourceV1{session: s, branch: resolvedBranch, data: data, digest: digest})
	}

	armManifest := exportManifest{SchemaVersion: brainManifestSchemaVersion, GeneratedAt: now.UTC(), RepoKey: storage.Key, DefaultBranch: manifest.DefaultBranch, TranscriptMode: manifest.TranscriptMode, Scope: manifest.Scope, Sources: &brainSources{Sessions: &sessionSourceManifest{GeneratedAt: now.UTC(), EntireCLIVersion: manifest.Sources.Sessions.EntireCLIVersion, DefaultBranch: manifest.Sources.Sessions.DefaultBranch, TranscriptMode: manifest.Sources.Sessions.TranscriptMode, Scope: manifest.Sources.Sessions.Scope, Sessions: make([]exportSession, 0, len(sources))}}}
	for _, source := range sources {
		s := source.session
		s.Branch = source.branch
		armManifest.Sources.Sessions.Sessions = append(armManifest.Sources.Sessions.Sessions, s)
		armManifest.Sessions = append(armManifest.Sessions, s)
	}
	result := distillQualitySnapshotManifestV1{Contract: distillQualitySnapshotContractV1, SchemaVersion: 1, GeneratedAt: now.UTC().Format(time.RFC3339Nano), RepositoryKey: storage.Key, SourceScopeDigest: "sha256:" + hex.EncodeToString(scopeHash.Sum(nil)), TaxonomyDigest: distillCandidateCacheDigestV2(factTaxonomyBlock(taxonomy)), TaxonomySnapshotDigest: distillQualitySHA256V1(taxonomyBytes), SourceManifestVersion: brainManifestSchemaVersion, NormalizerVersion: "normalized-transcript-v1", SelectedSessions: len(sources), ZeroCandidateSessions: zeroCandidates, SourceBytes: sourceBytes, PrivateSourceSnapshot: true, ReferenceLabelsComplete: false, ReleaseGateEvidence: false}
	for _, name := range []string{"legacy", "candidate"} {
		root := filepath.Join(outDir, name)
		arm := distillQualitySnapshotArmV1{Name: name, ConfigDir: filepath.Join(root, "config"), DataDir: filepath.Join(root, "data"), StateDir: filepath.Join(root, "state"), CacheDir: filepath.Join(root, "cache")}
		arm.BrainDir = filepath.Join(arm.DataDir, repoStoreDirName, filepath.FromSlash(storage.Key))
		result.Arms = append(result.Arms, arm)
	}

	// All canonical inputs are open, bounded, normalized, and copied in memory
	// before the first output directory is created.
	if err := os.Mkdir(outDir, 0o700); err != nil {
		return distillQualitySnapshotManifestV1{}, err
	}
	for _, arm := range result.Arms {
		for _, dir := range []string{arm.ConfigDir, arm.DataDir, arm.StateDir, arm.CacheDir, arm.BrainDir} {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return distillQualitySnapshotManifestV1{}, err
			}
		}
		for _, source := range sources {
			if err := writeBrainRelativeFileAtomic(arm.BrainDir, source.session.TranscriptPath, source.data, 0o600); err != nil {
				return distillQualitySnapshotManifestV1{}, err
			}
		}
		if err := writeBrainRelativeFileAtomic(arm.BrainDir, factsTaxonomyPath, taxonomyBytes, 0o600); err != nil {
			return distillQualitySnapshotManifestV1{}, err
		}
		if err := writeBrainManifestAndReadme(arm.BrainDir, armManifest); err != nil {
			return distillQualitySnapshotManifestV1{}, err
		}
	}
	sort.Slice(result.Arms, func(i, j int) bool { return result.Arms[i].Name < result.Arms[j].Name })
	manifestBytes, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return distillQualitySnapshotManifestV1{}, err
	}
	if err := writeFileAtomic(filepath.Join(outDir, "manifest.json"), append(manifestBytes, '\n'), 0o600); err != nil {
		return distillQualitySnapshotManifestV1{}, err
	}
	return result, nil
}

func newFactsDistillQualitySnapshotCommand(opts Options) *cobra.Command {
	var out, branch, session string
	var jsonOut bool
	cmd := &cobra.Command{Use: "snapshot [path]", Short: "Fork a private selected-session source scope into paired empty Brain stores", Long: "Create local-only legacy and candidate plugin roots from identical selected canonical sessions and taxonomy. The snapshots are private and unlabeled; they are not release-gate evidence.", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		target := "."
		if opts.Env.RepoRoot != "" {
			target = opts.Env.RepoRoot
		}
		if len(args) == 1 {
			target = args[0]
		}
		result, err := buildDistillQualitySnapshotV1(cmd.Context(), opts, target, out, branch, session, opts.Now())
		if err != nil {
			return err
		}
		if jsonOut {
			return writeJSON(cmd, result)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "created private paired source snapshot for %d sessions at %s\n", result.SelectedSessions, out)
		return nil
	}}
	cmd.Flags().StringVar(&out, "out", "", "Fresh output directory outside repository and Brain (required)")
	cmd.Flags().StringVar(&branch, "branch", "", "Limit to one resolved branch")
	cmd.Flags().StringVar(&session, "session", "", "Limit to one session id")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit manifest as JSON")
	_ = cmd.MarkFlagRequired("out")
	return cmd
}
