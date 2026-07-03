package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ashtom/entire-brain/internal/brainwire"
)

// Hosted brain publish (P1.M1.5, client half). This command is the ONLY path in
// the plugin allowed to egress brain data, and only behind an explicit opt-in.
//
// Two-gate opt-in (ADR-P1-A, no implicit egress): a network call happens only
// when BOTH gates are satisfied — (1) the user explicitly runs
// `entire brain publish`, and (2) ENTIRE_BRAIN_ALLOW_HOSTED is truthy. Either
// gate unsatisfied refuses BEFORE any resolution, disk read, or HTTP request, so
// the default (no gate) stays strictly local-only. The master local-only switch
// (ENTIRE_BRAIN_NO_EGRESS / ENTIRE_BRAIN_LOCAL_ONLY) always wins over the hosted
// opt-in.
const (
	// envBrainAllowHosted is the explicit opt-in gate for hosted publishing.
	envBrainAllowHosted = "ENTIRE_BRAIN_ALLOW_HOSTED"

	// The hosted target is resolved from these env vars (each overridable by a
	// flag). There is no pre-existing entire-api client in this plugin, so these
	// follow the plugin's ENTIRE_* environment convention. Auth is a bearer token,
	// matching the entire-api brain publish endpoint's push gate.
	envAPIBaseURL = "ENTIRE_API_URL"
	envAPIToken   = "ENTIRE_API_TOKEN"
	envRepoID     = "ENTIRE_REPO_ID"
)

const (
	// Artifact kinds, matching the server's brainstore.Kind* constants exactly.
	brainKindSnapshot = "snapshot"
	brainKindOverlay  = "overlay"
	brainKindFacts    = "facts"
	brainKindManifest = "manifest"

	mediaTypeJSON   = "application/json"
	mediaTypeNDJSON = "application/x-ndjson"

	// publishAPIPath is the route on entire-api: POST
	// {base}/api/v1/repos/{repo_id}/brain/artifacts.
	publishAPIPathPrefix = "/api/v1/repos/"
	publishAPIPathSuffix = "/brain/artifacts"

	publishRequestTimeout   = 5 * time.Minute
	maxPublishResponseBytes = 1 << 20 // 1 MiB is ample for the small JSON result.
)

// publishArtifact is one artifact in the publish bundle. Its JSON tags mirror the
// server's httpapi.BrainArtifactInput exactly: kind, ref, digest (omitempty), and
// data. Go marshals a []byte field as base64, which is exactly what the server's
// []byte Data field expects on the wire.
type publishArtifact struct {
	Kind   string `json:"kind"`
	Ref    string `json:"ref"`
	Digest string `json:"digest,omitempty"`
	Data   []byte `json:"data"`
}

// publishRequestBody is the POST body: {"artifacts":[...]}, matching the server's
// httpapi.BrainPublishInput.Body.
type publishRequestBody struct {
	Artifacts []publishArtifact `json:"artifacts"`
}

// publishResult mirrors the server's httpapi.BrainPublishOutput body.
type publishResult struct {
	Status string `json:"status"`
	Stored []struct {
		Kind string `json:"kind"`
		Ref  string `json:"ref"`
	} `json:"stored"`
}

type publishCommandOptions struct {
	repoID string
	apiURL string
	token  string
}

