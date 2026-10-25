package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// git_catfile_batch.go routes repeated object reads through one long-lived
// `git cat-file --batch` child per (gitDir, policy) instead of one `git
// cat-file` exec per blob. A cold export/distill on a ~1,000-checkpoint repo
// spawned ~5,000 git processes at ~30ms each (COR-2182); the same reads
// through one child are pipe round-trips.
//
// The child is spawned through hardenedGitInvocation — the same preamble the
// buffered and streaming runners use — and carries localOnlyGitEnv when the
// caller demanded a local-only read, so policy stays byte-for-byte the
// per-exec path's. Callers treat any infrastructure error as "fall back to
// the per-exec path", so a broken or killed child never changes behavior,
// only speed; a failing key cools down rather than respawning per read.
//
// Children exit on stdin EOF, which the OS delivers when this process exits.
// A long-lived parent (the MCP server) keeps at most one idle child per key,
// bounded by the handful of repositories it serves.

// catFileBatchMaxBlobBytes bounds one blob read so a corrupt size header
// cannot drive the process out of memory. The per-exec fallback buffers whole
// blobs too; this is a guard against a nonsense header, not a content policy.
const catFileBatchMaxBlobBytes = 1 << 30

// catFileBatchRetryInterval is how long a key that failed to spawn or broke
// mid-stream answers errors immediately (sending callers to the per-exec
// path) before a fresh child is attempted. Without it a persistently broken
// child would respawn per read — a spawn storm costlier than no batching.
const catFileBatchRetryInterval = time.Minute

// errGitObjectMissing is git's definitive "object not found" answer. It is
// a real result, not an infrastructure failure: callers must NOT retry it on
// the per-exec path, which would answer the same thing one spawn later.
//
// It is the ONLY signal a caller may read as "these bytes are not in this
// repository". Both object-probe paths wrap it — the batch child from git's
// machine-readable `<spec> missing` record, the per-exec path from an exit
// code (see localObjectMissingConfirmed) — so no caller has to decide what a
// bare `err != nil` from git meant.
var errGitObjectMissing = errors.New("object missing")

// gitObjectAbsentExitCode is the status `git cat-file -e <spec>` exits with,
// silently and with no stderr, when the spec resolved to an object name that
// is not present in this repository. Git's other refusals — a spec that does
// not resolve, a repository that cannot be opened, a bad config value — all
// use 128 instead.
const gitObjectAbsentExitCode = 1

// exitCoder is the capability an error has when it carries a process exit
// status. *exec.ExitError satisfies it through its embedded *os.ProcessState,
// so this classification does not have to assert a concrete os/exec type —
// and a test runner can model an exit status without spawning anything.
type exitCoder interface{ ExitCode() int }

// catFileBatchKey separates children by policy as well as repository. check
// children run `--batch-check` because a type probe must not stream the
// object body: transcript validation asks "is this a blob" about multi-MB
// transcript chunks, and answering through the content child would pipe the
// whole corpus through only to discard it.
type catFileBatchKey struct {
	gitDir    string
	localOnly bool
	check     bool
}

// catFileBatchEntry is the registry slot for one key. once gates the spawn so
// concurrent first users wait for one child instead of racing several, and a
// failed entry keeps err+retryAfter as a cooldown tombstone.
type catFileBatchEntry struct {
	once       sync.Once
	proc       *catFileBatchProc
	err        error
	retryAfter time.Time
}

// catFileBatchProc is one child. Requests are strictly serialized under mu:
// one spec line written, one full record read, so the stream cannot desync
// across goroutines. Any read/parse irregularity kills the child and
// tombstones its registry entry for catFileBatchRetryInterval.
type catFileBatchProc struct {
	key    catFileBatchKey
	mu     sync.Mutex
	cmd    *exec.Cmd
	in     io.WriteCloser
	out    *bufio.Reader
	stderr *cappedBuffer
	broken bool
}

var (
	catFileBatchMu      sync.Mutex
	catFileBatchEntries = map[catFileBatchKey]*catFileBatchEntry{}
)

func runnerAllowsCatFileBatch(runner CommandRunner) bool {
	// ponytail: batching is disabled on Windows. A lingering child holds its
	// CWD and mmap'd pack handles, and Windows refuses to delete directories
	// and files a live process holds — observed as TempDir cleanup failures in
	// CI, and a real hazard for any caller that removes a repo after reading
	// it. The per-exec path stays correct, just slower. Upgrade path: an idle
	// reaper plus a spawn that keeps the child's CWD outside the repository.
	if runtime.GOOS == "windows" {
		return false
	}
	b, ok := runner.(GitCatFileBatcher)
	return ok && b.CatFileBatchAllowed()
}

