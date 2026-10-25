package cli

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const batchTestSpec = "HEAD:blob.txt"

func newBatchTestRepo(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	// Registered AFTER t.TempDir so it runs BEFORE TempDir's RemoveAll
	// (cleanups are LIFO): on Windows a live child's CWD inside the repo makes
	// directory removal fail.
	t.Cleanup(closeCatFileBatches)
	liveGit(t, dir, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "blob.txt"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	liveGit(t, dir, "add", "blob.txt")
	liveGit(t, dir, "commit", "-q", "-m", "one")
	return dir
}

// perExecOnlyRunner runs commands exactly like ExecRunner but does not claim
// the cat-file batch capability, so it drives the per-exec fallback path.
type perExecOnlyRunner struct{}

func (perExecOnlyRunner) Run(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
	return ExecRunner{}.Run(ctx, dir, name, args...)
}

func (perExecOnlyRunner) RunWithEnv(ctx context.Context, dir string, env map[string]string, name string, args ...string) ([]byte, []byte, error) {
	return ExecRunner{}.RunWithEnv(ctx, dir, env, name, args...)
}

func TestCatFileBatchReadReturnsBlobContent(t *testing.T) {
	content := strings.Repeat("transcript line\n", 200)
	dir := newBatchTestRepo(t, content)
	for range 3 { // repeated reads reuse one child and stay in sync
		got, err := catFileBatchRead(context.Background(), dir, batchTestSpec, true)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != content {
			t.Fatalf("content mismatch: got %d bytes, want %d", len(got), len(content))
		}
	}
}

func TestCatFileBatchMissingIsDefinitiveAndNonFatal(t *testing.T) {
	dir := newBatchTestRepo(t, "hello\n")
	_, err := catFileBatchRead(context.Background(), dir, "HEAD:absent.txt", true)
	if !errors.Is(err, errGitObjectMissing) {
		t.Fatalf("want errGitObjectMissing, got %v", err)
	}
	// The child must survive a missing answer and serve the next read.
	got, err := catFileBatchRead(context.Background(), dir, batchTestSpec, true)
	if err != nil || string(got) != "hello\n" {
		t.Fatalf("read after missing: %q %v", got, err)
	}
}

func TestCatFileBatchFailureCoolsDownThenRespawns(t *testing.T) {
	dir := newBatchTestRepo(t, "respawn\n")
	ctx := context.Background()
	if _, err := catFileBatchRead(ctx, dir, batchTestSpec, true); err != nil {
		t.Fatal(err)
	}
	key := catFileBatchKey{gitDir: dir, localOnly: true}
	catFileBatchMu.Lock()
	entry := catFileBatchEntries[key]
	catFileBatchMu.Unlock()
	if entry == nil || entry.proc == nil {
		t.Fatal("expected a registered batch child")
	}
	entry.proc.mu.Lock()
	entry.proc.fail(errors.New("induced failure"))
	entry.proc.mu.Unlock()
	// During the cooldown the key answers an error without spawning.
	if _, err := catFileBatchRead(ctx, dir, batchTestSpec, true); err == nil || errors.Is(err, errGitObjectMissing) {
		t.Fatalf("want a cooldown error, got %v", err)
	}
	// An expired tombstone respawns a working child.
	catFileBatchMu.Lock()
	catFileBatchEntries[key].retryAfter = time.Now().Add(-time.Second)
	catFileBatchMu.Unlock()
	got, err := catFileBatchRead(ctx, dir, batchTestSpec, true)
	if err != nil || string(got) != "respawn\n" {
		t.Fatalf("read after cooldown: %q %v", got, err)
	}
}

func TestLocalObjectTypeBatchAndFallbackAgree(t *testing.T) {
	dir := newBatchTestRepo(t, "typed\n")
	ctx := context.Background()
	batched, err := localObjectType(ctx, ExecRunner{}, dir, batchTestSpec)
	if err != nil {
		t.Fatal(err)
	}
	perExec, err := localObjectType(ctx, perExecOnlyRunner{}, dir, batchTestSpec)
	if err != nil {
		t.Fatal(err)
	}
	if batched != "blob" || perExec != "blob" {
		t.Fatalf("want blob/blob, got %q/%q", batched, perExec)
	}
	// On platforms where the gate disables batching, the missing object still
	// errors — just not with the batch sentinel.
	if _, err := localObjectType(ctx, ExecRunner{}, dir, "HEAD:absent.txt"); err == nil {
		t.Fatal("want an error for a missing object")
	} else if runnerAllowsCatFileBatch(ExecRunner{}) && !errors.Is(err, errGitObjectMissing) {
		t.Fatalf("want errGitObjectMissing, got %v", err)
	}
	if _, err := localObjectType(ctx, perExecOnlyRunner{}, dir, "HEAD:absent.txt"); err == nil {
		t.Fatal("per-exec path must also error for a missing object")
	}
}

func TestCatFileWithPolicyMatchesPerExec(t *testing.T) {
	content := "policy parity\n"
	dir := newBatchTestRepo(t, content)
	ctx := context.Background()
	for _, localOnly := range []bool{true, false} {
		batched, err := catFileWithPolicy(ctx, ExecRunner{}, dir, "HEAD", "blob.txt", localOnly)
		if err != nil {
			t.Fatalf("batched localOnly=%v: %v", localOnly, err)
		}
		perExec, err := catFileWithPolicy(ctx, perExecOnlyRunner{}, dir, "HEAD", "blob.txt", localOnly)
		if err != nil {
			t.Fatalf("per-exec localOnly=%v: %v", localOnly, err)
		}
		if string(batched) != content || string(perExec) != content {
			t.Fatalf("localOnly=%v: batched %q per-exec %q", localOnly, batched, perExec)
		}
	}
	if _, err := catFileWithPolicy(ctx, ExecRunner{}, dir, "HEAD", "absent.txt", true); err == nil {
		t.Fatal("want error for a missing path")
	}
}

func TestCatFileBatchCancelledFirstCallerDoesNotPoisonTheKey(t *testing.T) {
	dir := newBatchTestRepo(t, "unpoisoned\n")
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := catFileBatchRead(cancelled, dir, batchTestSpec, true); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled for the cancelled caller, got %v", err)
	}
	// The key must stay healthy: no cooldown tombstone from the cancelled caller.
	got, err := catFileBatchRead(context.Background(), dir, batchTestSpec, true)
	if err != nil || string(got) != "unpoisoned\n" {
		t.Fatalf("read after cancelled first caller: %q %v", got, err)
	}
}