func newPublishCommand(opts Options) *cobra.Command {
	publishOpts := publishCommandOptions{}

	cmd := &cobra.Command{
		Use:   "publish",
		Short: "Publish the local brain to hosted Entire (opt-in)",
		Long: `Publish serializes the current on-disk brain into the versioned brain wire
artifacts (manifest, semantic snapshots, branch overlays, and durable facts) and
uploads them to hosted Entire.

Hosted publishing is opt-in and off by default: the brain stays entirely local
unless you BOTH run this command AND set ENTIRE_BRAIN_ALLOW_HOSTED=1. No brain
data ever leaves your machine implicitly.

The target repo, API base URL, and bearer token are resolved from --repo-id /
ENTIRE_REPO_ID, --api-url / ENTIRE_API_URL, and --token / ENTIRE_API_TOKEN.
Re-running publish is safe: the server overwrites each artifact at its
coordinate, so identical bytes produce an identical result.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPublish(cmd.Context(), cmd, opts, publishOpts)
		},
	}

	cmd.Flags().StringVar(&publishOpts.repoID, "repo-id", "", "Target repo ULID (overrides ENTIRE_REPO_ID)")
	cmd.Flags().StringVar(&publishOpts.apiURL, "api-url", "", "Entire API base URL (overrides ENTIRE_API_URL)")
	cmd.Flags().StringVar(&publishOpts.token, "token", "", "Entire API bearer token (overrides ENTIRE_API_TOKEN)")

	return cmd
}

func runPublish(ctx context.Context, cmd *cobra.Command, opts Options, publishOpts publishCommandOptions) error {
	// --- Two-gate opt-in (ADR-P1-A). Both gates are checked BEFORE resolving any
	// target, reading the brain, or making a network call, so a refusal is
	// guaranteed egress-free. Gate 1 (explicit invocation) is implicit here.

	// The master local-only switch always wins over the hosted opt-in.
	if brainNoEgressMode() {
		return fmt.Errorf("no_egress: hosted brain publish is disabled while ENTIRE_BRAIN_NO_EGRESS or ENTIRE_BRAIN_LOCAL_ONLY is set; unset it to publish")
	}
	// Gate 2: the explicit hosted opt-in must be truthy. Unset/empty/false refuses
	// with no network call — the client-side no-implicit-egress guarantee. This is
	// deliberately NOT securityToggleEnabled (which fails a garbage value ON): a
	// publish opt-in must fail CLOSED, so anything not clearly truthy stays local.
	if !envBool(envBrainAllowHosted) {
		return fmt.Errorf("hosted_publish_disabled: hosted brain publishing is opt-in and off by default; nothing was sent. Set %s=1 and re-run 'entire brain publish' to enable it", envBrainAllowHosted)
	}

	// Resolve the target (flag overrides env). Fail clearly before touching git or
	// disk so a misconfiguration never produces a partial request.
	repoID := publishFlagOrEnv(publishOpts.repoID, envRepoID)
	if repoID == "" {
		return fmt.Errorf("publish: target repo id is required; set --repo-id or %s", envRepoID)
	}
	baseURL := publishFlagOrEnv(publishOpts.apiURL, envAPIBaseURL)
	if baseURL == "" {
		return fmt.Errorf("publish: API base URL is required; set --api-url or %s", envAPIBaseURL)
	}
	token := publishFlagOrEnv(publishOpts.token, envAPIToken)
	if token == "" {
		return fmt.Errorf("publish: API token is required; set --token or %s", envAPIToken)
	}

	// Locate the on-disk brain for this repo.
	repoDir := opts.Env.RepoRoot
	if repoDir == "" {
		repoDir = "."
	}
	storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return fmt.Errorf("publish: resolve brain: %w", err)
	}
	if !brainExportExists(storage.BrainDir) {
		return fmt.Errorf("publish: no brain found for this repo; run 'entire brain refresh' first")
	}

	body, repoKey, err := buildPublishBundle(ctx, opts, storage, repoDir)
	if err != nil {
		return fmt.Errorf("publish: %w", err)
	}

	result, err := postBrainArtifacts(ctx, baseURL, repoID, token, body)
	if err != nil {
		return err
	}

	fmt.Fprintf(cmd.OutOrStdout(), "published %d artifact(s) for %s to %s\n",
		len(result.Stored), repoKey, strings.TrimRight(baseURL, "/"))
	return nil
}

// publishFlagOrEnv returns the trimmed flag value when set, else the trimmed
// environment value.
func publishFlagOrEnv(flagVal, envName string) string {
	if v := strings.TrimSpace(flagVal); v != "" {
		return v
	}
	return strings.TrimSpace(os.Getenv(envName))
}

// buildPublishBundle serializes the on-disk brain into the publish bundle: the
// manifest (kind=manifest, its data is the full brainwire.BrainArtifact JSON with
// content-addressed references) plus each snapshot/overlay/facts blob inline. It
// returns the bundle and the repo_key it declares.
func buildPublishBundle(ctx context.Context, opts Options, storage repoStorage, repoDir string) (publishRequestBody, string, error) {
	manifest, err := loadBrainManifest(storage.BrainDir)
	if err != nil {
		return publishRequestBody{}, "", fmt.Errorf("load brain manifest: %w", err)
	}
	repoKey := strings.TrimSpace(manifest.RepoKey)
	if repoKey == "" {
		// The deterministic store key is the wire slug; fall back to it so the
		// manifest always self-declares a repo_key.
		repoKey = storage.Key
	}

	art := brainwire.NewBrainArtifact(repoKey, manifest.DefaultBranch, manifest.GeneratedAt)
	if sem := semanticSourceOf(manifest); sem != nil {
		art.SetProvider(sem.Provider, sem.ProviderVersion, sem.SchemaVersion)
	}

	// Collect the heavy blobs (each also registers a content-addressed reference on
	// the wire manifest). Order within each kind is stable so re-publishing an
	// unchanged brain produces a byte-identical request (idempotent).
	var blobs []publishArtifact
	snapshots, err := collectSnapshotArtifacts(art, storage.BrainDir)
	if err != nil {
		return publishRequestBody{}, "", err
	}
	blobs = append(blobs, snapshots...)
	overlays, err := collectOverlayArtifacts(art, storage.BrainDir)
	if err != nil {
		return publishRequestBody{}, "", err
	}
	blobs = append(blobs, overlays...)
	facts, err := collectFactArtifacts(art, storage.BrainDir)
	if err != nil {
		return publishRequestBody{}, "", err
	}
	blobs = append(blobs, facts...)

	// The manifest artifact is the fully-built wire artifact (manifest header +
	// all references), addressed by the repo's HEAD commit (the store addresses a
	// manifest by commit).
	manifestData, err := json.Marshal(art)
	if err != nil {
		return publishRequestBody{}, "", fmt.Errorf("encode manifest: %w", err)
	}
	manifestArtifact := publishArtifact{
		Kind:   brainKindManifest,
		Ref:    publishManifestRef(ctx, opts, repoDir, manifest),
		Digest: contentDigest(manifestData),
		Data:   manifestData,
	}

	body := publishRequestBody{
		Artifacts: append([]publishArtifact{manifestArtifact}, blobs...),
	}
	return body, repoKey, nil
}

// collectSnapshotArtifacts reads semantic/snapshots/<commit>/snapshot.ndjson,
// keying each by its commit, and registers a SnapshotRef on art.
func collectSnapshotArtifacts(art *brainwire.BrainArtifact, brainDir string) ([]publishArtifact, error) {
	root := filepath.Join(brainDir, semanticDirName, semanticSnapshotsDir)
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read snapshots: %w", err)
	}
	commits := subdirNames(entries)
	sort.Strings(commits)

	var out []publishArtifact
	for _, commit := range commits {
		rel := filepath.ToSlash(filepath.Join(semanticDirName, semanticSnapshotsDir, commit, semanticSnapshotName))
		data, ok, err := readBrainBlob(filepath.Join(brainDir, filepath.FromSlash(rel)))
		if err != nil {
			return nil, fmt.Errorf("read snapshot %s: %w", commit, err)
		}
		if !ok {
			continue
		}
		digest := contentDigest(data)
		art.AddSnapshot(brainwire.SnapshotRef{
			Commit:  commit,
			Content: brainwire.ContentRef{Digest: digest, Size: int64(len(data)), MediaType: mediaTypeNDJSON, Path: rel},
		})
		out = append(out, publishArtifact{Kind: brainKindSnapshot, Ref: commit, Digest: digest, Data: data})
	}
	return out, nil
}

// collectOverlayArtifacts reads semantic/overlays/<base>..<head>.json, keying
// each by "base..head", and registers an OverlayRef on art. The all-branches.json
// refresh report is not a (base,head) overlay and is skipped.
func collectOverlayArtifacts(art *brainwire.BrainArtifact, brainDir string) ([]publishArtifact, error) {
	root := filepath.Join(brainDir, semanticDirName, "overlays")
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read overlays: %w", err)
	}

	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		ref := strings.TrimSuffix(name, ".json")
		// Only (base..head)-addressed overlays are shareable references; skip the
		// aggregate all-branches.json report and any non-.json file.
		if name == ref || !strings.Contains(ref, "..") {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)

	var out []publishArtifact
	for _, name := range names {
		ref := strings.TrimSuffix(name, ".json")
		base, head, _ := strings.Cut(ref, "..")
		rel := filepath.ToSlash(filepath.Join(semanticDirName, "overlays", name))
		data, ok, err := readBrainBlob(filepath.Join(brainDir, filepath.FromSlash(rel)))
		if err != nil {
			return nil, fmt.Errorf("read overlay %s: %w", ref, err)
		}
		if !ok {
			continue
		}
		digest := contentDigest(data)
		art.AddOverlay(brainwire.OverlayRef{
			BaseCommit: base,
			HeadCommit: head,
			Branch:     overlayBranchName(data),
			Content:    brainwire.ContentRef{Digest: digest, Size: int64(len(data)), MediaType: mediaTypeJSON, Path: rel},
		})
		out = append(out, publishArtifact{Kind: brainKindOverlay, Ref: ref, Digest: digest, Data: data})
	}
	return out, nil
}

// collectFactArtifacts reads facts/<slug>/facts.ndjson, recovering each stream's
// logical branch from the records (the on-disk slug is lossy), and registers a
// FactsRef on art. Streams are keyed by branch and emitted in branch order.
func collectFactArtifacts(art *brainwire.BrainArtifact, brainDir string) ([]publishArtifact, error) {
	root := filepath.Join(brainDir, factsDirName)
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read facts: %w", err)
	}

	dirs := subdirNames(entries)
	sort.Strings(dirs)

	type factStream struct {
		branch string
		data   []byte
	}
	var streams []factStream
	seen := make(map[string]struct{})
	for _, dir := range dirs {
		data, ok, err := readBrainBlob(filepath.Join(root, dir, factsFileName))
		if err != nil {
			return nil, fmt.Errorf("read facts %s: %w", dir, err)
		}
		if !ok {
			continue
		}
		branch := firstFactBranch(data)
		if branch == "" {
			continue
		}
		if _, dup := seen[branch]; dup {
			// Two on-disk streams collapsing to one branch would be a duplicate
			// coordinate the server rejects; keep the first deterministically.
			continue
		}
		seen[branch] = struct{}{}
		streams = append(streams, factStream{branch: branch, data: data})
	}
	sort.Slice(streams, func(i, j int) bool { return streams[i].branch < streams[j].branch })

	var out []publishArtifact
	for _, stream := range streams {
		rel := factsFileRelPath(stream.branch)
		digest := contentDigest(stream.data)
		art.AddFacts(brainwire.FactsRef{
			Branch:  stream.branch,
			Content: brainwire.ContentRef{Digest: digest, Size: int64(len(stream.data)), MediaType: mediaTypeNDJSON, Path: rel},
		})
		out = append(out, publishArtifact{Kind: brainKindFacts, Ref: stream.branch, Digest: digest, Data: stream.data})
	}
	return out, nil
}

// publishManifestRef addresses the manifest by the repo HEAD commit (the store
// addresses a manifest by commit), falling back to the default branch, then a
// stable literal, when HEAD is not resolvable.
func publishManifestRef(ctx context.Context, opts Options, repoDir string, manifest *exportManifest) string {
	if head := strings.TrimSpace(string(runGitOutput(ctx, opts.Runner, repoDir, "rev-parse", "HEAD"))); head != "" {
		return head
	}
	if manifest != nil {
		if branch := strings.TrimSpace(manifest.DefaultBranch); branch != "" {
			return branch
		}
	}
	return brainKindManifest
}

// postBrainArtifacts POSTs the bundle to the hosted brain publish endpoint and
// maps the response to a clear error or result. It is the only network call in
// the command; it runs only after both opt-in gates have passed.
func postBrainArtifacts(ctx context.Context, baseURL, repoID, token string, body publishRequestBody) (publishResult, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return publishResult{}, fmt.Errorf("publish: encode request: %w", err)
	}
	endpoint := strings.TrimRight(baseURL, "/") + publishAPIPathPrefix + url.PathEscape(repoID) + publishAPIPathSuffix

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return publishResult{}, fmt.Errorf("publish: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{Timeout: publishRequestTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return publishResult{}, fmt.Errorf("publish: request to %s failed: %w", endpoint, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxPublishResponseBytes))

	switch resp.StatusCode {
	case http.StatusOK:
		var result publishResult
		if err := json.Unmarshal(respBody, &result); err != nil {
			return publishResult{}, fmt.Errorf("publish: decode server response: %w", err)
		}
		return result, nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return publishResult{}, fmt.Errorf("publish_auth: authentication or push authorization failed (HTTP %d)%s", resp.StatusCode, publishServerDetail(respBody))
	case http.StatusUnprocessableEntity:
		return publishResult{}, fmt.Errorf("publish_rejected: server rejected the brain artifacts (HTTP 422)%s", publishServerDetail(respBody))
	case http.StatusServiceUnavailable:
		return publishResult{}, fmt.Errorf("publish_unavailable: hosted brain publishing is not configured on the server (HTTP 503)")
	default:
		return publishResult{}, fmt.Errorf("publish_failed: unexpected server response (HTTP %d)%s", resp.StatusCode, publishServerDetail(respBody))
	}
}

// publishServerDetail extracts a short human-readable reason from a server error
// body (huma emits {"title","detail",...}), falling back to a bounded raw
// snippet, so a failure surfaces the server's message without dumping the body.
func publishServerDetail(body []byte) string {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return ""
	}
	var env struct {
		Detail string `json:"detail"`
		Title  string `json:"title"`
	}
	if json.Unmarshal(body, &env) == nil {
		if env.Detail != "" {
			return ": " + env.Detail
		}
		if env.Title != "" {
			return ": " + env.Title
		}
	}
	if len(trimmed) > 200 {
		trimmed = trimmed[:200]
	}
	return ": " + trimmed
}

// contentDigest returns the "sha256:"-prefixed lowercase-hex digest of data, the
// exact form the wire contract's ContentRef.Digest and the server's VerifyDigest
// expect.
func contentDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func semanticSourceOf(manifest *exportManifest) *semanticSourceManifest {
	if manifest == nil || manifest.Sources == nil {
		return nil
	}
	return manifest.Sources.Semantic
}

// readBrainBlob reads a brain payload file, returning ok=false (not an error) for
// a missing file or a non-regular entry (a symlink is not followed).
func readBrainBlob(path string) ([]byte, bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if !info.Mode().IsRegular() {
		return nil, false, nil
	}
	data, err := safeReadFile(path, semanticSnapshotMaxBytes())
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

func subdirNames(entries []os.DirEntry) []string {
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	return names
}

// firstFactBranch recovers the logical branch from a facts.ndjson stream by
// reading the branch of its first record. It returns "" when no branch can be
// read (advisory recovery, so a parse error is not fatal to the caller).
func firstFactBranch(data []byte) string {
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var record struct {
			Branch string `json:"branch"`
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			return ""
		}
		return strings.TrimSpace(record.Branch)
	}
	return ""
}

// overlayBranchName reads the advisory "branch" field from an overlay payload.
func overlayBranchName(data []byte) string {
	var payload struct {
		Branch string `json:"branch"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return ""
	}
	return strings.TrimSpace(payload.Branch)
}
