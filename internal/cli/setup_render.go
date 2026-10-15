package cli

import (
	"sync"
	"time"

	"github.com/entireio/entire-brain/internal/tui"
)

// setup_render.go is the presentation half of `setup`. It is deliberately
// separate from setup.go: the orchestration there decides WHAT happened, this
// decides how it looks, and the two must not be able to drift into each other.
//
// Everything here obeys the same three rules:
//
//   - The report is never re-derived. Every line is rendered from the
//     setupReport that --json would emit, so the text and the JSON can never
//     disagree about an outcome.
//   - Timings are render-only. They are NOT added to setupReport, because
//     `setup --json` is a published contract and a new field in it is a
//     breaking change for anything parsing it today.
//   - Nothing here decides whether it may use colour. The renderer already
//     resolved that from the destination stream.

// setupPhaseTiming is how long one phase took. Render-only; see above.
type setupPhaseTiming struct {
	Name    string
	Elapsed time.Duration
}

// setupTimings records per-phase durations in phase order.
type setupTimings struct {
	mu      sync.Mutex
	entries []setupPhaseTiming
}

func (t *setupTimings) record(name string, elapsed time.Duration) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.entries = append(t.entries, setupPhaseTiming{Name: name, Elapsed: elapsed})
}

func (t *setupTimings) get(name string) (time.Duration, bool) {
	if t == nil {
		return 0, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, entry := range t.entries {
		if entry.Name == name {
			return entry.Elapsed, true
		}
	}
	return 0, false
}

func (t *setupTimings) total() time.Duration {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	var sum time.Duration
	for _, entry := range t.entries {
		sum += entry.Elapsed
	}
	return sum
}

// setupComponentObserver lets the instant phase report components AS THEY LAND
// rather than only in the slice it returns at the end. Without it the tick row
// could not exist: a phase that takes twenty seconds and then reveals six
// outcomes at once is exactly the experience this work is replacing.
//
// It is a struct rather than a plain callback because the observer has to be
// handed to resolveSetupSteps (which builds the instant closure) BEFORE the
// progress task it feeds exists.
type setupComponentObserver struct {
	mu sync.Mutex
	fn func(setupComponent)
	// observed is every component forwarded so far. It exists so a test can
	// show it HAD sub-steps before asserting none of them became a line: an
	// assertion over an empty set passes for the wrong reason.
	observed []setupComponent
}

func (o *setupComponentObserver) bind(fn func(setupComponent)) {
	if o == nil {
		return
	}
	o.mu.Lock()
	o.fn = fn
	o.mu.Unlock()
}

func (o *setupComponentObserver) notify(component setupComponent) {
	if o == nil {
		return
	}
	o.mu.Lock()
	fn := o.fn
	o.observed = append(o.observed, component)
	o.mu.Unlock()
	if fn != nil {
		fn(component)
	}
}

// setupComponentMark maps a component outcome onto a status glyph.
func setupComponentMark(state string) tui.Mark {
	switch state {
	case "ok", "built":
		return tui.MarkDone
	case "failed":
		return tui.MarkFailed
	case "skipped", "missing":
		return tui.MarkSkipped
	default:
		return tui.MarkPending
	}
}

// setupPhaseMark maps a phase state onto a glyph.
func setupPhaseMark(state string) tui.Mark {
	switch state {
	case "ok":
		return tui.MarkDone
	case "degraded", "failed":
		return tui.MarkFailed
	case "skipped":
		return tui.MarkSkipped
	default:
		return tui.MarkPending
	}
}

func roundedDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	if d < time.Second {
		return d.Round(10 * time.Millisecond).String()
	}
	return d.Round(100 * time.Millisecond).String()
}
