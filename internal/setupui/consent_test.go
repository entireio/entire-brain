package setupui

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/entireio/entire-brain/internal/tui"
)

// failReader fails the test if anything reads it. It is how "the no-terminal
// path does not block" is proved rather than asserted: a function that cannot
// read stdin cannot wait on it, and that is a stronger claim than observing
// that it happened to return.
type failReader struct{ t *testing.T }

func (r failReader) Read([]byte) (int, error) {
	r.t.Helper()
	r.t.Fatal("stdin was read on a path that must never read it; in CI this is a wedged pipeline")
	return 0, io.EOF
}

func spendRequest() ConsentRequest {
	return ConsentRequest{Sessions: 12, Agent: "claude-code", Model: "claude-haiku-5-5", Effort: "low", TTY: true}
}

func ask(t *testing.T, req ConsentRequest, stdin io.Reader) (ConsentOutcome, string) {
	t.Helper()
	var buf bytes.Buffer
	rend := rendererFor(tui.PlainCaps(), 100)
	return Consent(req, stdin, &buf, rend), buf.String()
}

// TestConsentDefaultsToNo covers every way a human can decline, including the
// ways that are not an answer at all.
func TestConsentDefaultsToNo(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		stdin string
	}{
		{"bare enter", "\n"},
		{"explicit n", "n\n"},
		{"explicit no", "NO\n"},
		{"eof", ""},
		{"garbage then eof", "maybe\n"},
		{"garbage twice", "maybe\nperhaps\n"},
		{"whitespace", "   \n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, out := ask(t, spendRequest(), strings.NewReader(tc.stdin))
			if got.Spend {
				t.Fatalf("stdin %q spent money", tc.stdin)
			}
			if got.Reason == "" {
				t.Error("a skip with no reason is a dead end")
			}
			if !strings.Contains(got.Reason, "entire brain distill") {
				t.Errorf("skip reason %q does not name the command that starts it later", got.Reason)
			}
			if !strings.Contains(out, "Distill now? [y/N]") {
				t.Errorf("the prompt was never shown:\n%s", out)
			}
		})
	}
}

// TestConsentAcceptsYes is the other branch: without it every test above
// passes on a function that returns false unconditionally.
func TestConsentAcceptsYes(t *testing.T) {
	t.Parallel()
	for _, answer := range []string{"y\n", "Y\n", "yes\n", "  YES  \n"} {
		got, out := ask(t, spendRequest(), strings.NewReader(answer))
		if !got.Spend {
			t.Errorf("answer %q was not taken as consent", answer)
		}
		if !got.Prompted {
			t.Errorf("answer %q reports Prompted=false", answer)
		}
		if !strings.Contains(out, "12 sessions can be distilled") {
			t.Errorf("the block never said what was being bought:\n%s", out)
		}
		if !strings.Contains(out, "claude-haiku-5-5") {
			t.Errorf("the block never named the model:\n%s", out)
		}
	}
}

// TestConsentRepromptsOnceThenDefaults pins the budget: a loop here is just a
// slower way to wedge a pipeline.
func TestConsentRepromptsOnceThenDefaults(t *testing.T) {
	t.Parallel()
	got, out := ask(t, spendRequest(), strings.NewReader("maybe\ny\n"))
	if !got.Spend {
		t.Error("a corrected answer after one re-prompt was not accepted")
	}
	if strings.Count(out, "Distill now? [y/N]") != 2 {
		t.Errorf("want exactly two prompts, got %d:\n%s", strings.Count(out, "Distill now? [y/N]"), out)
	}
	if !strings.Contains(out, "Please answer y or n.") {
		t.Errorf("the re-prompt gave no correction:\n%s", out)
	}

	got, out = ask(t, spendRequest(), strings.NewReader("maybe\nperhaps\nyes\n"))
	if got.Spend {
		t.Error("a third attempt was read; two is the budget")
	}
	if strings.Count(out, "Distill now? [y/N]") != 2 {
		t.Errorf("want exactly two prompts, got %d", strings.Count(out, "Distill now? [y/N]"))
	}
}

// TestConsentNeverReadsStdinWithoutATerminal is the CI guard. The reader
// fails the test on contact, so a regression shows up as a failure rather than
// as a job that hangs until the runner's timeout.
func TestConsentNeverReadsStdinWithoutATerminal(t *testing.T) {
	t.Parallel()
	req := spendRequest()
	req.TTY = false
	got, out := ask(t, req, failReader{t})
	if got.Spend {
		t.Fatal("a process with no terminal spent money")
	}
	if got.Prompted {
		t.Fatal("a process with no terminal was prompted")
	}
	if strings.Contains(out, "Distill now?") {
		t.Errorf("a prompt was printed with nobody to answer it:\n%s", out)
	}
	if got.Reason == "" {
		t.Fatal("the non-interactive skip said nothing")
	}
	for _, want := range []string{"no terminal", "--yes"} {
		if !strings.Contains(got.Reason, want) {
			t.Errorf("reason %q does not mention %q", got.Reason, want)
		}
	}
}

