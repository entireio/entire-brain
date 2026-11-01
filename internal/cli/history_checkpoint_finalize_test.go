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

// commitCheckpointFiles writes files into the fixture's checkpoint and moves the
// checkpoint ref to the result, with nothing in the Brain changing.
func commitCheckpointFiles(t *testing.T, repo, message string, files map[string]string) {
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
	root := filepath.Join(repo, filepath.FromSlash(checkpointPath("aaa111aaa111")))
	for rel, data := range files {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git("add", ".")
	git("-c", "commit.gpgsign=false", "commit", "-qm", message)
	git("update-ref", v1MainRef, "HEAD")
}

// finalizeCheckpointTranscript does what a "Finalize transcript" commit does:
// it rewrites an existing checkpoint's transcript under the same checkpoint id.
func finalizeCheckpointTranscript(t *testing.T, repo string) string {
	t.Helper()
	finalized := conversationClaudeFixture + `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"` + finalizedMarker + `"}]}}` + "\n" +
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Decision: keep ` + finalizedMarker + `"}]}}` + "\n"
	commitCheckpointFiles(t, repo, "Finalize transcript for Checkpoint: aaa111aaa111", map[string]string{"0/full.jsonl": finalized})
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
	const withManifestUpdate = "alongside an unrelated manifest update"
	afterBuild := func(fire func()) (historyIndexProgress, func()) {
		return func(done, total int) {
			if total > 0 && done == total {
				fire()
			}
		}, func() {}
	}
	windows := map[string]func(fire func()) (historyIndexProgress, func()){
		"after the build read the transcript, before receipts": afterBuild,
		withManifestUpdate: afterBuild,
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
					if name == withManifestUpdate {
						// Touches the manifest without changing any session.
						manifest, err := bmanifest.Load(brain)
						if err != nil {
							t.Fatal(err)
						}
						if err := writeBrainManifestAndReadme(brain, *manifest); err != nil {
							t.Fatal(err)
						}
					}
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

// moveRefAfterListing moves the checkpoint ref once, straight after a store lists
// the tree's object ids and before it reads anything through them.
type moveRefAfterListing struct {
	ExecRunner
	move  func()
	moved bool
}

func (r *moveRefAfterListing) RunWithEnv(ctx context.Context, dir string, env map[string]string, name string, args ...string) ([]byte, []byte, error) {
	stdout, stderr, err := r.ExecRunner.RunWithEnv(ctx, dir, env, name, args...)
	joined := strings.Join(args, " ")
	if !r.moved && strings.Contains(joined, "ls-tree -r") && !strings.Contains(joined, "--name-only") {
		r.moved = true
		r.move()
	}
	return stdout, stderr, err
}

// A snapshot's metadata and transcripts must come from the same listing. If the
// metadata followed the moved ref, the store could file one session's pinned
// bytes under whichever session the ref now names at that location, and the
// tombstone gate would judge them by the wrong session.
func TestPinnedStoreReadsMetadataAndTranscriptFromOneListing(t *testing.T) {
	opts, brain, repo := filelessCheckpointFixture(t)
	runFilelessExport(t, opts, "", true)
	manifest, err := bmanifest.Load(brain)
	if err != nil {
		t.Fatal(err)
	}
	rel := manifest.Sources.Sessions.Sessions[0].TranscriptPath
	// A cached metadata blob would hide the read this test is about.
	if err := os.Remove(filepath.Join(brain, filepath.FromSlash(checkpointMetadataCachePath))); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	metadataPath := filepath.Join(repo, filepath.FromSlash(checkpointPath("aaa111aaa111")), "0", "metadata.json")
	metadata, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	runner := &moveRefAfterListing{move: func() {
		commitCheckpointFiles(t, repo, "reassign location", map[string]string{
			"0/metadata.json": strings.ReplaceAll(string(metadata), "session-one", "session-two"),
			"0/full.jsonl":    "other session's bytes\n",
		})
	}}
	store := newPinnedTranscriptStore(runner, brain)
	data, err := store.Read(context.Background(), rel)
	if err != nil || string(data) != conversationClaudeFixture {
		t.Fatalf("read = %q, %v; want the listed session's own bytes", data, err)
	}
	if !runner.moved {
		t.Fatal("the ref never moved, so this proves nothing")
	}
}

// countingListings counts tree listings by shape.
type countingListings struct {
	ExecRunner
	nameOnly, withIDs int
}

func (r *countingListings) RunWithEnv(ctx context.Context, dir string, env map[string]string, name string, args ...string) ([]byte, []byte, error) {
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "ls-tree -r --name-only") {
		r.nameOnly++
	} else if strings.Contains(joined, "ls-tree -r") {
		r.withIDs++
	}
	return r.ExecRunner.RunWithEnv(ctx, dir, env, name, args...)
}

// Names and object ids must come from one listing. Two listings could straddle a
// ref move, and a path present in only one of them has no consistent answer.
func TestCheckpointSnapshotListsEachRefOnce(t *testing.T) {
	opts, brain, _ := filelessCheckpointFixture(t)
	runFilelessExport(t, opts, "", true)
	manifest, err := bmanifest.Load(brain)
	if err != nil {
		t.Fatal(err)
	}
	runner := &countingListings{}
	store := newPinnedTranscriptStore(runner, brain)
	if _, err := store.Read(context.Background(), manifest.Sources.Sessions.Sessions[0].TranscriptPath); err != nil {
		t.Fatal(err)
	}
	if runner.nameOnly != 0 || runner.withIDs == 0 {
		t.Fatalf("listings: %d name-only, %d with ids; want one kind, with ids", runner.nameOnly, runner.withIDs)
	}
}

func TestPinnedReadStaysInsideTheListing(t *testing.T) {
	source := checkpointSnapshotSource{GitDir: "/repo", Ref: v1MainRef, OIDs: map[string]string{"aa/bb/other.jsonl": "oid-1"}}
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "cat-file", "-p", v1MainRef+":aa/bb/full.jsonl"): {stdout: "from a later tree"},
	}}
	if data, ok, err := readPinnedCheckpointSourceTranscript(context.Background(), runner, source, "aa/bb/full.jsonl"); !ok || err == nil {
		t.Fatalf("transcript outside the listing: data=%q ok=%v err=%v; want a refusal", data, ok, err)
	}
	source.OIDs = nil
	if _, ok, _ := readPinnedCheckpointSourceTranscript(context.Background(), runner, source, "aa/bb/full.jsonl"); ok {
		t.Fatal("with no listing there is nothing to pin to, so the caller must read through the ref")
	}
}

func TestTreePathOIDsKeepsExactFileNames(t *testing.T) {
	listing := "100644 blob aaa\t0/full.jsonl\n100644 blob bbb\t0/full.jsonl \n"
	oids := treePathOIDs([]byte(listing))
	if oids["0/full.jsonl"] != "aaa" || oids["0/full.jsonl "] != "bbb" {
		t.Fatalf("oids = %q: a differently named file must not replace the transcript", oids)
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