// closeCatFileBatches synchronously kills and reaps every batch child and
// clears the registry. Tests call it before temp-dir cleanup; a long-lived
// caller may use it to release repository handles.
func closeCatFileBatches() {
	catFileBatchMu.Lock()
	entries := make([]*catFileBatchEntry, 0, len(catFileBatchEntries))
	for key, e := range catFileBatchEntries {
		entries = append(entries, e)
		delete(catFileBatchEntries, key)
	}
	catFileBatchMu.Unlock()
	for _, e := range entries {
		e.once.Do(func() {})
		p := e.proc
		if p == nil {
			continue
		}
		p.mu.Lock()
		p.broken = true
		_ = p.in.Close()
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
		_ = p.cmd.Wait()
		p.mu.Unlock()
	}
}

// catFileBatchEntryFor returns the live entry for key, replacing an expired
// tombstone. It holds the registry lock only for map access; spawning happens
// in the entry's once, outside the lock, so a slow spawn on one key never
// blocks reads on another.
func catFileBatchEntryFor(key catFileBatchKey) *catFileBatchEntry {
	catFileBatchMu.Lock()
	defer catFileBatchMu.Unlock()
	if e, ok := catFileBatchEntries[key]; ok {
		if e.err == nil || time.Now().Before(e.retryAfter) {
			return e
		}
	}
	e := &catFileBatchEntry{}
	catFileBatchEntries[key] = e
	return e
}

func catFileBatchProcFor(ctx context.Context, key catFileBatchKey) (*catFileBatchProc, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	e := catFileBatchEntryFor(key)
	e.once.Do(func() {
		// The child outlives any one caller, so its spawn must too: a first
		// caller whose context is already cancelled or near its deadline must
		// not fail the spawn and tombstone the key for everyone else. The
		// filter probe inside the spawn carries its own timeout, so a genuine
		// probe failure still cools the key down.
		proc, err := spawnCatFileBatch(context.Background(), key)
		// Published under the registry lock: catFileBatchEntryFor reads these
		// fields under the same lock while this spawn may still be in flight.
		catFileBatchMu.Lock()
		e.proc, e.err = proc, err
		if err != nil {
			e.retryAfter = time.Now().Add(catFileBatchRetryInterval)
		}
		catFileBatchMu.Unlock()
	})
	return e.proc, e.err
}

func spawnCatFileBatch(ctx context.Context, key catFileBatchKey) (*catFileBatchProc, error) {
	mode := "--batch"
	if key.check {
		mode = "--batch-check"
	}
	var env map[string]string
	if key.localOnly {
		env = localOnlyGitEnv()
	}
	args, mergedEnv, err := hardenedGitInvocation(ctx, key.gitDir, env, "cat-file", mode)
	if err != nil {
		return nil, err
	}
	// Not CommandContext: the child outlives any one caller's context on
	// purpose. It dies on stdin EOF when this process exits.
	cmd := exec.Command("git", args...)
	cmd.WaitDelay = commandWaitDelay
	cmd.Dir = key.gitDir
	cmd.Env = mergedEnv
	stderr := &cappedBuffer{limit: streamStderrCap}
	cmd.Stderr = stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &catFileBatchProc{key: key, cmd: cmd, in: in, out: bufio.NewReader(out), stderr: stderr}, nil
}

// fail kills the child and tombstones its registry entry so the key answers
// errors for a cooldown instead of respawning per read. Called under p.mu.
func (p *catFileBatchProc) fail(cause error) {
	p.broken = true
	_ = p.in.Close()
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	// Reap synchronously: the child is already killed, so Wait is bounded by
	// WaitDelay, and on Windows the repo directory stays undeletable until the
	// child's handles are released.
	_ = p.cmd.Wait()
	tombstone := &catFileBatchEntry{
		err:        fmt.Errorf("git cat-file batch: cooling down after: %w (stderr: %s)", cause, strings.TrimSpace(string(p.stderr.Bytes()))),
		retryAfter: time.Now().Add(catFileBatchRetryInterval),
	}
	tombstone.once.Do(func() {})
	catFileBatchMu.Lock()
	catFileBatchEntries[p.key] = tombstone
	catFileBatchMu.Unlock()
}

