package setupui

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"github.com/entireio/entire-brain/internal/tui"
)

// consent.go is phase 4's gate. Distilling sessions into durable facts is the
// only part of setup that spends money, and until this file existed it was
// opt-OUT: a first run distilled unless the operator happened to know about
// --no-backfill.
//
// Two properties matter more than the wording:
//
//   - DEFAULT NO. Bare Enter, EOF, a closed stdin, garbage twice: all skip.
//     Silence is never consent to spend.
//   - NEVER BLOCK WITHOUT A TERMINAL. A prompt in CI is a wedged pipeline, and
//     a wedged pipeline is a worse outcome than any amount of unspent budget.
//     The no-TTY path does not read stdin AT ALL — not a byte, not a peek —
//     which is a stronger and far more testable claim than "it defaults".

// Override is an explicit instruction from a flag, which suppresses the prompt.
type Override int

const (
	// OverrideNone leaves the decision to the prompt.
	OverrideNone Override = iota
	// OverrideYes is --yes: the only way to spend without a human.
	OverrideYes
	// OverrideNo is --no-backfill.
	OverrideNo
)

// ConsentRequest is everything the gate needs. It carries the TTY capability
// explicitly rather than sniffing a file descriptor, so every path through
// this function is reachable from a unit test.
type ConsentRequest struct {
	// Compact uses the short setup prompt instead of the verbose phase block.
	Compact bool
	// Sessions is how many sessions THIS run would distill: the pending pool
	// capped by the spend budget. The cost estimate is derived from it.
	Sessions int
	// Total is the whole pending pool before the budget cap. When it exceeds
	// Sessions, the prompt says both numbers — a reader deciding whether to
	// spend needs to know the pool is 272 even when this run covers 2.
	Total int
	// Covered is how many sessions team memories already cover (fact
	// provenance from the hosted store); Corpus is the whole session count.
	// When Covered > 0 the prompt says so — the reader deciding whether to
	// spend should know most of the corpus needs nothing.
	Covered int
	Corpus  int
	// Agent is the agent CLI that would run. Empty means none was found.
	Agent string
	// Model and Effort qualify the agent, for the informed-decision line.
	Model  string
	Effort string
	// Override is the flag, if one was passed.
	Override Override
	// TTY reports whether a human can answer.
	TTY bool
	// ResumeCommand is the one command that starts a distill later. It is
	// printed on every skip, because a skip that does not say how to change
	// its mind is a dead end.
	ResumeCommand string
}

// ConsentReason is WHY consent landed where it did, as a value.
//
// Reason below is a sentence for a human. Anything that needs to branch on the
// outcome must branch on this instead: the short form has to tell "no agent on
// PATH" apart from "the human said no", and deriving that by matching English
// against Reason breaks the moment the sentence is reworded.
type ConsentReason int

const (
	// ConsentReasonUnset is the zero value; treat it as "not decided here".
	ConsentReasonUnset ConsentReason = iota
	// ConsentReasonNoBackfill is --no-backfill.
	ConsentReasonNoBackfill
	// ConsentReasonNoSessions is a repo with nothing recorded yet.
	ConsentReasonNoSessions
	// ConsentReasonNoAgent is sessions present and no CLI able to read them.
	ConsentReasonNoAgent
	// ConsentReasonFlagYes is --yes.
	ConsentReasonFlagYes
	// ConsentReasonNoTTY is a non-interactive stdin, where silence is not
	// consent.
	ConsentReasonNoTTY
	// ConsentReasonAccepted is a human who answered yes.
	ConsentReasonAccepted
	// ConsentReasonDeclined is a human who answered no.
	ConsentReasonDeclined

	// The four below are reached BEFORE consent is asked, so they are not
	// consent decisions at all -- they are the reasons the question never
	// came up. They live in this enum because the short form needs one typed
	// answer to "why was nothing distilled", and inventing a second enum for
	// half of it would just move the matching problem.

	// ConsentReasonPaused is `brain stop`: background work is paused, and
	// re-running setup will skip again. The remedy is `start`.
	ConsentReasonPaused
	// ConsentReasonAlreadyDistilled is a re-run with nothing pending.
	ConsentReasonAlreadyDistilled
	// ConsentReasonAlreadyRunning is a backfill already in flight. Reporting
	// this as "skipped" contradicts the machine's actual state.
	ConsentReasonAlreadyRunning
	// ConsentReasonNoExecutable is an unresolvable path to this binary, so
	// nothing could be spawned.
	ConsentReasonNoExecutable
)

