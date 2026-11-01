package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/historystore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
)

const finalizedMarker = "finalizedmarkerword"

// finalizeCheckpointTranscript does what a "Finalize transcript" commit does:
// it rewrites an existing checkpoint's transcript on the checkpoint ref, under
// the same checkpoint id, with nothing in the Brain changing.
func finalizeCheckpointTranscript(t *testing.T, repo string) string {
	t.Helper()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	finalized := conversationClaudeFixture + `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"` + finalizedMarker + `"}]}}` + "\n" +
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Decision: keep ` + finalizedMarker + `"}]}}` + "\n"
	path := filepath.Join(repo, filepath.FromSlash(checkpointPath("aaa111aaa111")), "0", "full.jsonl")
	if err := os.WriteFile(path, []byte(finalized), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("-c", "commit.gpgsign=false", "commit", "-qm", "Finalize transcript for Checkpoint: aaa111aaa111")
	git("update-ref", v1MainRef, "HEAD")
	return finalized
}

func transcriptTestDigest(data string) string {
	sum := sha256.Sum256([]byte(data))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// A finalize landing anywhere in a rebuild must not fail it, and whatever gets
// published must describe one version of the transcript: the index and the
// receipts attesting to it cannot disagree about which bytes were processed.
func TestHistoryRebuildSurvivesCheckpointFinalize(t *testing.T) {
	windows := map[string]func(fire func()) (historyIndexProgress, func()){
		"after the build read the transcript, before receipts": func(fire func()) (historyIndexProgress, func()) {
			return func(done, total int) {
				if total > 0 && done == total {
					fire()
				}
			}, func() {}
		},
		"before the post-scan identity check": func(fire func()) (historyIndexProgress, func()) {
			original := beforeHistoryProjectionContentDigest
			beforeHistoryProjectionContentDigest = func(string) { fire() }
			return nil, func() { beforeHistoryProjectionContentDigest = original }
		},
	}
	for name, window := range windows {
		t.Run(name, func(t *testing.T) {
			opts, brain, repo := filelessCheckpointFixture(t)
			runFilelessExport(t, opts, "", true)
			finalized := ""
			progress, restore := window(func() {
				if finalized == "" {
					finalized = finalizeCheckpointTranscript(t, repo)
				}
			})
			defer restore()

			now := opts.Now().Add(time.Minute)
			prepared, err := prepareBrainHistoryProjection(brain, now, progress)
			if err != nil {
				t.Fatalf("rebuild lost to a checkpoint finalize: %v", err)
			}
			restore()
			if finalized == "" {
				t.Fatal("the finalize never fired, so this window proves nothing")
			}
			if strings.Contains(mustMarshalIndex(t, prepared.index), finalizedMarker) {
				t.Fatal("index contains finalized bytes the rebuild's identity does not describe")
			}
			if _, err := publishBrainHistoryProjectionLocked(brain, prepared, nil); err != nil {
				t.Fatalf("publish: %v", err)
			}
			assertReceiptDigest(t, brain, transcriptTestDigest(conversationClaudeFixture))

			// Nothing is lost, exactly as if the finalize had landed just after
			// publish: the index reads as stale, the next delta serves the new
			// bytes, and the next rebuild absorbs them.
			manifest, err := bmanifest.Load(brain)
			if err != nil {
				t.Fatal(err)
			}
			if historyIndexCurrent(brain, manifest) {
				t.Fatal("index reads as current after its transcript was finalized")
			}
			if _, err := buildHistoryShortTermLocked(brain, now.Add(time.Minute)); err != nil {
				t.Fatalf("delta: %v", err)
			}
			overlay, _ := loadHistoryShortTermRaw(brain)
			overlayData, err := json.Marshal(overlay)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(overlayData), finalizedMarker) {
				t.Fatal("the next delta did not pick up the finalized transcript")
			}
			if _, err := writeBrainHistoryIndexAndSourceLocked(brain, now.Add(2*time.Minute), nil); err != nil {
				t.Fatalf("next rebuild: %v", err)
			}
			assertReceiptDigest(t, brain, transcriptTestDigest(finalized))
			manifest, err = bmanifest.Load(brain)
			if err != nil {
				t.Fatal(err)
			}
			index, err := historystore.LoadIndex(brain, manifest.Sources.History)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(mustMarshalIndex(t, index), finalizedMarker) {
				t.Fatal("the next rebuild did not pick up the finalized transcript")
			}
		})
	}
}

// Pinning belongs to one rebuild. Long-lived stores must keep following the ref,
// or a running watcher would serve superseded transcripts indefinitely.
func TestTranscriptStorePinningIsOptIn(t *testing.T) {
	opts, brain, repo := filelessCheckpointFixture(t)
	runFilelessExport(t, opts, "", true)
	manifest, err := bmanifest.Load(brain)
	if err != nil {
		t.Fatal(err)
	}
	rel := manifest.Sources.Sessions.Sessions[0].TranscriptPath
	ctx := context.Background()
	pinned := newPinnedTranscriptStore(ExecRunner{}, brain)
	following := newTranscriptStore(ExecRunner{}, brain)
	for _, store := range []*transcriptStore{pinned, following} {
		if data, err := store.Read(ctx, rel); err != nil || string(data) != conversationClaudeFixture {
			t.Fatalf("first read: %v %q", err, data)
		}
	}
	finalized := finalizeCheckpointTranscript(t, repo)
	if data, err := pinned.Read(ctx, rel); err != nil || string(data) != conversationClaudeFixture {
		t.Fatalf("pinned store followed the moved ref: %v %q", err, data)
	}
	if data, err := following.Read(ctx, rel); err != nil || string(data) != finalized {
		t.Fatalf("unpinned store stopped following the ref: %v %q", err, data)
	}
}

func mustMarshalIndex(t *testing.T, index historyIndex) string {
	t.Helper()
	data, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func assertReceiptDigest(t *testing.T, brain, want string) {
	t.Helper()
	manifest, err := bmanifest.Load(brain)
	if err != nil {
		t.Fatal(err)
	}
	state, _, err := loadProjectionStateChecked(brain, manifest.Sources.History)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Sessions) != 1 || state.Sessions[0].InputDigest != want {
		t.Fatalf("receipts = %+v, want one with input digest %s", state.Sessions, want)
	}
}
