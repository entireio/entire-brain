package cli

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// explainFanoutRunner answers `checkpoint explain` from a fixture and records
// how many children were ever in flight at once. Unlike fakeCommandRunner it
// locks its own bookkeeping, which is what lets it claim the concurrency
// capability; fakeCommandRunner appends to an unsynchronised slice and must
// never claim it.
type explainFanoutRunner struct {
	mu          sync.Mutex
	details     map[string]string
	failures    map[string]error
	calls       int
	inflight    int
	maxInflight int

	// barrier releases every waiter once width children are in flight at the
	// same time. A sequential caller never fills it and each call falls
	// through after barrierWait instead of hanging.
	barrierWidth int
	barrier      chan struct{}

	honorContext bool
	concurrent   bool
}

const explainFanoutBarrierWait = 3 * time.Second

func newExplainFanoutRunner(details map[string]string) *explainFanoutRunner {
	return &explainFanoutRunner{details: details, failures: map[string]error{}, concurrent: true}
}

func (r *explainFanoutRunner) withBarrier(width int) *explainFanoutRunner {
	r.barrierWidth = width
	r.barrier = make(chan struct{})
	return r
}

func (r *explainFanoutRunner) Run(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
	if r.honorContext {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
	}
	id := args[len(args)-1]

	r.mu.Lock()
	r.calls++
	r.inflight++
	if r.inflight > r.maxInflight {
		r.maxInflight = r.inflight
	}
	reached := r.barrier != nil && r.inflight >= r.barrierWidth
	r.mu.Unlock()

	if r.barrier != nil {
		if reached {
			// Closing is idempotent only once, so guard it under the lock.
			r.mu.Lock()
			select {
			case <-r.barrier:
			default:
				close(r.barrier)
			}
			r.mu.Unlock()
		} else {
			select {
			case <-r.barrier:
			case <-time.After(explainFanoutBarrierWait):
			}
		}
	}

	r.mu.Lock()
	r.inflight--
	r.mu.Unlock()

	if err, ok := r.failures[id]; ok {
		return nil, nil, err
	}
	stdout, ok := r.details[id]
	if !ok {
		return nil, nil, fmt.Errorf("no fixture for %s", id)
	}
	return []byte(stdout), nil, nil
}

func (r *explainFanoutRunner) ConcurrentRunAllowed() bool { return r.concurrent }

func (r *explainFanoutRunner) observed() (calls, maxInflight int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls, r.maxInflight
}

func fanoutCheckpointFixture(n int) ([]checkpointListEntry, map[string]string) {
	checkpoints := make([]checkpointListEntry, 0, n)
	details := make(map[string]string, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("cp%04d", i)
		checkpoints = append(checkpoints, checkpointListEntry{
			CheckpointID: id,
			IsLogsOnly:   true,
			Date:         time.Date(2026, 1, 1, 0, 0, i, 0, time.UTC),
		})
		details[id] = fmt.Sprintf(
			`{"checkpoint_id":%q,"branch":"main","sessions":[{"index":0,"session_id":"session-%d","branch":"main"}]}`,
			id, i)
	}
	return checkpoints, details
}

