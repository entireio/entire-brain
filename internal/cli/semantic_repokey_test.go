package cli

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// semanticProviderHeaderLine is one provider header carrying the repo key the
// provider chose, with the commit and tree this repository's HEAD really has.
func semanticProviderHeaderLine(t *testing.T, repoKey string) string {
	t.Helper()
	line, err := json.Marshal(semanticHeader{
		SchemaVersion: "1.0",
		Provider:      "entire-graph",
		RepoKey:       repoKey,
		Commit:        "c0ffee",
		Tree:          "7ee0",
	})
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	return string(line) + "\n"
}

// TestSetupOnANewGitRepoAcceptsTheProvidersRepoKey is the headline defect. A
// repository created with `git init` and no remote is named `local/<basename>`
// by entire-graph and `local/<basename>-<12hex>` by the brain, so the semantic
// component failed on EVERY brand-new repository — and the failure told the
// reader to add a git remote, which would not have helped.
//
// Reverting the two lines in scanSemanticStream that reconcile the two
// spellings still compiles, and fails here.
func TestSetupOnANewGitRepoAcceptsTheProvidersRepoKey(t *testing.T) {
	t.Parallel()
	repoDir := t.TempDir()
	brainKey := "local/" + filepath.Base(repoDir) + "-0123456789ab"
	providerKey := "local/" + filepath.Base(repoDir)

	out := &bytes.Buffer{}
	res, err := scanSemanticStream(
		strings.NewReader(semanticProviderHeaderLine(t, providerKey)),
		out,
		semanticStreamScanConfig{repoDir: repoDir, repoKey: brainKey},
	)
	if err != nil {
		t.Fatalf("scan stream: %v", err)
	}
	if res.header.RepoKey != brainKey {
		t.Fatalf("a snapshot the brain just built for this repo must be stored under the brain's own key: got %q, want %q", res.header.RepoKey, brainKey)
	}
	if res.providerRepoKey != providerKey {
		t.Fatalf("the provider's own spelling must survive for diagnostics: got %q, want %q", res.providerRepoKey, providerKey)
	}
	if !strings.Contains(out.String(), brainKey) {
		t.Fatalf("the snapshot WRITTEN to disk must carry the brain's key, or the next read rejects it:\n%s", out.String())
	}
	if err := validateLiveSemanticHeader(res.header, brainKey, "c0ffee", "7ee0", false); err != nil {
		t.Fatalf("a fresh `git init` repo must reach a green semantic component with no user action: %v", err)
	}
}

// TestSemanticAcceptsTheProviderKeyForANonGitHubRemote covers the rest of the
// same class: entire-graph recognises github.com and nothing else, so a GitLab
// or Bitbucket origin produced `local/<basename>` against the brain's
// `gl/owner/name` and failed identically.
func TestSemanticAcceptsTheProviderKeyForANonGitHubRemote(t *testing.T) {
	t.Parallel()
	repoDir := t.TempDir()
	brainKey := "gl/acme/" + filepath.Base(repoDir)

	res, err := scanSemanticStream(
		strings.NewReader(semanticProviderHeaderLine(t, "local/"+filepath.Base(repoDir))),
		&bytes.Buffer{},
		semanticStreamScanConfig{repoDir: repoDir, repoKey: brainKey},
	)
	if err != nil {
		t.Fatalf("scan stream: %v", err)
	}
	if res.header.RepoKey != brainKey {
		t.Fatalf("a GitLab-origin repo must not fail the semantic component: got %q, want %q", res.header.RepoKey, brainKey)
	}
}

// TestSemanticStillRejectsAnotherRepositorysSnapshot is the guard the
// relaxation must not cost. A provider key that is neither this repository's
// key nor the provider's own spelling of THIS directory is left untouched, so
// the header check still fails with the provider's key visible in the message.
func TestSemanticStillRejectsAnotherRepositorysSnapshot(t *testing.T) {
	t.Parallel()
	repoDir := t.TempDir()
	brainKey := "gh/acme/mine"
	foreign := "local/someone-elses-checkout"

	res, err := scanSemanticStream(
		strings.NewReader(semanticProviderHeaderLine(t, foreign)),
		&bytes.Buffer{},
		semanticStreamScanConfig{repoDir: repoDir, repoKey: brainKey},
	)
	if err != nil {
		t.Fatalf("scan stream: %v", err)
	}
	if res.header.RepoKey != foreign {
		t.Fatalf("a snapshot naming a different repository must keep its own key: got %q", res.header.RepoKey)
	}
	err = validateLiveSemanticHeader(res.header, brainKey, "c0ffee", "7ee0", false)
	if err == nil {
		t.Fatal("a snapshot naming a different repository must still be refused")
	}
	if !strings.Contains(err.Error(), foreign) {
		t.Fatalf("the refusal must name what the provider actually said: %v", err)
	}
}

// TestSemanticRepoKeyReconciliationIsOptIn pins that a caller which supplies no
// repo key (the fuzzer, the stream unit tests) gets the provider's header
// unmodified. Silently rewriting a key nobody asked about would make the
// bundle-import boundary — where the key is the ONLY provenance — depend on
// which caller happened to build the stream.
func TestSemanticRepoKeyReconciliationIsOptIn(t *testing.T) {
	t.Parallel()
	res, err := scanSemanticStream(
		strings.NewReader(semanticProviderHeaderLine(t, "local/whatever")),
		&bytes.Buffer{},
		semanticStreamScanConfig{repoDir: t.TempDir()},
	)
	if err != nil {
		t.Fatalf("scan stream: %v", err)
	}
	if res.header.RepoKey != "local/whatever" {
		t.Fatalf("with no repo key configured the header must be untouched: got %q", res.header.RepoKey)
	}
}

func TestSemanticProviderLocalRepoKeyMirrorsTheProvider(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "myproject")
	if got, want := semanticProviderLocalRepoKey(dir), "local/myproject"; got != want {
		t.Fatalf("brain must mirror entire-graph's `\"local/\" + filepath.Base(repo)`: got %q, want %q", got, want)
	}
	if got := semanticProviderLocalRepoKey(""); got != "" {
		t.Fatalf("no repo dir means no provider key to recognise, got %q", got)
	}
}