// ConsentOutcome is the decision and why it was reached.
type ConsentOutcome struct {
	// Spend is the answer.
	Spend bool
	// Reason is always set, including when Spend is true. It is what the
	// settled phase line says.
	Reason string
	// Why is the same decision as a value, for callers that must branch.
	Why ConsentReason
	// Prompted reports whether a human was actually asked. A test asserting
	// "CI does not block" asserts on this, not on the answer.
	Prompted bool
}

// DefaultResumeCommand is the command printed when a distill is skipped.
const DefaultResumeCommand = "entire brain distill"

// Consent decides whether phase 4 may spend.
//
// Order is load bearing. The three "spending is impossible" checks come first,
// so --yes in a repo with no sessions and no agent reports the truth instead of
// claiming it spent. Then the flags, then the terminal. The reader `in` is
// touched on exactly one path: a real prompt on a real terminal.
func Consent(req ConsentRequest, in io.Reader, out io.Writer, r *Renderer) ConsentOutcome {
	resume := req.ResumeCommand
	if strings.TrimSpace(resume) == "" {
		resume = DefaultResumeCommand
	}

	switch {
	// Nothing to distill comes FIRST. The flag declines work; it cannot
	// decline work that does not exist, and "not distilled (--no-backfill)"
	// to someone whose repo has no captured sessions reports a decision
	// instead of explaining the state.
	case req.Sessions <= 0:
		return ConsentOutcome{Reason: "no sessions to distill; the brain is queryable now", Why: ConsentReasonNoSessions}
	case req.Override == OverrideNo:
		return ConsentOutcome{Reason: "not distilled (--no-backfill); start later: " + resume, Why: ConsentReasonNoBackfill}
	case req.Agent == "":
		return ConsentOutcome{Reason: "no agent CLI on PATH, nothing can distill; " + RemedyAgent, Why: ConsentReasonNoAgent}
	case req.Override == OverrideYes:
		return ConsentOutcome{Spend: true, Reason: "distilling " + plural(req.Sessions, "session") + " (--yes)", Why: ConsentReasonFlagYes}
	case !req.TTY:
		// Not a prompt, not a read, not a wait: one line and a default.
		return ConsentOutcome{Reason: "not distilled: no terminal to ask, and silence is not consent; run " + resume + " or pass --yes", Why: ConsentReasonNoTTY}
	}

	blockLines := writePrompt(out, r, req)
	answer, prompted, promptLines := askTwice(in, out, r, req.Compact)
	erasePrompt(out, blockLines+promptLines)
	if answer {
		return ConsentOutcome{Spend: true, Reason: "distilling " + plural(req.Sessions, "session") + ", newest first", Why: ConsentReasonAccepted, Prompted: prompted}
	}
	return ConsentOutcome{Reason: "not distilled; the brain is queryable now, start later: " + resume, Why: ConsentReasonDeclined, Prompted: prompted}
}

// writePrompt paints the block the operator is answering. It uses the same
// gutter as every phase line, with the glyph column left blank: a question is
// not an outcome and must not wear an outcome's tick.
func writePrompt(out io.Writer, r *Renderer, req ConsentRequest) int {
	if out == nil {
		return 0
	}
	bullet := " " + r.Bullet() + " "
	agent := "Agent: " + req.Agent
	if req.Model != "" {
		agent += bullet + "model: " + req.Model
	}
	if req.Effort != "" {
		agent += bullet + "effort: " + req.Effort
	}
	if req.Compact {
		pool := max(req.Total, req.Sessions)
		verb := "remain"
		if pool == 1 {
			verb = "remains"
		}
		// The question is its own block: a blank line keeps it from reading as
		// one more status row. Counted, so the erase removes it too.
		fmt.Fprintln(out)
		count := 1
		for i, text := range []string{
			fmt.Sprintf("%s %s. Distill up to %d now?", plural(pool, "session"), verb, req.Sessions),
			agent + bullet + "uses agent tokens",
		} {
			for _, line := range r.Wrap(text) {
				if i > 0 {
					line = r.TUI().Paint(tui.CLIMuted, line)
				}
				fmt.Fprintf(out, "  %s\n", line)
				count++
			}
		}
		return count
	}
	pool := plural(req.Sessions, "session")
	estimate := "Estimated: ~" + plural(req.Sessions, "agent call") + ", newest first, resumable."
	if req.Total > req.Sessions {
		pool = plural(req.Total, "session")
		estimate = fmt.Sprintf("Estimated: ~%s this run (spend budget), newest first, resumable.",
			plural(req.Sessions, "agent call"))
	}
	lines := []Line{
		{Title: PhaseFacts.Title(), Text: pool + " can be distilled into durable facts.", Accent: PhaseFacts.Accent()},
	}
	if req.Covered > 0 {
		lines = append(lines, Line{
			Text:   fmt.Sprintf("%d of %d sessions are already covered by team memories.", req.Covered, req.Corpus),
			Accent: PhaseFacts.Accent(),
		})
	}
	lines = append(lines,
		Line{Text: agent, Accent: PhaseFacts.Accent()},
		Line{Text: estimate, Accent: PhaseFacts.Accent()},
		Line{Blank: true},
	)
	for _, line := range lines {
		fmt.Fprintln(out, r.Line(line))
	}
	return len(lines)
}