// The fan-out is allowed to move the reads and nothing else. Running the same
// fixture through the concurrent path and the sequential one must produce the
// same selection, the same warnings in the same order, and the same
// completeness verdict — otherwise the export's output depends on which child
// finished first.
func TestRoutedDetailFanoutMatchesTheSequentialSelection(t *testing.T) {
	t.Parallel()
	checkpoints, details := fanoutCheckpointFixture(24)

	// A spread of the cases the pass decides between, so the comparison covers
	// the warning text and ordering rather than only the happy path.
	checkpoints[3].IsLogsOnly = false
	checkpoints[7].CheckpointID = ""
	details["cp0011"] = `{"checkpoint_id":"cp0011","branch":"main","sessions":[{"index":0,"error":"detail said no"}]}`
	details["cp0013"] = `{"checkpoint_id":"cp0013","branch":"main","sessions":[{"index":0,"session_id":"","branch":"main"}]}`
	// A collision: two checkpoints offering the same branch+session, so
	// shouldReplaceSession has to pick one.
	details["cp0017"] = `{"checkpoint_id":"cp0017","branch":"main","sessions":[{"index":0,"session_id":"session-5","branch":"main"}]}`

	// The barrier makes the overlap deterministic: the first two children to
	// arrive release it for good, so the differential cannot pass by accident
	// on a host where the fixture answers faster than a worker starts.
	concurrent := newExplainFanoutRunner(details).withBarrier(2)
	concurrent.failures["cp0019"] = errors.New("unreadable detail")
	sequential := newExplainFanoutRunner(details)
	sequential.failures["cp0019"] = errors.New("unreadable detail")
	sequential.concurrent = false

	gotSelected, gotWarnings, gotComplete, gotErr := selectRoutedCheckpointSessions(
		context.Background(), concurrent, "/repo", "entire-test", checkpoints, checkpointBranchDestinations{}, nil, nil)
	wantSelected, wantWarnings, wantComplete, wantErr := selectRoutedCheckpointSessions(
		context.Background(), sequential, "/repo", "entire-test", checkpoints, checkpointBranchDestinations{}, nil, nil)

	if gotErr != nil || wantErr != nil {
		t.Fatalf("fixture was expected to survive: concurrent err:%v sequential err:%v", gotErr, wantErr)
	}
	if _, maxInflight := concurrent.observed(); maxInflight < 2 {
		if checkpointDetailFanoutLimit() >= 2 {
			t.Fatalf("the concurrent arm never overlapped, so this proved nothing: maxInflight=%d", maxInflight)
		}
		t.Logf("fan-out is off on this host; the arms are the same path")
	}
	if _, maxInflight := sequential.observed(); maxInflight != 1 {
		t.Fatalf("the sequential arm overlapped: maxInflight=%d", maxInflight)
	}
	if gotComplete != wantComplete {
		t.Fatalf("completeness differed: concurrent=%v sequential=%v", gotComplete, wantComplete)
	}
	if !reflect.DeepEqual(gotWarnings, wantWarnings) {
		t.Fatalf("warnings differed:\nconcurrent: %v\nsequential: %v", gotWarnings, wantWarnings)
	}
	if !reflect.DeepEqual(gotSelected, wantSelected) {
		t.Fatalf("selection differed:\nconcurrent: %+v\nsequential: %+v", gotSelected, wantSelected)
	}
	if len(gotSelected) == 0 || len(gotWarnings) == 0 {
		t.Fatalf("fixture exercised neither selection nor warnings: selected=%d warnings=%d", len(gotSelected), len(gotWarnings))
	}
}

// The point of the change: the detail reads actually run several at a time.
// The barrier only opens once two children are in flight together, so a pass
// that still reads them one by one cannot fill it.
func TestRoutedDetailFanoutReadsSeveralCheckpointsAtOnce(t *testing.T) {
	t.Parallel()
	if checkpointDetailFanoutLimit() < 2 {
		t.Skipf("fan-out is off on this host: limit is %d", checkpointDetailFanoutLimit())
	}
	checkpoints, details := fanoutCheckpointFixture(8)
	runner := newExplainFanoutRunner(details).withBarrier(2)

	selected, warnings, complete, err := selectRoutedCheckpointSessions(
		context.Background(), runner, "/repo", "entire-test", checkpoints, checkpointBranchDestinations{}, nil, nil)
	if err != nil || !complete || len(selected) != 8 {
		t.Fatalf("fan-out selection = selected:%d warnings:%v complete:%v err:%v", len(selected), warnings, complete, err)
	}
	calls, maxInflight := runner.observed()
	if calls != 8 {
		t.Fatalf("expected one detail read per checkpoint, got %d", calls)
	}
	if maxInflight < 2 {
		t.Fatalf("detail reads never overlapped: maxInflight=%d", maxInflight)
	}
}