func TestCatFileBatchStalledChildIsKilledAtDeadline(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs sh; batching is disabled on Windows anyway")
	}
	// A child that swallows requests and never answers simulates a stalled
	// lazy fetch; the caller's deadline must kill it rather than hang.
	cmd := exec.Command("sh", "-c", "cat > /dev/null")
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &catFileBatchProc{
		key:    catFileBatchKey{gitDir: t.TempDir(), localOnly: true},
		cmd:    cmd,
		in:     in,
		out:    bufio.NewReader(out),
		stderr: &cappedBuffer{limit: 1024},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, err = p.request(ctx, "deadbeef", true)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want context.DeadlineExceeded, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("request hung %v past its 100ms deadline", elapsed)
	}
	if !p.isBrokenForTest() {
		t.Fatal("a killed child must be marked broken")
	}
}

func (p *catFileBatchProc) isBrokenForTest() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.broken
}

func TestCatFileBatchRejectsWhitespaceSpecs(t *testing.T) {
	dir := newBatchTestRepo(t, "x\n")
	for _, spec := range []string{"HEAD:a\nHEAD:b", "HEAD:has space.txt"} {
		if _, err := catFileBatchRead(context.Background(), dir, spec, true); err == nil {
			t.Fatalf("want error for spec %q", spec)
		}
	}
}

func TestCatFileBatchConcurrentReadsStayInSync(t *testing.T) {
	content := strings.Repeat("row\n", 500)
	dir := newBatchTestRepo(t, content)
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				got, err := catFileBatchRead(ctx, dir, batchTestSpec, true)
				if err != nil {
					errs <- err
					return
				}
				if string(got) != content {
					errs <- errors.New("content mismatch under concurrency")
					return
				}
				if _, err := catFileBatchRead(ctx, dir, "HEAD:absent.txt", true); !errors.Is(err, errGitObjectMissing) {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// The per-exec path has to classify from an exit code, because `cat-file -t`
// answers a missing object with exit 128 and a `fatal:` line -- the same status
// a repository that cannot be opened produces. Against REAL git: a resolvable
// spec whose object has been pruned is typed as errGitObjectMissing, and an
// unresolvable spec is deliberately not, because its 128 is indistinguishable
// from an unusable repository without reading git's prose.
func TestLocalObjectTypePerExecClassifiesMissingByExitCode(t *testing.T) {
	dir := newBatchTestRepo(t, "pruned\n")
	ctx := context.Background()
	oid := strings.TrimSpace(liveGit(t, dir, "rev-parse", batchTestSpec))
	if len(oid) < 3 {
		t.Fatalf("rev-parse %s returned %q", batchTestSpec, oid)
	}
	// Present: the per-exec probe answers the type and asks nothing further.
	if kind, err := localObjectType(ctx, perExecOnlyRunner{}, dir, batchTestSpec); err != nil || kind != "blob" {
		t.Fatalf("want blob, got %q %v", kind, err)
	}
	// Prune the object itself, leaving the tree entry that names it. This is
	// the shape a partial clone or a pruned checkpoint leaves behind, and the
	// one case the export may skip a session over.
	if err := os.Remove(filepath.Join(dir, ".git", "objects", oid[:2], oid[2:])); err != nil {
		t.Fatalf("pruning the blob: %v", err)
	}
	_, err := localObjectType(ctx, perExecOnlyRunner{}, dir, batchTestSpec)
	if !errors.Is(err, errGitObjectMissing) {
		t.Fatalf("a pruned object must be typed as missing, got %v", err)
	}
	// An unresolvable spec errors, but NOT as a missing object: on this path
	// that verdict is reserved for git's exit 1.
	_, err = localObjectType(ctx, perExecOnlyRunner{}, dir, "HEAD:absent.txt")
	if err == nil {
		t.Fatal("an unresolvable spec must still error")
	}
	if errors.Is(err, errGitObjectMissing) {
		t.Fatalf("git's generic exit 128 was read as a missing object: %v", err)
	}
}