// erasePrompt clears the consent block once it is answered. The question must
// be ASKED — it gates token spend — but it does not have to be KEPT: after the
// answer the block is history, and the final summary should be the only output
// left on the operator's terminal. Only the real-terminal path reaches this,
// so the ANSI is safe; lines counts the block plus one line per prompt attempt
// (the operator's Enter completed each).
func erasePrompt(out io.Writer, lines int) {
	if out == nil || lines <= 0 {
		return
	}
	fmt.Fprintf(out, "\x1b[%dA\x1b[J", lines)
}

// stripTerminalNoise removes escape sequences and control bytes the TERMINAL
// put on the line, not the person: arrow keys pressed while an earlier phase
// ran, and query replies (cursor position, background color) a full-screen
// prompt left in the stdin buffer. Observed live: "\x1b[B\x1b[Bn" — a plain n
// after two fidgety arrow presses — was rejected as garbage and the question
// re-asked.
func stripTerminalNoise(line string) string {
	var b strings.Builder
	for i := 0; i < len(line); i++ {
		c := line[i]
		if c == 0x1b {
			i++
			if i >= len(line) {
				break
			}
			switch line[i] {
			case '[': // CSI: parameter bytes, then one final byte in 0x40–0x7e
				for i++; i < len(line) && (line[i] < 0x40 || line[i] > 0x7e); i++ {
				}
			case ']': // OSC: runs to BEL or ESC-backslash
				for i++; i < len(line); i++ {
					if line[i] == 0x07 {
						break
					}
					if line[i] == 0x1b && i+1 < len(line) && line[i+1] == '\\' {
						i++
						break
					}
				}
			case 'O': // SS3: one final byte
				i++
			}
			continue
		}
		if c < 0x20 || c == 0x7f {
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// question is the prompt itself. "[y/N]" is not decoration: the capital is the
// default, and it is the default.
const question = "Distill now? [y/N]"

// askTwice reads an answer, re-asking ONCE on something unrecognisable and
// defaulting to No after that. Two chances is the whole budget: a loop here is
// a different way of blocking a pipeline.
func askTwice(in io.Reader, out io.Writer, r *Renderer, compact bool) (answer, prompted bool, lines int) {
	if in == nil {
		return false, false, 0
	}
	reader := bufio.NewReader(in)
	promptLine := func(text string) string {
		if compact {
			return "  " + text
		}
		return r.Line(Line{Text: text, Accent: PhaseFacts.Accent()})
	}
	for attempt := 0; attempt < 2; attempt++ {
		// Printed with a trailing space Line would have trimmed: the cursor has to
		// sit clear of the "]" or the answer reads as part of the question.
		fmt.Fprint(out, promptLine(question)+" ")
		// The operator's Enter (or the explicit newline on EOF) completes the
		// prompt line, so each attempt occupies exactly one line for the erase.
		lines++
		line, err := reader.ReadString('\n')
		got := strings.ToLower(strings.TrimSpace(stripTerminalNoise(line)))
		if err != nil && got == "" {
			// EOF, a closed stdin, an interrupt that shut the pipe: No, with a
			// newline so the next line does not land beside the prompt.
			fmt.Fprintln(out)
			return false, true, lines
		}
		switch got {
		case "y", "yes":
			return true, true, lines
		case "", "n", "no":
			return false, true, lines
		}
		if attempt == 0 {
			fmt.Fprintln(out, promptLine("Please answer y or n."))
			lines++
		}
		if err != nil {
			fmt.Fprintln(out)
			return false, true, lines
		}
	}
	return false, true, lines
}
