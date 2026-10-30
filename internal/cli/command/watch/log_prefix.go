package watch

import (
	"bytes"
	"io"
	"os"
	"strconv"
	"sync"
	"time"
)

// LogFields are the facts a watch.log line must carry to be readable on
// its own: when it was written, which process and build wrote it, which tick
// it belongs to, and which repo it is about.
//
// watch.log is the only record of what a background daemon did, and it was
// written as bare `[watch] ...` lines carrying none of that. Diagnosing the
// write.lock race (COR-2175) meant correlating a "reconciliation queued" line
// with a "refresh failed" line further down and GUESSING they were the same
// tick on the same repo -- which was only possible at all because one repo
// dominated that machine's log. Diagnosing the upgrade wedge (COR-2174) meant
// inferring which binary wrote a line from process start times and binary
// mtimes, because the log never said.
//
// repo is empty for workspace-level lines, which belong to no single repo.
// tick is nil for lines written outside a tick (the startup banner); when set
// it is read at write time, so one writer tracks a loop's counter as it
// advances rather than freezing the value it was built with.
type LogFields struct {
	pid   int
	Build string
	Tick  *int
	Repo  string
}

// watchLogPrefixWriter stamps every line written through it with those fields.
//
// The stamp is applied at the writer rather than at the ~30 call sites, so a
// line added later cannot forget it, and per line rather than per write, so a
// two-line write does not leave its second line unattributed.
type watchLogPrefixWriter struct {
	// mu makes this writer safe for concurrent use, which is a contract rather
	// than a repair.
	//
	// No goroutine writes to one of these today, and that was checked rather
	// than assumed: the module's goroutines return everything over channels,
	// agent subprocesses get capped buffers instead of this sink, and the only
	// progress goroutine is gated behind a TTY check that begins by asserting
	// the writer is an *os.File -- which this never is. So there is no live
	// race.
	//
	// The lock is here because of how the writer is SHARED. watchLoop builds
	// one instance and hands it to distill, refresh and seed as both stdout and
	// stderr, and distill owns a --jobs worker pool. Both facts that keep that
	// safe are implicit, and neither is stated at the places that could break
	// it, so one `fmt.Fprintf(cmd.ErrOrStderr(), ...)` added inside a worker
	// would interleave and mis-stamp lines with nothing to catch it. midLine is
	// read and written on every call, so the corruption would be silent
	// mis-attribution rather than a crash.
	//
	// The cost is one uncontended atomic per line on a logging path.
	mu     sync.Mutex
	out    io.Writer
	fields LogFields
	now    func() time.Time
	// midLine records that the previous write ended without a newline, so the
	// continuation of that line is not stamped a second time in its middle.
	midLine bool
}

// NewLogPrefixWriter wraps out so each line gets the watch.log prefix.
// Wrap a given sink once: nesting two of these stamps every line twice.
func NewLogPrefixWriter(out io.Writer, fields LogFields, now func() time.Time) io.Writer {
	if now == nil {
		now = time.Now
	}
	if fields.pid == 0 {
		fields.pid = os.Getpid()
	}
	if fields.Build == "" {
		fields.Build = "dev"
	}
	return &watchLogPrefixWriter{out: out, fields: fields, now: now}
}

// prefix renders one line's stamp. The timestamp is UTC rather than local:
// a workspace daemon's log is read beside logs from other machines, and an
// offset that changes with the reader's zone is one more thing to reconcile.
func (w *watchLogPrefixWriter) prefix() string {
	var b bytes.Buffer
	b.WriteString(w.now().UTC().Format(time.RFC3339))
	b.WriteString(" pid=")
	b.WriteString(strconv.Itoa(w.fields.pid))
	b.WriteString(" build=")
	b.WriteString(w.fields.Build)
	if w.fields.Tick != nil {
		b.WriteString(" tick=")
		b.WriteString(strconv.Itoa(*w.fields.Tick))
	}
	if w.fields.Repo != "" {
		b.WriteString(" repo=")
		b.WriteString(w.fields.Repo)
	}
	b.WriteByte(' ')
	return b.String()
}

func (w *watchLogPrefixWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	// Held across the sink write as well as the state update: releasing before
	// w.out.Write would keep midLine consistent while still letting two
	// writers' bytes interleave inside one line.
	w.mu.Lock()
	defer w.mu.Unlock()
	stamp := w.prefix()
	var buf bytes.Buffer
	for _, line := range bytes.SplitAfter(p, []byte("\n")) {
		// SplitAfter yields a trailing empty element for a newline-terminated
		// input; skip it rather than emit a bare prefix.
		if len(line) == 0 {
			continue
		}
		if !w.midLine {
			buf.WriteString(stamp)
		}
		buf.Write(line)
		w.midLine = line[len(line)-1] != '\n'
	}
	if _, err := w.out.Write(buf.Bytes()); err != nil {
		return 0, err
	}
	// Report the caller's length: the prefix is ours, not theirs, and a short
	// count makes io.Copy and friends treat the write as failed.
	return len(p), nil
}
