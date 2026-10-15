package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/entireio/entire-brain/internal/tui"
)

// The three hooks below exist for a caller that composes its own settled rows
// (internal/setupui). Each test guards the one property that made the hook
// necessary; see progress.go for why Note and Finish could not serve.

func TestProgressTTYReportsTheRendererCapability(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if newTestProgress(t, &buf, tui.PlainCaps()).TTY() {
		t.Fatal("a plain renderer must not report TTY")
	}
	if !newTestProgress(t, &buf, ttyCaps()).TTY() {
		t.Fatal("a TTY renderer must report TTY")
	}
}

func TestEmitLineWritesTheLineVerbatim(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	const line = "+ Build     instant core ready -- 9 components"
	newTestProgress(t, &buf, tui.PlainCaps()).EmitLine(line)

	if got := buf.String(); got != line+"\n" {
		t.Fatalf("EmitLine must write the line verbatim.\n got: %q\nwant: %q", got, line+"\n")
	}
	// The distinction from NotePhase, which is the whole reason the hook exists.
	if strings.HasPrefix(buf.String(), "  ") {
		t.Fatalf("EmitLine must not indent the way NotePhase does: %q", buf.String())
	}
}

func TestEmitLineDiffersFromNotePhase(t *testing.T) {
	t.Parallel()
	const line = "watcher  not installed"
	var emitted, noted bytes.Buffer
	newTestProgress(t, &emitted, tui.PlainCaps()).EmitLine(line)
	newTestProgress(t, &noted, tui.PlainCaps()).NotePhase(line, tui.PhaseNeutral)
	if emitted.String() == noted.String() {
		t.Fatalf("EmitLine and NotePhase must differ, else the hook is redundant; both gave %q", emitted.String())
	}
}

func TestAbandonAddsNoSettledLine(t *testing.T) {
	t.Parallel()
	// Plain mode announces a task at Begin on purpose, so a log reads in the
	// order things happened; Abandon cannot un-write that and must not try.
	// What it must guarantee is that it adds NOTHING of its own — no " done",
	// no " failed", no composed row. On a TTY that means the spinner row is
	// erased and nothing survives it.
	t.Run("tty leaves no surviving line", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		newTestProgress(t, &buf, ttyCaps()).Begin("fact backfill").Abandon()
		for _, line := range visibleLines(buf.String()) {
			if strings.TrimSpace(stripANSI(line)) != "" {
				t.Fatalf("Abandon must leave no surviving line, got %q", stripANSI(line))
			}
		}
	})
	t.Run("plain adds nothing after Begin", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		p := newTestProgress(t, &buf, tui.PlainCaps())
		task := p.Begin("fact backfill")
		afterBegin := buf.String()
		task.Abandon()
		if buf.String() != afterBegin {
			t.Fatalf("Abandon must add nothing after Begin.\nafter Begin: %q\nafter Abandon: %q", afterBegin, buf.String())
		}
	})
}

func TestAbandonThenFinishAddsNothingEither(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	p := newTestProgress(t, &buf, tui.PlainCaps())
	task := p.Begin("fact backfill")
	afterBegin := buf.String()
	task.Abandon()
	// A second terminal line for one task is exactly the duplicate the
	// finished flag exists to prevent; Abandon must share that flag.
	task.Finish(errors.New("boom"))
	if buf.String() != afterBegin {
		t.Fatalf("Finish after Abandon must stay silent, got %q", buf.String()[len(afterBegin):])
	}
}

func TestFinishStillWritesAFailureWhenNotAbandoned(t *testing.T) {
	t.Parallel()
	// Non-vacuity for the two tests above: the identical setup DOES write when
	// the task finishes normally, so their silence is Abandon's doing and not
	// a buffer that was never going to receive anything.
	//
	// Finish(nil) is NOT the control here: it deliberately stays silent when
	// the label is unchanged since Begin ("needs no second line to say it
	// again with ' done' bolted on"). A FAILURE always prints — silence is
	// never an acceptable way to report one — so that is the control.
	var buf bytes.Buffer
	p := newTestProgress(t, &buf, tui.PlainCaps())
	task := p.Begin("fact backfill")
	afterBegin := buf.String()
	task.Finish(errors.New("boom"))
	if buf.String() == afterBegin {
		t.Fatal("a failed task must add its settled line; if it does not, the Abandon tests above prove nothing")
	}
}
