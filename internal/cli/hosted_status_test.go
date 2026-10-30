package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/base"

	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
)

// hostedStatusFixture builds the smallest brain whose status report can carry
// hosted state: a manifest declaring two fact branches and nothing else.
func hostedStatusFixture(t *testing.T) (Options, string, string) {
	t.Helper()
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	env.RepoRoot = repoDir
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	now := time.Date(2026, 10, 14, 2, 0, 0, 0, time.UTC)
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time { return now }}
	storage, err := repoStoragePaths(context.Background(), runner, env, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(storage.BrainDir, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := exportManifest{
		SchemaVersion: bmanifest.SchemaVersion,
		RepoKey:       storage.Key,
		GeneratedAt:   now,
		Sources: &brainSources{
			Facts: &factSourceManifest{GeneratedAt: now, Branches: []string{"feature", "main"}},
		},
	}
	if err := writeBrainManifestAndReadme(storage.BrainDir, manifest); err != nil {
		t.Fatal(err)
	}
	return opts, repoDir, storage.BrainDir
}

func TestStatusHostedDisconnectedWithoutBinding(t *testing.T) {
	opts, repoDir, _ := hostedStatusFixture(t)
	report, err := buildAvailableBrainStatusReport(context.Background(), opts, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	if report.Hosted == nil {
		t.Fatal("status must always report hosted state")
	}
	if report.Hosted.State != "disconnected" || report.Hosted.RepoState != "disconnected" {
		t.Fatalf("state = %q/%q, want disconnected: %+v", report.Hosted.State, report.Hosted.RepoState, report.Hosted)
	}
	if report.Hosted.RepoID != "" || report.Hosted.BaseURL != "" {
		t.Fatalf("never-connected must not carry a target: %+v", report.Hosted)
	}

	out, err := execute(t, NewRootCommand(opts), "status")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Brainstore | disconnected") {
		t.Fatalf("short status does not show hosted state:\n%s", out)
	}
}

func TestStatusHostedConnectedListsSyncedBranches(t *testing.T) {
	opts, repoDir, brainDir := hostedStatusFixture(t)
	const repoID = "01M2Q9WATHXVM46CD8Y4D3AX8W"
	const baseURL = "https://cell.api.example.com"
	if err := brainstore.WriteHostedRepoBinding(brainDir, brainstore.HostedRepoBinding{RepoID: repoID, BaseURL: baseURL}); err != nil {
		t.Fatal(err)
	}
	// main synced against this target; feature never synced; a stale marker
	// pointing at another target must not count as synced.
	if err := writeHostedFactsBinding(brainDir, "main", repoID, baseURL); err != nil {
		t.Fatal(err)
	}
	if err := writeHostedFactsBinding(brainDir, "feature", "01OTHERREPOID0000000000000", "https://other.example.com"); err != nil {
		t.Fatal(err)
	}

	report, err := buildAvailableBrainStatusReport(context.Background(), opts, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	h := report.Hosted
	if h == nil || h.State != "connected" {
		t.Fatalf("state = %+v, want connected", h)
	}
	if h.RepoID != repoID || h.BaseURL != baseURL {
		t.Fatalf("target mismatch: %+v", h)
	}
	if len(h.SyncedBranches) != 1 || h.SyncedBranches[0] != "main" {
		t.Fatalf("synced branches = %v, want [main]", h.SyncedBranches)
	}

	short, err := execute(t, NewRootCommand(opts), "status")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, short)
	}
	if !strings.Contains(short, "Brainstore | connected") {
		t.Fatalf("short status missing the Brainstore item:\n%s", short)
	}

	out, err := execute(t, NewRootCommand(opts), "status", "--verbose")
	if err != nil {
		t.Fatalf("status --verbose: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Connected to Brainstore (1 branch synced)") {
		t.Fatalf("verbose status missing the Brainstore line:\n%s", out)
	}
	// Coordinates are machine detail: --json carries them, humans don't.
	if strings.Contains(out, repoID) {
		t.Fatalf("human output must not show the repo id:\n%s", out)
	}
}

func TestStatusHostedLocalOnlyByKillSwitch(t *testing.T) {
	opts, repoDir, brainDir := hostedStatusFixture(t)
	if err := brainstore.WriteHostedRepoBinding(brainDir, brainstore.HostedRepoBinding{RepoID: "01M2Q9WATHXVM46CD8Y4D3AX8W", BaseURL: "https://cell.api.example.com"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "1")

	report, err := buildAvailableBrainStatusReport(context.Background(), opts, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	h := report.Hosted
	if h == nil || h.State != "local_only" || !h.NoEgress {
		t.Fatalf("state = %+v, want local_only with no_egress", h)
	}
	// The repo state and binding stay visible so the reader sees what the
	// switch suppresses and what unsetting it restores.
	if h.RepoState != "connected" || h.RepoID == "" || h.BaseURL == "" {
		t.Fatalf("local-only must keep the suppressed repo state and target visible: %+v", h)
	}

	out, err := execute(t, NewRootCommand(opts), "status")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Brainstore | local-only") {
		t.Fatalf("short status does not show local-only:\n%s", out)
	}
	verbose, err := execute(t, NewRootCommand(opts), "status", "--verbose")
	if err != nil {
		t.Fatalf("status --verbose: %v\n%s", err, verbose)
	}
	if !strings.Contains(verbose, "ENTIRE_BRAIN_NO_EGRESS set") || !strings.Contains(verbose, "repo is connected") {
		t.Fatalf("verbose status must name the kill switch and the underlying repo state:\n%s", verbose)
	}
}

func TestStatusHostedCorruptBranchMarkerWarnsInsteadOfNeverSynced(t *testing.T) {
	opts, repoDir, brainDir := hostedStatusFixture(t)
	const repoID = "01M2Q9WATHXVM46CD8Y4D3AX8W"
	const baseURL = "https://cell.api.example.com"
	if err := brainstore.WriteHostedRepoBinding(brainDir, brainstore.HostedRepoBinding{RepoID: repoID, BaseURL: baseURL}); err != nil {
		t.Fatal(err)
	}
	if err := writeHostedFactsBinding(brainDir, "feature", repoID, baseURL); err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(brainDir, filepath.FromSlash(hostedFactsBindingPath("main")))
	if err := os.MkdirAll(filepath.Dir(markerPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(markerPath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	report, err := buildAvailableBrainStatusReport(context.Background(), opts, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	h := report.Hosted
	if h == nil || h.State != "connected" {
		t.Fatalf("state = %+v, want connected", h)
	}
	if len(h.SyncedBranches) != 1 || h.SyncedBranches[0] != "feature" {
		t.Fatalf("synced branches = %v, want [feature]", h.SyncedBranches)
	}
	found := false
	for _, w := range report.Warnings {
		if strings.Contains(w, "main") && strings.Contains(w, "marker") {
			found = true
		}
	}
	if !found {
		t.Fatalf("a corrupt branch marker must warn, not pose as never-synced: %v", report.Warnings)
	}
}

func TestStatusHostedDisconnectedByOptOut(t *testing.T) {
	opts, repoDir, brainDir := hostedStatusFixture(t)
	const repoID = "01M2Q9WATHXVM46CD8Y4D3AX8W"
	if err := brainstore.WriteHostedRepoBinding(brainDir, brainstore.HostedRepoBinding{RepoID: repoID, BaseURL: "https://cell.api.example.com", OptedOut: true}); err != nil {
		t.Fatal(err)
	}

	report, err := buildAvailableBrainStatusReport(context.Background(), opts, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	h := report.Hosted
	if h == nil || h.State != "disconnected" {
		t.Fatalf("opted-out binding must report disconnected, got %+v", h)
	}
	// Coordinates survive an opt-out (reconnect works without placement
	// resolution), so status keeps them visible too.
	if h.RepoID != repoID {
		t.Fatalf("opted-out state must keep the target visible: %+v", h)
	}

	out, err := execute(t, NewRootCommand(opts), "status")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Brainstore | disconnected") {
		t.Fatalf("short status does not show the disconnect:\n%s", out)
	}
	verbose, err := execute(t, NewRootCommand(opts), "status", "--verbose")
	if err != nil {
		t.Fatalf("status --verbose: %v\n%s", err, verbose)
	}
	if !strings.Contains(verbose, "run '"+base.BrainInvocation(os.LookupEnv, "connect")+"' to join") {
		t.Fatalf("verbose status must name the rejoin remedy:\n%s", verbose)
	}
	if strings.Contains(verbose, "ENTIRE_BRAIN_NO_EGRESS") {
		t.Fatalf("an opt-out must not be blamed on the kill switch:\n%s", verbose)
	}
}

func TestStatusHostedCorruptBindingFallsBackWithWarning(t *testing.T) {
	opts, repoDir, brainDir := hostedStatusFixture(t)
	if err := os.WriteFile(filepath.Join(brainDir, brainstore.HostedRepoBindingFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := buildAvailableBrainStatusReport(context.Background(), opts, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	if report.Hosted == nil || report.Hosted.State != "disconnected" {
		t.Fatalf("corrupt binding must fall back to disconnected: %+v", report.Hosted)
	}
	found := false
	for _, w := range report.Warnings {
		if strings.Contains(w, "hosted") {
			found = true
		}
	}
	if !found {
		t.Fatalf("corrupt binding must surface a warning: %v", report.Warnings)
	}

	// Sync never self-heals a binding it cannot read, so status must not
	// promise auto-connect here; the remedy is an explicit connect.
	out, err := execute(t, NewRootCommand(opts), "status", "--verbose")
	if err != nil {
		t.Fatalf("status --verbose: %v\n%s", err, out)
	}
	if strings.Contains(out, "auto-connect") {
		t.Fatalf("unreadable binding must not promise auto-connect:\n%s", out)
	}
}
