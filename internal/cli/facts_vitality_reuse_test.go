package cli

import (
	"testing"
	"time"
)

// The reasoning-capture exit test is "a memory served on more than one distinct
// day". The per-event log cannot answer it for long: an append past
// factsVitalityLogMaxBytes folds the log into the rollup and TRUNCATES it, so
// the timestamps a day-counting query reads are destroyed. Measured
// 2026-10-31: 178.8KiB of a 1MiB cap at 666 events, so compaction fires around
// 3,900 — days away once the hooks deliver, which is exactly when the metric
// is meant to show its result.
//
// A span answers it exactly rather than approximately, which is the claim this
// test exists to hold.
func TestSurfaceSpanAnswersMoreThanOneDistinctDay(t *testing.T) {
	t.Parallel()

	day := func(d int, hour int) time.Time {
		return time.Date(2026, 10, d, hour, 0, 0, 0, time.UTC)
	}
	for name, tc := range map[string]struct {
		span vitalitySurfaceSpan
		want bool
	}{
		"never served":       {vitalitySurfaceSpan{}, false},
		"one serve":          {vitalitySurfaceSpan{First: day(1, 9), Last: day(1, 9)}, false},
		"twice the same day": {vitalitySurfaceSpan{First: day(1, 9), Last: day(1, 23)}, false},
		"across midnight":    {vitalitySurfaceSpan{First: day(1, 23), Last: day(2, 1)}, true},
		"days apart":         {vitalitySurfaceSpan{First: day(1, 9), Last: day(9, 9)}, true},
		"only a first":       {vitalitySurfaceSpan{First: day(1, 9)}, false},
		"only a last":        {vitalitySurfaceSpan{Last: day(1, 9)}, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := tc.span.MultiDay(); got != tc.want {
				t.Errorf("MultiDay() = %v, want %v for %+v", got, tc.want, tc.span)
			}
		})
	}
}

// The spans must be recorded as events fold in, or the metric has no input.
func TestMergeVitalityEventsRecordsPerSurfaceSpans(t *testing.T) {
	t.Parallel()

	rollup := &vitalityRollup{Facts: map[string]*vitalityFactRollup{}}
	at := func(d int) time.Time { return time.Date(2026, 10, d, 12, 0, 0, 0, time.UTC) }
	mergeVitalityEvents(rollup, []vitalityEvent{
		{FactID: "fact:a", Surface: "hook-pre-edit", At: at(1)},
		{FactID: "fact:a", Surface: "hook-pre-edit", At: at(4)},
		{FactID: "fact:a", Surface: "recall", At: at(2)},
		{FactID: "fact:b", Surface: "recall", At: at(1)},
		{FactID: "fact:b", Surface: "recall", At: at(1)},
	})

	a := rollup.Facts["fact:a"]
	if a == nil {
		t.Fatal("no entry for fact:a")
	}
	if !a.SurfaceSpans["hook-pre-edit"].MultiDay() {
		t.Errorf("hook-pre-edit span %+v must be multi-day", a.SurfaceSpans["hook-pre-edit"])
	}
	// One recall serve is not reuse, and must not borrow the hook's span.
	if a.SurfaceSpans["recall"].MultiDay() {
		t.Errorf("a single recall serve must not read as multi-day: %+v", a.SurfaceSpans["recall"])
	}
	// Two serves on one day are not reuse.
	if b := rollup.Facts["fact:b"]; b == nil || b.SurfaceSpans["recall"].MultiDay() {
		t.Errorf("two serves on one day must not read as multi-day: %+v", b)
	}
}

// THE CONFOUND THIS PINS. The overall figure is movable by an agent querying
// more: measured here, it rose 2.1% -> 2.89% inside one session purely from
// `recall`. An agent asking twice is the re-asking the milestone exists to
// remove, so the delivery-restricted figure is the honest one and the two must
// never collapse into a single number.
func TestReuseSeparatesDeliveryFromPull(t *testing.T) {
	t.Parallel()

	at := func(d int) time.Time { return time.Date(2026, 10, d, 12, 0, 0, 0, time.UTC) }
	multi := func(surface string) map[string]vitalitySurfaceSpan {
		return map[string]vitalitySurfaceSpan{surface: {First: at(1), Last: at(5)}}
	}

	// Three memories: one reused only by pull, one by a delivery surface, one
	// not reused at all.
	rollup := &vitalityRollup{Facts: map[string]*vitalityFactRollup{
		"pull-only": {FirstServed: at(1), LastServed: at(5), SurfaceSpans: multi("recall")},
		"delivered": {FirstServed: at(1), LastServed: at(5), SurfaceSpans: multi("hook-prompt")},
		"once":      {FirstServed: at(1), LastServed: at(1), SurfaceSpans: map[string]vitalitySurfaceSpan{"recall": {First: at(1), Last: at(1)}}},
	}}

	summary := factsVitalitySummary{}
	for _, entry := range rollup.Facts {
		reusedAny, reusedDelivery := false, false
		for surface, span := range entry.SurfaceSpans {
			if !span.MultiDay() {
				continue
			}
			reusedAny = true
			if vitalityDeliverySurfaces[surface] {
				reusedDelivery = true
			}
		}
		if reusedAny {
			summary.ReusedAnySurface++
		}
		if reusedDelivery {
			summary.ReusedViaDelivery++
		}
	}

	if summary.ReusedAnySurface != 2 {
		t.Errorf("reused on any surface = %d, want 2", summary.ReusedAnySurface)
	}
	if summary.ReusedViaDelivery != 1 {
		t.Errorf("reused via delivery = %d, want 1 (the pull-only memory must not count)", summary.ReusedViaDelivery)
	}
}

// `recall`, `query` and the MCP surface are PULLS and must never be counted as
// delivery, or the exit test inflates on exactly the behaviour it measures the
// absence of. And the set is named, not prefix-matched: a future `hook-`
// surface that merely logs must not be counted by accident.
func TestDeliverySurfacesExcludeEveryPullSurface(t *testing.T) {
	t.Parallel()

	for _, pull := range []string{"recall", "query", "mcp:brain_search", "inspect", ""} {
		if vitalityDeliverySurfaces[pull] {
			t.Errorf("%q is a pull surface and must not count as delivery", pull)
		}
	}
	for _, delivery := range []string{"hook-pre-edit", "hook-post-failure", "hook-prompt", "brief"} {
		if !vitalityDeliverySurfaces[delivery] {
			t.Errorf("%q delivers unrequested and must count", delivery)
		}
	}
	// The three wired hook verbs must each be present, or a surface that is
	// live in the product is invisible to the metric.
	if len(vitalityDeliverySurfaces) != 4 {
		t.Errorf("the delivery set has %d entries; add new surfaces deliberately rather than by prefix", len(vitalityDeliverySurfaces))
	}
}