// TestConsentOverridesSuppressThePrompt covers both flags, both branches, and
// proves neither touches stdin.
func TestConsentOverridesSuppressThePrompt(t *testing.T) {
	t.Parallel()
	t.Run("--yes spends", func(t *testing.T) {
		t.Parallel()
		req := spendRequest()
		req.Override = OverrideYes
		got, out := ask(t, req, failReader{t})
		if !got.Spend {
			t.Fatal("--yes did not spend")
		}
		if got.Prompted {
			t.Error("--yes still prompted")
		}
		if !strings.Contains(got.Reason, "--yes") {
			t.Errorf("reason %q does not record the override", got.Reason)
		}
		if out != "" {
			t.Errorf("--yes printed a prompt block:\n%s", out)
		}
	})
	t.Run("--no-backfill skips", func(t *testing.T) {
		t.Parallel()
		req := spendRequest()
		req.Override = OverrideNo
		got, out := ask(t, req, failReader{t})
		if got.Spend {
			t.Fatal("--no-backfill spent")
		}
		if got.Prompted {
			t.Error("--no-backfill still prompted")
		}
		if !strings.Contains(got.Reason, "--no-backfill") {
			t.Errorf("reason %q does not record the override", got.Reason)
		}
		if out != "" {
			t.Errorf("--no-backfill printed a prompt block:\n%s", out)
		}
	})
	t.Run("--yes on a terminal still skips when there is nothing to buy", func(t *testing.T) {
		t.Parallel()
		req := spendRequest()
		req.Override = OverrideYes
		req.Sessions = 0
		got, _ := ask(t, req, failReader{t})
		if got.Spend {
			t.Fatal("--yes claimed to distill zero sessions")
		}
	})
}

// TestConsentDoesNotAskWhenThereIsNothingToBuy covers the two impossibilities.
func TestConsentDoesNotAskWhenThereIsNothingToBuy(t *testing.T) {
	t.Parallel()
	t.Run("no sessions", func(t *testing.T) {
		t.Parallel()
		req := spendRequest()
		req.Sessions = 0
		got, out := ask(t, req, failReader{t})
		if got.Spend || got.Prompted {
			t.Fatalf("prompted or spent with no sessions: %+v", got)
		}
		if !strings.Contains(got.Reason, "no sessions") {
			t.Errorf("reason %q does not say why", got.Reason)
		}
		if out != "" {
			t.Errorf("printed a block anyway:\n%s", out)
		}
	})
	t.Run("no agent", func(t *testing.T) {
		t.Parallel()
		req := spendRequest()
		req.Agent = ""
		got, out := ask(t, req, failReader{t})
		if got.Spend || got.Prompted {
			t.Fatalf("prompted or spent with no agent: %+v", got)
		}
		if !strings.Contains(got.Reason, RemedyAgent) {
			t.Errorf("reason %q names no remedy", got.Reason)
		}
		if out != "" {
			t.Errorf("printed a block anyway:\n%s", out)
		}
	})
}

// TestConsentSurvivesAClosedStdin is the interrupt case: no panic, no hang,
// a clean No.
func TestConsentSurvivesAClosedStdin(t *testing.T) {
	t.Parallel()
	reader, writer := io.Pipe()
	if err := writer.CloseWithError(io.ErrClosedPipe); err != nil {
		t.Fatal(err)
	}
	got, _ := ask(t, spendRequest(), reader)
	if got.Spend {
		t.Fatal("a closed stdin was read as consent")
	}

	got, _ = ask(t, spendRequest(), nil)
	if got.Spend {
		t.Fatal("a nil stdin was read as consent")
	}
}

// TestConsentPromptRendersInEveryMode keeps the question readable wherever it
// is asked, and within the width budget.
func TestConsentPromptRendersInEveryMode(t *testing.T) {
	t.Parallel()
	for _, width := range []int{40, 80} {
		for _, m := range renderModes(width) {
			var buf bytes.Buffer
			rend := rendererFor(m.caps, width)
			got := Consent(spendRequest(), strings.NewReader("\n"), &buf, rend)
			if got.Spend {
				t.Fatalf("%s/%d: bare enter spent", m.name, width)
			}
			out := buf.String()
			if !strings.Contains(StripANSI(out), "Distill now? [y/N]") {
				t.Errorf("%s/%d: question missing:\n%s", m.name, width, out)
			}
			for _, line := range outputLines(out) {
				if cells := len([]rune(StripANSI(line))); cells > width {
					t.Errorf("%s/%d: prompt row of %d cells overflows: %q", m.name, width, cells, StripANSI(line))
				}
			}
			if !m.caps.Unicode {
				for _, r := range StripANSI(out) {
					if r > 127 {
						t.Errorf("%s/%d: non-ASCII %q in the prompt", m.name, width, r)
					}
				}
			}
		}
	}
}