// request writes one spec and reads one record. body is nil in check mode.
func (p *catFileBatchProc) request(ctx context.Context, spec string, withBody bool) ([]byte, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	// A newline would inject a second request; whitespace of any kind would
	// make git's space-delimited "missing" answer unparseable. Both route the
	// caller to the per-exec path, which handles any spec.
	if strings.ContainsAny(spec, " \t\n\r") {
		return nil, "", fmt.Errorf("git cat-file batch: spec contains whitespace: %q", spec)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.broken {
		return nil, "", errors.New("git cat-file batch: process is broken")
	}
	// A read blocked on a stalled child — a lazy fetch on the non-local-only
	// key in a partial clone, or a wedged git — must not outlive its caller:
	// everyone else on this key queues behind p.mu. Killing the child
	// unblocks the read with an error, and the normal fail path tombstones
	// the key so later callers go per-exec. The rare benign race (ctx ending
	// just as a request completes) kills a healthy child; the key respawns.
	stopWatcher := context.AfterFunc(ctx, func() {
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
	})
	defer stopWatcher()
	fail := func(err error) ([]byte, string, error) {
		if ctxErr := ctx.Err(); ctxErr != nil {
			err = errors.Join(ctxErr, err)
		}
		p.fail(err)
		return nil, "", err
	}
	if _, err := io.WriteString(p.in, spec+"\n"); err != nil {
		return fail(fmt.Errorf("write request: %w", err))
	}
	header, err := p.out.ReadString('\n')
	if err != nil {
		return fail(fmt.Errorf("read header: %w", err))
	}
	fields := strings.Fields(header)
	if len(fields) == 2 {
		// "<spec> missing" and friends ("ambiguous") are answers from a
		// healthy child, not failures.
		return nil, "", fmt.Errorf("git cat-file batch: %s %s: %w", spec, fields[1], errGitObjectMissing)
	}
	if len(fields) != 3 {
		return fail(fmt.Errorf("unexpected header %q for %s", strings.TrimSpace(header), spec))
	}
	objType := fields[1]
	size, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil || size < 0 {
		return fail(fmt.Errorf("unparseable size in header %q", strings.TrimSpace(header)))
	}
	if !withBody {
		return nil, objType, nil
	}
	if size > catFileBatchMaxBlobBytes {
		// Killing the child is the standard recovery; the caller falls back
		// to the per-exec path, whose consumers apply their own read bounds.
		return fail(fmt.Errorf("%s is %d bytes, over the %d byte guard", spec, size, catFileBatchMaxBlobBytes))
	}
	body := make([]byte, size)
	if _, err := io.ReadFull(p.out, body); err != nil {
		return fail(fmt.Errorf("read %d-byte body for %s: %w", size, spec, err))
	}
	// Every record ends with one LF after the content.
	if trailer, err := p.out.ReadByte(); err != nil || trailer != '\n' {
		return fail(fmt.Errorf("record for %s not terminated by LF", spec))
	}
	return body, objType, nil
}

func catFileBatchRequest(ctx context.Context, gitDir, spec string, localOnly, check bool) ([]byte, string, error) {
	key := catFileBatchKey{gitDir: gitDir, localOnly: localOnly, check: check}
	p, err := catFileBatchProcFor(ctx, key)
	if err != nil {
		return nil, "", err
	}
	return p.request(ctx, spec, !check)
}

// catFileBatchRead reads one object's content: `git cat-file -p <spec>`, batched.
func catFileBatchRead(ctx context.Context, gitDir, spec string, localOnly bool) ([]byte, error) {
	body, _, err := catFileBatchRequest(ctx, gitDir, spec, localOnly, false)
	return body, err
}

// catFileBatchType reports one object's type: `git cat-file -t <spec>`, batched.
func catFileBatchType(ctx context.Context, gitDir, spec string, localOnly bool) (string, error) {
	_, objType, err := catFileBatchRequest(ctx, gitDir, spec, localOnly, true)
	return objType, err
}

// localObjectType answers `git cat-file -t` under the local-only policy,
// batched for batch-capable runners and per-exec for every other runner.
// A batch "missing" answer is definitive; other batch errors fall back.
//
// Either path reports a definitively absent object as errGitObjectMissing and
// everything else as a plain error, so a caller can tell git's ANSWER from a
// failure to ask it. partitionLocalTranscriptSessions depends on exactly that
// distinction: it skips a session on the former and refuses the export on the
// latter.
func localObjectType(ctx context.Context, runner CommandRunner, gitDir, spec string) (string, error) {
	if runnerAllowsCatFileBatch(runner) {
		objType, err := catFileBatchType(ctx, gitDir, spec, true)
		if err == nil || errors.Is(err, errGitObjectMissing) {
			return objType, err
		}
	}
	kind, _, err := runCheckpointGitLocal(ctx, runner, gitDir, "cat-file", "-t", spec)
	if err != nil {
		if localObjectMissingConfirmed(ctx, runner, gitDir, spec) {
			return "", fmt.Errorf("%w: %w", errGitObjectMissing, err)
		}
		return "", err
	}
	return strings.TrimSpace(string(kind)), nil
}

// localObjectMissingConfirmed asks git a second, narrower question whose
// answer is an EXIT CODE rather than prose, so "this object is not here" can
// be told apart from "git could not be asked".
//
// `git cat-file -t` gives the per-exec path no machine-readable missing
// answer: it exits 128 and prints a `fatal:` line, and 128 is also what a
// repository that cannot be opened, an unparseable config value and a spec
// that does not resolve produce. Treating any `err != nil` as "the bytes are
// gone" is the defect this exists to prevent: because the export is fatal
// only when NOTHING is servable, one cancelled context or transient git
// fault would silently republish a shrunken corpus.
//
// `git cat-file -e` is narrower. Exit 0 means the object IS present, so the
// type probe failed for some other reason and must stay an error. Exit 1 —
// silent, no stderr — is git's answer for an object name that is not present
// here, which is the pruned or promisor-absent blob a vanished checkpoint
// leaves behind. Reading that exit code rather than git's stderr prose is the
// same route enumerateRepoFilterDrivers already takes for `git config`'s exit
// 1, and it cannot rot the way a stderr substring can.
//
// Everything else stays an error, git's 128 included. That makes the per-exec
// path STRICTER than the batch child, whose `<spec> missing` record is
// definitive for an unresolvable spec too. The asymmetry is deliberate and in
// the safe direction: this path can only refuse an export the batch path would
// have partitioned, never silently drop a session from one.
//
// The extra spawn is paid only by a probe that already failed, so a healthy
// repository never reaches it.
func localObjectMissingConfirmed(ctx context.Context, runner CommandRunner, gitDir, spec string) bool {
	// A cancelled or expired context is never evidence about an object: the
	// confirming probe would be killed on arrival and report nothing at all.
	if ctx != nil && ctx.Err() != nil {
		return false
	}
	if _, _, err := runCheckpointGitLocal(ctx, runner, gitDir, "cat-file", "-e", spec); err != nil {
		var coder exitCoder
		return errors.As(err, &coder) && coder.ExitCode() == gitObjectAbsentExitCode
	}
	return false
}

// promisorRemoteAnswers caches, per git directory, whether the repository has a
// promisor remote. The answer is a local config read and cannot change during
// an export.
var promisorRemoteAnswers sync.Map

// gitDirHasPromisorRemote reports whether this repository is a partial clone
// whose absent objects might still be FETCHABLE.
//
// It matters because "the object is not here" and "the object does not exist"
// are the same answer from `cat-file` and different facts. In a partial clone
// an unfetched blob reports exactly what a pruned one does -- measured on git
// 2.54.0, `cat-file -e` exits 1 silently for both under GIT_NO_LAZY_FETCH=1,
// which is how every local-policy probe runs. Partitioning on that answer
// would drop a session whose bytes a sync would recover, and republish a
// corpus smaller than the one already published.
//
// The marker is `remote.<name>.promisor`, matched by SUFFIX across every
// remote. It is deliberately not a lookup of `remote.origin.promisor` or of
// `extensions.partialclone`: the real case this exists for is the cli
// checkpoint repository, whose promisor remote is named by URL
// (`remote.https://github.com/entireio/cli-checkpoints.git.promisor`) while
// `extensions.partialclone` is not set at all. A check on either would miss it.
//
// A repository with no promisor remote is unaffected: an absent object there is
// absent for good, and the caller still partitions.
func gitDirHasPromisorRemote(ctx context.Context, runner CommandRunner, gitDir string) bool {
	if cached, ok := promisorRemoteAnswers.Load(gitDir); ok {
		answer, _ := cached.(bool)
		return answer
	}
	stdout, _, err := runCheckpointGitLocal(ctx, runner, gitDir, "config", "--get-regexp", `\.promisor$`)
	// `config --get-regexp` exits 1 when nothing matches, so only a match with
	// a non-empty value counts. A failure to ask is treated as "no promisor
	// remote", which keeps this helper from turning a config-read fault into a
	// refusal; the probe's own error handling already covers that class.
	answer := err == nil && strings.TrimSpace(string(stdout)) != ""
	promisorRemoteAnswers.Store(gitDir, answer)
	return answer
}
