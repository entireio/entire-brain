package setupui

import (
	"bufio"
	"fmt"
	"io"
	"strings"
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
	// Sessions is how many sessions could be distilled.
	Sessions int
	// Agent is the agent CLI that would run. Empty means none was found.
	Agent string
	// Model and Effort qualify the agent, for the informed-decision line.
	Model  string
	Effort string
	// Override is the flag, if one was passed.
	Override Override
	// DeclineRemembered reports that OverrideNo came from a decline recorded
	// by an EARLIER run rather than a flag typed on this one. setup resolves
	// the two into one boolean before anything downstream sees it, so without
	// this the skip line credits a flag the reader never typed — and then the
	// one command that would change it (--backfill) goes unmentioned.
	DeclineRemembered bool
	// TTY reports whether a human can answer.
	TTY bool
	// ResumeCommand is the one command that starts a distill later. It is
	// printed on every skip, because a skip that does not say how to change
	// its mind is a dead end.
	ResumeCommand string
}

// ConsentOutcome is the decision and why it was reached.
type ConsentOutcome struct {
	// Spend is the answer.
	Spend bool
	// Reason is always set, including when Spend is true. It is what the
	// settled phase line says.
	Reason string
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
	case req.Override == OverrideNo:
		if req.DeclineRemembered {
			return ConsentOutcome{Reason: "not distilled (you declined on an earlier run; --backfill starts it); start later: " + resume}
		}
		return ConsentOutcome{Reason: "not distilled (--no-backfill); start later: " + resume}
	case req.Sessions <= 0:
		return ConsentOutcome{Reason: "no sessions to distill; the brain is queryable now"}
	case req.Agent == "":
		return ConsentOutcome{Reason: "no agent CLI on PATH, nothing can distill; " + RemedyAgent}
	case req.Override == OverrideYes:
		return ConsentOutcome{Spend: true, Reason: "distilling " + plural(req.Sessions, "session") + " (--yes)"}
	case !req.TTY:
		// Not a prompt, not a read, not a wait: one line and a default.
		return ConsentOutcome{Reason: "not distilled: no terminal to ask, and silence is not consent; run " + resume + " or pass --yes"}
	}

	writePrompt(out, r, req)
	answer, prompted := askTwice(in, out, r)
	if answer {
		return ConsentOutcome{Spend: true, Reason: "distilling " + plural(req.Sessions, "session") + ", newest first", Prompted: prompted}
	}
	return ConsentOutcome{Reason: "not distilled; the brain is queryable now, start later: " + resume, Prompted: prompted}
}

// writePrompt paints the block the operator is answering. It uses the same
// gutter as every phase line, with the glyph column left blank: a question is
// not an outcome and must not wear an outcome's tick.
func writePrompt(out io.Writer, r *Renderer, req ConsentRequest) {
	if out == nil {
		return
	}
	bullet := " " + r.Bullet() + " "
	agent := "Agent: " + req.Agent
	if req.Model != "" {
		agent += bullet + "model: " + req.Model
	}
	if req.Effort != "" {
		agent += bullet + "effort: " + req.Effort
	}
	lines := []Line{
		{Title: PhaseFacts.Title(), Text: plural(req.Sessions, "session") + " can be distilled into durable facts.", Accent: PhaseFacts.Accent()},
		{Text: agent, Accent: PhaseFacts.Accent()},
		{Text: "Estimated: ~" + plural(req.Sessions, "agent call") + ", newest first, resumable.", Accent: PhaseFacts.Accent()},
		{Blank: true},
	}
	for _, line := range lines {
		fmt.Fprintln(out, r.Line(line))
	}
}

// question is the prompt itself. "[y/N]" is not decoration: the capital is the
// default, and it is the default.
const question = "Distill now? [y/N]"

// askTwice reads an answer, re-asking ONCE on something unrecognisable and
// defaulting to No after that. Two chances is the whole budget: a loop here is
// a different way of blocking a pipeline.
func askTwice(in io.Reader, out io.Writer, r *Renderer) (bool, bool) {
	if in == nil {
		return false, false
	}
	reader := bufio.NewReader(in)
	for attempt := 0; attempt < 2; attempt++ {
		// Printed with a trailing space Line would have trimmed: the cursor has to
		// sit clear of the "]" or the answer reads as part of the question.
		fmt.Fprint(out, r.Line(Line{Text: question, Accent: PhaseFacts.Accent()})+" ")
		line, err := reader.ReadString('\n')
		answer := strings.ToLower(strings.TrimSpace(line))
		if err != nil && answer == "" {
			// EOF, a closed stdin, an interrupt that shut the pipe: No, with a
			// newline so the next line does not land beside the prompt.
			fmt.Fprintln(out)
			return false, true
		}
		switch answer {
		case "y", "yes":
			return true, true
		case "", "n", "no":
			return false, true
		}
		if attempt == 0 {
			fmt.Fprintln(out, r.Line(Line{Text: "Please answer y or n.", Accent: PhaseFacts.Accent()}))
		}
		if err != nil {
			fmt.Fprintln(out)
			return false, true
		}
	}
	return false, true
}
