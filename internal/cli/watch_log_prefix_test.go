package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

// testWatchLogFields is the fields a test line carries, with pid pinned so the
// expected prefix is a constant rather than a reconstruction of the code under
// test.
func testWatchLogFields(repo string, tick *int) watchLogFields {
	return watchLogFields{pid: 4242, build: "0.2.0", tick: tick, repo: repo}
}

// TestWatchLogPrefixStampsEveryLine is the guard for the diagnosis that cost
// the time: every line must answer when, which build, which tick and which
// repo on its own, not just the banner above it.
func TestWatchLogPrefixStampsEveryLine(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	at := time.Date(2026, 10, 24, 9, 30, 15, 0, time.UTC)
	tick := 42
	w := newWatchLogPrefixWriter(&buf, testWatchLogFields("gh/entireio/entire-brain", &tick), fixedClock(at))

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
	w := newWatchLogPrefixWriter(&buf, testWatchLogFields("repo", &tick), fixedClock(at))
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
	w := newWatchLogPrefixWriter(&buf, testWatchLogFields("", nil), fixedClock(at))
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
	w := newWatchLogPrefixWriter(&buf, watchLogFields{repo: "repo"}, fixedClock(time.Unix(0, 0).UTC()))
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
	w := newWatchLogPrefixWriter(&buf, testWatchLogFields("repo", nil), fixedClock(time.Unix(0, 0).UTC()))
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
	w := newWatchLogPrefixWriter(&buf, testWatchLogFields("repo", nil), fixedClock(time.Unix(0, 0).UTC()))
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
	w := newWatchLogPrefixWriter(&buf, testWatchLogFields("repo", nil), fixedClock(time.Unix(0, 0).UTC()))
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
	w := newWatchLogPrefixWriter(&buf, testWatchLogFields("repo", nil), func() time.Time {
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
	w := defaultWatchOptions()
	w.once = true

	var buf bytes.Buffer
	var sawSink bool
	stepsFor := func(sink io.Writer) watchSteps {
		sawSink = true
		steps := fakeWatchSteps("ck1:head1", &refreshed, &seeded, &distilled)
		inner := steps.refresh
		steps.refresh = func(c context.Context) error {
			// Stand in for watchDeterministicRefresh's progress reporter,
			// which writes through the command it is handed.
			fmt.Fprintln(sink, "refresh: seed baseline failed")
			return inner(c)
		}
		return steps
	}
	if err := watchLoop(context.Background(), &buf, watchLogFields{pid: 4242, build: "0.2.0", repo: "gh/entireio/entire-brain"},
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
