package cli

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"sync"
	"time"

	"github.com/mattn/go-isatty"
)

var progressCountPattern = regexp.MustCompile(`: \d+/\d+ [^,]+`)

const clearTerminalLine = "\r\033[2K"

type refreshProgress struct {
	out     io.Writer
	prefix  string
	spinner bool
	mu      sync.Mutex
}

type refreshProgressTask struct {
	progress    *refreshProgress
	label       string
	lastPrinted string
	lastStatus  string
	lastPrintAt time.Time
	done        chan struct{}
	stopped     chan struct{}
}

func newRefreshProgress(out io.Writer) *refreshProgress {
	return newProgress(out, "refresh")
}

// newProgress builds a progress reporter whose non-TTY lines are prefixed with
// the given command name (for example "refresh" or "distill"). On a TTY it
// renders a single in-place spinner line instead.
func newProgress(out io.Writer, prefix string) *refreshProgress {
	spinner := false
	if file, ok := out.(*os.File); ok {
		spinner = isatty.IsTerminal(file.Fd())
	}
	return &refreshProgress{
		out:     out,
		prefix:  prefix,
		spinner: spinner,
	}
}

func (p *refreshProgress) Step(label string) func(error) {
	task := p.Begin(label)
	return task.Finish
}

func (p *refreshProgress) Begin(label string) *refreshProgressTask {
	if p == nil || p.out == nil {
		return &refreshProgressTask{}
	}
	task := &refreshProgressTask{
		progress:    p,
		label:       label,
		lastPrinted: label,
		lastStatus:  progressStatusKey(label),
		lastPrintAt: time.Now(),
	}
	if p.spinner {
		task.done = make(chan struct{})
		task.stopped = make(chan struct{})
		task.startSpinner()
		return task
	}
	fmt.Fprintf(p.out, "%s: %s\n", p.prefix, label)
	return task
}

// Skip reports a step that ran but had nothing to do. Like Finish, it renders
// per mode: the TTY form gets its own marker ("-", alongside Finish's "+" and
// "!") so a skipped step doesn't print the non-TTY "prefix:" form into an
// otherwise marker-styled list.
func (p *refreshProgress) Skip(label string) {
	if p == nil || p.out == nil {
		return
	}
	if p.spinner {
		fmt.Fprintf(p.out, "%s- %s skipped\n", clearTerminalLine, label)
		return
	}
	fmt.Fprintf(p.out, "%s: %s skipped\n", p.prefix, label)
}

func (t *refreshProgressTask) Update(label string) {
	if t == nil || t.progress == nil || t.progress.out == nil {
		return
	}
	t.progress.mu.Lock()
	t.label = label
	if t.progress.spinner {
		fmt.Fprintf(t.progress.out, "%s%s", clearTerminalLine, spinnerLine('|', label))
	} else if t.shouldPrintUpdate(label) {
		fmt.Fprintf(t.progress.out, "%s: %s\n", t.progress.prefix, label)
		t.lastPrinted = label
		t.lastStatus = progressStatusKey(label)
		t.lastPrintAt = time.Now()
	}
	t.progress.mu.Unlock()
}

func (t *refreshProgressTask) shouldPrintUpdate(label string) bool {
	if label == t.lastPrinted {
		return false
	}
	status := progressStatusKey(label)
	if status != t.lastStatus {
		return true
	}
	return time.Since(t.lastPrintAt) >= time.Second
}

func (t *refreshProgressTask) Finish(err error) {
	if t == nil || t.progress == nil || t.progress.out == nil {
		return
	}
	if t.done != nil {
		close(t.done)
		<-t.stopped
	}
	t.progress.mu.Lock()
	defer t.progress.mu.Unlock()
	if err != nil {
		if t.progress.spinner {
			fmt.Fprintf(t.progress.out, "%s! %s failed\n", clearTerminalLine, t.label)
			return
		}
		fmt.Fprintf(t.progress.out, "%s: %s failed\n", t.progress.prefix, t.label)
		return
	}
	if t.progress.spinner {
		fmt.Fprintf(t.progress.out, "%s+ %s done\n", clearTerminalLine, t.label)
		return
	}
	fmt.Fprintf(t.progress.out, "%s: %s done\n", t.progress.prefix, t.label)
}

func (t *refreshProgressTask) startSpinner() {
	frames := []rune{'|', '/', '-', '\\'}
	go func() {
		defer close(t.stopped)
		ticker := time.NewTicker(120 * time.Millisecond)
		defer ticker.Stop()
		i := 0
		for {
			select {
			case <-t.done:
				return
			case <-ticker.C:
				t.progress.mu.Lock()
				fmt.Fprintf(t.progress.out, "%s%s", clearTerminalLine, spinnerLine(frames[i%len(frames)], t.label))
				t.progress.mu.Unlock()
				i++
			}
		}
	}()
	t.progress.mu.Lock()
	fmt.Fprintf(t.progress.out, "%s%s", clearTerminalLine, spinnerLine(frames[0], t.label))
	t.progress.mu.Unlock()
}

func spinnerLine(frame rune, label string) string {
	return fmt.Sprintf("%c %s", frame, label)
}

func progressStatusKey(label string) string {
	return progressCountPattern.ReplaceAllString(label, "")
}
