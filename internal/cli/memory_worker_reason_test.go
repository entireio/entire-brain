package cli

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A worker whose prepass fails relaunches on a 1m..30m backoff for about two
// hours. Recording only THAT it failed turns that into a retry storm with no
// cause attached to any of it -- on this machine every recent worker log read
// `prepass_failed` with nothing else, across six repositories at once.

func TestWorkerLogRecordsWhyAPassFailed(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 11, 22, 18, 49, 0, time.UTC)

	if err := appendMemoryWorkerLogReason(dir, now, "prepass_failed", "5ef3ea5f", "export sessions: permission denied"); err != nil {
		t.Fatalf("append: %v", err)
	}

	line := readWorkerLogLine(t, dir)
	if !strings.Contains(line, "event=prepass_failed") {
		t.Fatalf("the event must survive the reason: %q", line)
	}
	if !strings.Contains(line, "permission denied") {
		t.Fatalf("a failure that records no cause leaves the retry storm undiagnosable: %q", line)
	}
}

// The reader finds the event by scanning strings.Fields for an "event=" prefix,
// so a reason carrying "event=" would be read AS the event, and a newline would
// forge a whole entry. Neither is hypothetical: a Go error wraps whatever a
// child process wrote, including its stderr.
//
// Note which line actually holds which half. strconv.Quote escapes the newline,
// so removing the whitespace collapse leaves this test GREEN -- verified by
// weakening. The collapse is for readability; Quote is the containment. Saying
// otherwise would credit a guard that is not load-bearing.
func TestWorkerLogReasonCannotForgeAnEvent(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 11, 22, 18, 49, 0, time.UTC)

	if err := appendMemoryWorkerLogReason(dir, now, "prepass_failed", "owner1",
		"boom\n2026-10-11T23:00:00Z event=worker_complete owner=forged"); err != nil {
		t.Fatalf("append: %v", err)
	}

	line := readWorkerLogLine(t, dir)
	if strings.Count(line, "\n") != 0 {
		t.Fatalf("a reason must not add lines to the log: %q", line)
	}
	if strings.Contains(line, "event=worker_complete") {
		t.Fatalf("a reason forged an event the worker never emitted: %q", line)
	}
	if !strings.Contains(line, "event=prepass_failed") {
		t.Fatalf("the real event was lost: %q", line)
	}
}

func TestWorkerLogReasonIsBounded(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 11, 22, 18, 49, 0, time.UTC)
	huge := strings.Repeat("x", memoryWorkerLogReasonMax*4)

	if err := appendMemoryWorkerLogReason(dir, now, "prepass_failed", "owner1", huge); err != nil {
		t.Fatalf("append: %v", err)
	}

	line := readWorkerLogLine(t, dir)
	// The log is a bounded ring: an unbounded reason evicts the history it is
	// supposed to explain.
	if len(line) > memoryWorkerLogReasonMax*2 {
		t.Fatalf("an unbounded reason would evict the log it explains: %d bytes", len(line))
	}
	if !strings.Contains(line, "xxx") {
		t.Fatalf("the reason was dropped entirely rather than truncated: %q", line)
	}
}

// An event with no reason must keep the exact line shape it had before, or
// every existing reader and every golden log changes meaning.
func TestWorkerLogWithoutAReasonIsUnchanged(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 11, 22, 18, 49, 0, time.UTC)

	if err := appendMemoryWorkerLog(dir, now, "worker_started", "5ef3ea5f"); err != nil {
		t.Fatalf("append: %v", err)
	}

	line := readWorkerLogLine(t, dir)
	if line != "2026-10-11T22:18:49Z event=worker_started owner=5ef3ea5f" {
		t.Fatalf("the reasonless line shape changed: %q", line)
	}
}

