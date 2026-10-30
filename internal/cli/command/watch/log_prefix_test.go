package watch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

// testWatchLogFields is the fields a test line carries, with pid pinned so the
// expected prefix is a constant rather than a reconstruction of the code under
// test.
func testWatchLogFields(repo string, tick *int) LogFields {
	return LogFields{pid: 4242, Build: "0.2.0", Tick: tick, Repo: repo}
}

// TestWatchLogPrefixStampsEveryLine is the guard for the diagnosis that cost
// the time: every line must answer when, which build, which tick and which
// repo on its own, not just the banner above it.
func TestWatchLogPrefixStampsEveryLine(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	at := time.Date(2026, 10, 24, 9, 30, 15, 0, time.UTC)
	tick := 42
	w := NewLogPrefixWriter(&buf, testWatchLogFields("gh/entireio/entire-brain", &tick), fixedClock(at))

	fmt.Fprintf(w, "[watch] refresh failed: %v\n", errors.New("brain_locked"))
	fmt.Fprint(w, "refresh: seed baseline failed\n[watch] distill skipped\n")

	stamp := "2026-10-24T09:30:15Z pid=4242 build=0.2.0 tick=42 repo=gh/entireio/entire-brain "
	want := stamp + "[watch] refresh failed: brain_locked\n" +
		stamp + "refresh: seed baseline failed\n" +
		stamp + "[watch] distill skipped\n"
	if got := buf.String(); got != want {
		t.Fatalf("stamped output\n got: %q\nwant: %q", got, want)
	}
}

// TestWatchLogPrefixTracksTheLoopCounter: the tick is read at write time, so
// one writer follows the loop's counter as it advances. A writer that froze
// the value it was built with would label every tick 1.
func TestWatchLogPrefixTracksTheLoopCounter(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	at := time.Date(2026, 10, 24, 9, 30, 15, 0, time.UTC)
	tick := 1
	w := NewLogPrefixWriter(&buf, testWatchLogFields("repo", &tick), fixedClock(at))
	fmt.Fprint(w, "[watch] first\n")
	tick = 2
	fmt.Fprint(w, "[watch] second\n")
	got := buf.String()
	if !strings.Contains(got, "tick=1 repo=repo [watch] first") {
		t.Fatalf("first line lost its tick: %q", got)
	}
	if !strings.Contains(got, "tick=2 repo=repo [watch] second") {
		t.Fatalf("second line did not follow the counter: %q", got)
	}
}

// TestWatchLogPrefixOmitsAbsentFields: workspace-level lines belong to no repo
// and the startup banner belongs to no tick. Neither may leave an empty
// key=value behind.
func TestWatchLogPrefixOmitsAbsentFields(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	at := time.Date(2026, 10, 24, 9, 30, 15, 0, time.UTC)
	w := NewLogPrefixWriter(&buf, testWatchLogFields("", nil), fixedClock(at))
	fmt.Fprint(w, "[watch] workspace ws — 3 repos\n")
	want := "2026-10-24T09:30:15Z pid=4242 build=0.2.0 [watch] workspace ws — 3 repos\n"
	if got := buf.String(); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	for _, bad := range []string{"tick=", "repo="} {
		if strings.Contains(buf.String(), bad) {
			t.Fatalf("absent field rendered anyway (%q): %q", bad, buf.String())
		}
	}
}

// TestWatchLogPrefixDefaultsPidAndBuild: a caller that names neither still
// produces a line that says which process wrote it -- the question COR-2174
// could not answer.
func TestWatchLogPrefixDefaultsPidAndBuild(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	w := NewLogPrefixWriter(&buf, LogFields{Repo: "repo"}, fixedClock(time.Unix(0, 0).UTC()))
	fmt.Fprint(w, "[watch] line\n")
	wantPid := fmt.Sprintf("pid=%d ", os.Getpid())
	if !strings.Contains(buf.String(), wantPid) {
		t.Fatalf("line does not name the writing process (want %q): %q", wantPid, buf.String())
	}
	if !strings.Contains(buf.String(), "build=dev ") {
		t.Fatalf("line does not name the build: %q", buf.String())
	}
}