// The capability is an opt-in, not an inherited seam. A runner that only
// implements Run — every fixture runner in this package, each of them keeping
// unsynchronised call records — must still be read one checkpoint at a time.
func TestRoutedDetailFanoutSkipsARunnerThatHasNotOptedIn(t *testing.T) {
	t.Parallel()
	checkpoints, details := fanoutCheckpointFixture(8)

	declined := newExplainFanoutRunner(details)
	declined.concurrent = false
	if got := prefetchCheckpointDetails(context.Background(), declined, "/repo", "entire-test", checkpoints, nil, nil); got != nil {
		t.Fatalf("a runner answering ConcurrentRunAllowed()=false was fanned out: %d results", len(got))
	}

	// runOnlyCommandRunner forwards nothing but Run, which is how a wrapper
	// drops a capability it must not claim on the inner runner's behalf.
	opaque := runOnlyCommandRunner{inner: &fakeCommandRunner{}}
	if got := prefetchCheckpointDetails(context.Background(), opaque, "/repo", "entire-test", checkpoints, nil, nil); got != nil {
		t.Fatalf("a runner with no concurrency capability was fanned out: %d results", len(got))
	}
}

// A position no worker reached must not read back as a successful empty
// detail. Cancelling before the pass starts leaves the queue unfinished, and
// every requested checkpoint still has to be accounted for — as a warning and
// an incomplete verdict, never as a silent drop.
func TestRoutedDetailFanoutDoesNotTreatAnUnreachedPositionAsRead(t *testing.T) {
	t.Parallel()
	checkpoints, details := fanoutCheckpointFixture(8)
	runner := newExplainFanoutRunner(details)
	runner.honorContext = true

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	results := prefetchCheckpointDetails(ctx, runner, "/repo", "entire-test", checkpoints, nil, nil)
	for i, result := range results {
		if result.fetched && result.err == nil {
			t.Fatalf("position %d reported a successful read under a cancelled context", i)
		}
		if !result.fetched && (result.err != nil || result.detail.CheckpointID != "") {
			t.Fatalf("position %d carried a result it never fetched: %+v", i, result)
		}
	}

	selected, warnings, complete, err := selectRoutedCheckpointSessions(
		ctx, runner, "/repo", "entire-test", checkpoints, checkpointBranchDestinations{}, nil, nil)
	if err == nil {
		t.Fatalf("a cancelled pass with no readable detail must fail: selected:%d warnings:%v complete:%v", len(selected), warnings, complete)
	}
	if !strings.Contains(err.Error(), checkpointScopeIncompleteCode) {
		t.Fatalf("cancelled pass did not report an incomplete scope: %v", err)
	}
	if len(selected) != 0 || complete {
		t.Fatalf("cancelled pass reported work it did not do: selected:%d complete:%v", len(selected), complete)
	}
}

// Progress has to keep moving while the reads are in flight, because that is
// where the hours go; and it must not walk backwards afterwards, which is what
// a second pass over the same list would do.
func TestRoutedDetailFanoutReportsProgressWhileReading(t *testing.T) {
	t.Parallel()
	if checkpointDetailFanoutLimit() < 2 {
		t.Skipf("fan-out is off on this host: limit is %d", checkpointDetailFanoutLimit())
	}
	checkpoints, details := fanoutCheckpointFixture(8)
	runner := newExplainFanoutRunner(details)

	// Deliberately unsynchronised, because that is what a progress sink in
	// this package is: the fan-out holds its own lock across the callback, so
	// two workers can never be inside this append at once. Take that lock away
	// and the race detector says so.
	var current []int
	progress := func(p exportProgress) {
		if p.Total != 8 {
			t.Errorf("progress reported against the wrong total: %d", p.Total)
		}
		current = append(current, p.Current)
	}

	if _, _, _, err := selectRoutedCheckpointSessions(
		context.Background(), runner, "/repo", "entire-test", checkpoints, checkpointBranchDestinations{}, progress, nil); err != nil {
		t.Fatalf("fan-out selection failed: %v", err)
	}

	// Safe to read: prefetchCheckpointDetails joined every worker before it
	// returned.
	if len(current) < 8 {
		t.Fatalf("progress was reported %d times for 8 checkpoints: %v", len(current), current)
	}
	for i := 1; i < len(current); i++ {
		if current[i] < current[i-1] {
			t.Fatalf("progress walked backwards at %d: %v", i, current)
		}
	}
	if current[len(current)-1] != 8 {
		t.Fatalf("progress did not finish at the total: %v", current)
	}
}