func readWorkerLogLine(t *testing.T, brainDir string) string {
	t.Helper()
	data, present, err := readMemoryStateFile(brainDir, memoryWorkerLogRel, "worker log", memoryWorkerLogMaxBytes)
	if err != nil {
		t.Fatalf("read worker log: %v", err)
	}
	if !present {
		t.Fatal("no worker log was written at all, so this guard would pass on any change")
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected exactly one entry, got %d:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	_ = filepath.Join
	return lines[0]
}

// A byte-level cut can split a multi-byte character, and strconv.Quote then
// renders the trailing fragment as a replacement character rather than text.
// Error strings carry paths and messages that are routinely not ASCII.
// Raised as a finding, and correct.
func TestALongReasonIsTruncatedOnACharacterBoundary(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// A single ASCII byte first, so the limit does NOT land on a rune boundary.
	// Without it 240 is divisible by 3 and a byte-level cut aligns by accident —
	// the first version of this test passed with the bug restored.
	reason := "x" + strings.Repeat("日", memoryWorkerLogReasonMax)
	if memoryWorkerLogReasonMax%3 == 0 && (memoryWorkerLogReasonMax-1)%3 == 0 {
		t.Fatal("fixture cannot land mid-character at this limit, so it would prove nothing")
	}
	if err := appendMemoryWorkerLogReason(dir, time.Now().UTC(), "prepass_failed", "owner1", reason); err != nil {
		t.Fatalf("append: %v", err)
	}
	line := readWorkerLogLine(t, dir)
	if !strings.Contains(line, "日") {
		t.Fatalf("the reason was dropped rather than truncated: %q", line)
	}
	// NOT a replacement character: strconv.Quote escapes an invalid byte as
	// \xNN, so the quoted line stays valid UTF-8 and U+FFFD never appears. The
	// real symptom of a split rune is those escapes. Checking for U+FFFD is why
	// the first two versions of this guard passed with the bug restored.
	if strings.Contains(line, `\x`) {
		t.Fatalf("truncation split a character, leaving escaped bytes in the log: %q", line)
	}
}

// A reason is error text, and error text carries whatever the failing call had
// in hand: a home directory with a username in it, a bearer token from a failed
// request, a key read off disk. The worker log is durable and the reader is
// asked to paste it into an issue, so it gets the same redaction every other
// persisted free-form path here already applies.
//
// Raised as two findings on this PR -- one for the log, one for the `--json`
// field -- and correct: the log's own doc comment still claimed it held no
// error text at all.
func TestTheWorkerLogReasonIsRedacted(t *testing.T) {
	t.Parallel()

	for name, probe := range map[string]struct{ raw, leaked string }{
		"bearer token":  {"request failed: Authorization: Bearer abcDEF123456ghiJKL789", "abcDEF123456ghiJKL789"},
		"github token":  {"push rejected: ghp_0123456789abcdefghijklmnopqrstuvwxyz", "ghp_0123456789abcdefghijklmnopqrstuvwxyz"},
		"secret in env": {"spawn failed: ANTHROPIC_API_KEY=sk-ant-0123456789abcdef", "sk-ant-0123456789abcdef"},
	} {
		got := memoryWorkerLogReason(probe.raw)
		if strings.Contains(got, probe.leaked) {
			t.Errorf("%s: the reason persisted a credential verbatim: %s", name, got)
		}
		if !strings.Contains(got, "REDACTED") {
			t.Errorf("%s: nothing was redacted, so the guard proves nothing: %s", name, got)
		}
	}

	// Redaction must run BEFORE truncation, and the interesting case is a
	// credential the cut lands inside. reGitHubTok needs twenty characters
	// after the prefix, so a fragment shorter than that no longer matches the
	// pattern that recognises it: redacting afterwards would leave `ghp_aaa`
	// sitting in the log, under the length any redactor would catch.
	//
	// The assertion is therefore that NO part of it survives, not that some
	// minimum-length prefix does not -- a floor is exactly how this guard
	// passed against the defect it was written for.
	token := "ghp_" + strings.Repeat("a", 36)

	// Non-vacuity, at a length where the marker itself survives: the token is
	// recognised and replaced rather than merely trimmed off the end. Asserting
	// this inside the loop below instead would fail on the long pads, where the
	// cut lands inside "[REDACTED TOKEN]".
	if short := memoryWorkerLogReason("push rejected: " + token); !strings.Contains(short, "REDACTED") {
		t.Fatalf("the token was not recognised at all, so the loop below proves nothing: %s", short)
	}

	for _, pad := range []int{memoryWorkerLogReasonMax - 8, memoryWorkerLogReasonMax - 16, memoryWorkerLogReasonMax - 30} {
		if got := memoryWorkerLogReason(strings.Repeat("x", pad) + " " + token); strings.Contains(got, "ghp_") {
			t.Errorf("pad %d: a token fragment survived truncation: %s", pad, got)
		}
	}
}

// The same text reaches `entire memory worker --json` as prepass_error, and a
// durable failure reaches it through durablePrepassMessage. Both are pasted
// into issues, so neither may carry a credential the log path strips.
//
// Two assignments feed that field; the finding named one. Redacting at the
// message constructor covers both, and a third caller added later.
func TestTheDurablePrepassMessageIsRedacted(t *testing.T) {
	t.Parallel()

	token := "ghp_" + strings.Repeat("b", 36)
	got := durablePrepassMessage(errors.New("refresh failed for " + token))
	if strings.Contains(got, token) {
		t.Errorf("the durable message carries a credential verbatim: %s", got)
	}
	if !strings.Contains(got, "REDACTED") {
		t.Errorf("nothing was redacted, so this guard proves nothing: %s", got)
	}
	if durablePrepassMessage(nil) != "" {
		t.Error("a nil error must still render as empty")
	}
}