// TestWatchLogPrefixReportsCallerLength: a short count makes io.Copy and
// fmt.Fprintf treat the write as failed, so the prefix must not be counted.
func TestWatchLogPrefixReportsCallerLength(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	w := NewLogPrefixWriter(&buf, testWatchLogFields("repo", nil), fixedClock(time.Unix(0, 0).UTC()))
	payload := []byte("[watch] line\n")
	n, err := w.Write(payload)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != len(payload) {
		t.Fatalf("Write returned %d, want caller length %d", n, len(payload))
	}
	if _, err := fmt.Fprintf(w, "[watch] %s\n", "second"); err != nil {
		t.Fatalf("Fprintf saw a short write: %v", err)
	}
}

// TestWatchLogPrefixNoBarePrefixOnTrailingNewline: a newline-terminated write
// must not emit a stamp with nothing after it.
func TestWatchLogPrefixNoBarePrefixOnTrailingNewline(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	w := NewLogPrefixWriter(&buf, testWatchLogFields("repo", nil), fixedClock(time.Unix(0, 0).UTC()))
	fmt.Fprint(w, "[watch] only line\n")
	want := "1970-01-01T00:00:00Z pid=4242 build=0.2.0 repo=repo [watch] only line\n"
	if got := buf.String(); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

// TestWatchLogPrefixDoesNotSplitAPartialLine: a write that ends mid-line is
// continued, not re-stamped in its middle.
func TestWatchLogPrefixDoesNotSplitAPartialLine(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	w := NewLogPrefixWriter(&buf, testWatchLogFields("repo", nil), fixedClock(time.Unix(0, 0).UTC()))
	fmt.Fprint(w, "[watch] half")
	fmt.Fprint(w, " and half\n")
	fmt.Fprint(w, "[watch] next\n")
	stamp := "1970-01-01T00:00:00Z pid=4242 build=0.2.0 repo=repo "
	want := stamp + "[watch] half and half\n" + stamp + "[watch] next\n"
	if got := buf.String(); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

// TestWatchLogPrefixAdvancesTheClock: the stamp is taken per write, so two
// writes a minute apart are distinguishable in the log.
func TestWatchLogPrefixAdvancesTheClock(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	cur := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	w := NewLogPrefixWriter(&buf, testWatchLogFields("repo", nil), func() time.Time {
		at := cur
		cur = cur.Add(time.Minute)
		return at
	})
	fmt.Fprint(w, "[watch] first\n")
	fmt.Fprint(w, "[watch] second\n")
	if got := buf.String(); !strings.Contains(got, "2026-10-24T09:00:00Z") || !strings.Contains(got, "2026-10-24T09:01:00Z") {
		t.Fatalf("clock did not advance between writes: %q", got)
	}
}

// TestWatchLoopStampsTickLinesAndStepOutput is the single-repo half of
// COR-2176, driven through the real loop rather than the writer. Two things
// are guarded that the earlier attempt at this fix got wrong: the loop, not
// the tick, decides attribution (that attempt stamped only the banner printed
// above the loop, which was never the problem), and the STEPS write to the
// same attributed sink -- `refresh: seed baseline failed` is the line the
// ticket quotes, and it is written by the refresh step, not by the tick.
func TestWatchLoopStampsTickLinesAndStepOutput(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cursor := filepath.Join(dir, "watch.json")
	at := time.Date(2026, 10, 24, 9, 30, 15, 0, time.UTC)
	var refreshed, seeded, distilled int
	w := DefaultFlags()
	w.Once = true

	var buf bytes.Buffer
	var sawSink bool
	stepsFor := func(sink io.Writer) Steps {
		sawSink = true
		steps := fakeWatchSteps("ck1:head1", &refreshed, &seeded, &distilled)
		inner := steps.Refresh
		steps.Refresh = func(c context.Context) error {
			// Stand in for watchDeterministicRefresh's progress reporter,
			// which writes through the command it is handed.
			fmt.Fprintln(sink, "refresh: seed baseline failed")
			return inner(c)
		}
		return steps
	}
	if err := watchLoop(context.Background(), &buf, LogFields{pid: 4242, Build: "0.2.0", Repo: "gh/entireio/entire-brain"},
		fixedClock(at), w, cursor, stepsFor); err != nil {
		t.Fatalf("watchLoop: %v", err)
	}
	if !sawSink {
		t.Fatalf("the loop never asked for steps bound to its sink")
	}
	out := buf.String()
	if strings.TrimSpace(out) == "" {
		t.Fatalf("tick wrote nothing; the guard would pass vacuously")
	}
	stamp := "2026-10-24T09:30:15Z pid=4242 build=0.2.0 tick=1 repo=gh/entireio/entire-brain "
	for i, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		if !strings.HasPrefix(line, stamp) {
			t.Fatalf("tick line %d is unattributed: %q\nfull log:\n%s", i, line, out)
		}
	}
	if !strings.Contains(out, stamp+"refresh: seed baseline failed") {
		t.Fatalf("the refresh step's own line did not reach the attributed sink:\n%s", out)
	}
}

// The writer's concurrency contract, stated as a test because the two things
// that keep it safe in this build are both implicit.
//
// watchLoop builds ONE of these and hands it to distill, refresh and seed as
// both stdout and stderr, and distill owns a --jobs worker pool. Nothing in
// this build writes to it from a second goroutine -- the workers return over
// channels, agent subprocesses get capped buffers, and the only progress
// goroutine is gated behind a TTY check that requires an *os.File, which this
// never is -- so this is not a regression test for an observed race. It is the
// guarantee that makes the sharing above safe to keep: every line comes out
// whole, stamped exactly once, whoever wrote it.
//
// Run under -race this fails without the lock; it also fails without it on
// content alone, because midLine is read and written on every call, so losing
// it mis-attributes continuation lines rather than crashing.
func TestWatchLogPrefixIsSafeForConcurrentWriters(t *testing.T) {
	var sink lockedBuffer
	w := NewLogPrefixWriter(&sink, testWatchLogFields("repo-a", nil), fixedClock(time.Unix(0, 0).UTC()))

	const writers, perWriter = 8, 40
	var wg sync.WaitGroup
	for id := 0; id < writers; id++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for n := 0; n < perWriter; n++ {
				if _, err := fmt.Fprintf(w, "writer=%d line=%d\n", id, n); err != nil {
					t.Errorf("write: %v", err)
					return
				}
			}
		}(id)
	}
	wg.Wait()

	const stamp = "1970-01-01T00:00:00Z pid=4242 build=0.2.0 repo=repo-a "
	lines := strings.Split(strings.TrimSuffix(sink.String(), "\n"), "\n")
	if len(lines) != writers*perWriter {
		t.Fatalf("got %d lines, want %d; lines were interleaved or lost", len(lines), writers*perWriter)
	}
	seen := map[string]bool{}
	for _, line := range lines {
		if !strings.HasPrefix(line, stamp) {
			t.Fatalf("line lost or doubled its stamp: %q", line)
		}
		body := strings.TrimPrefix(line, stamp)
		if strings.Contains(body, "writer=") != true || strings.Contains(body, stamp) {
			t.Fatalf("line body is not one whole write: %q", body)
		}
		if seen[body] {
			t.Fatalf("duplicate line %q", body)
		}
		seen[body] = true
	}
	if len(seen) != writers*perWriter {
		t.Fatalf("got %d distinct lines, want %d", len(seen), writers*perWriter)
	}
}

// lockedBuffer isolates what is under test: without it the SINK would be the
// race, and the test would pass or fail on bytes.Buffer's thread-safety rather
// than on the writer's.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
