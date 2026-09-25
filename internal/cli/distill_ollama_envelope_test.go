package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ollamaEnvelopeWithContext renders a /api/generate reply whose `response` is
// short but whose echoed `context` token array makes the JSON envelope roughly
// contextInts*7 bytes. Ollama returns this array on every non-streaming
// generate call and the distiller never reads it.
func ollamaEnvelopeWithContext(response string, contextInts int) string {
	var b strings.Builder
	b.WriteString(`{"response":`)
	fmt.Fprintf(&b, "%q", response)
	b.WriteString(`,"context":[`)
	for i := 0; i < contextInts; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%d", 100000+i%800000)
	}
	b.WriteString(`]}`)
	return b.String()
}

// A large prompt makes ollama echo a proportionally large `context` array, so
// the envelope outgrows distillMaxOutputBytes while the generated text stays
// tiny. Bounding the read by distillMaxOutputBytes rejected those replies on
// prompt size, which capped --max-chunk-bytes at roughly 96 KiB and reported
// the failure as oversized output the model never produced.
func TestOllamaDistillAgentAcceptsEnvelopeInflatedByEchoedContext(t *testing.T) {
	const want = "project.tooling.stack\tThe project uses Go.\n"
	body := ollamaEnvelopeWithContext(want, 42000)
	if len(body) <= distillMaxOutputBytes {
		t.Fatalf("fixture envelope %d bytes must exceed distillMaxOutputBytes %d to exercise the regression", len(body), distillMaxOutputBytes)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, body)
	}))
	defer server.Close()
	t.Setenv("ENTIRE_BRAIN_OLLAMA_URL", server.URL+"/api/generate")

	got, err := execOllamaDistillAgent(context.Background(), t.TempDir(),
		[]string{"ollama", "llama3.2", "system prompt"}, []byte("short prompt"), 30*time.Second)
	if err != nil {
		t.Fatalf("envelope of %d bytes carrying %d bytes of generated text must be accepted: %v", len(body), len(want), err)
	}
	if got != want {
		t.Fatalf("response text = %q, want %q", got, want)
	}
}

// The distiller must still refuse genuinely oversized generated text, and say
// so in those terms rather than blaming the envelope.
func TestOllamaDistillAgentRejectsOversizedGeneratedText(t *testing.T) {
	huge := strings.Repeat("x", distillMaxOutputBytes+1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s", ollamaEnvelopeWithContext(huge, 0))
	}))
	defer server.Close()
	t.Setenv("ENTIRE_BRAIN_OLLAMA_URL", server.URL+"/api/generate")

	_, err := execOllamaDistillAgent(context.Background(), t.TempDir(),
		[]string{"ollama", "llama3.2", "system prompt"}, []byte("short prompt"), 30*time.Second)
	if err == nil {
		t.Fatal("generated text over distillMaxOutputBytes must be rejected")
	}
	if !strings.Contains(err.Error(), "ollama output exceeds") {
		t.Fatalf("error must name the generated output, got: %v", err)
	}
}

// ollamaServerWithPS serves /api/generate with the given body and /api/ps
// advertising the model loaded at window tokens, which is what the truncation
// check reads.
func ollamaServerWithPS(t *testing.T, model string, window int64, generate string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/api/ps") {
			fmt.Fprintf(w, `{"models":[{"name":%q,"model":%q,"context_length":%d}]}`, model, model, window)
			return
		}
		fmt.Fprint(w, generate)
	}))
}

// A truncated prompt fills the loaded window exactly, so prompt_eval_count pins
// to it. Distilling part of a chunk while reporting success puts partial
// extraction into a store that is read as authoritative.
func TestOllamaDistillAgentRejectsSilentlyTruncatedPrompt(t *testing.T) {
	prompt := strings.Repeat("real transcript text that tokenises normally. ", 4000)
	server := ollamaServerWithPS(t, "gemma4:12b", 32768,
		`{"response":"kind\tpath\tfact\n","prompt_eval_count":32768,"eval_count":8}`)
	defer server.Close()
	t.Setenv("ENTIRE_BRAIN_OLLAMA_URL", server.URL+"/api/generate")

	_, err := execOllamaDistillAgent(context.Background(), t.TempDir(),
		[]string{"ollama", "gemma4:12b", "system prompt"}, []byte(prompt), 30*time.Second)
	if err == nil {
		t.Fatalf("a %d-byte prompt whose token count pins to the 32768 window is truncated and must not report success", len(prompt))
	}
	if !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("error must name truncation, got: %v", err)
	}
	// The request carries the chunk and the system prompt, but --max-chunk-bytes
	// only moves the chunk, so the message must separate them rather than report
	// one figure the caller cannot act on.
	sys := "system prompt"
	for _, want := range []string{
		fmt.Sprintf("%d-byte prompt", len(prompt)+len(sys)),
		fmt.Sprintf("%d-byte chunk", len(prompt)),
		fmt.Sprintf("%d-byte system prompt", len(sys)),
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error must report %q so the caller can size --max-chunk-bytes; got: %v", want, err)
		}
	}
}

// A prompt read in full sits below the window and must pass. This is the case
// a chars-per-token estimate got wrong: measured text ran 4.89 chars/token
// against a divisor of 5, so ordinary input sat 2% from a spurious failure.
// The window comparison has no tokenizer term, so a dense prompt is fine.
func TestOllamaDistillAgentAcceptsFullyReadPrompt(t *testing.T) {
	prompt := strings.Repeat("dense", 2000) // 10000 bytes
	server := ollamaServerWithPS(t, "gemma4:12b", 32768,
		`{"response":"kind\tpath\tfact\n","prompt_eval_count":2500,"eval_count":8}`)
	defer server.Close()
	t.Setenv("ENTIRE_BRAIN_OLLAMA_URL", server.URL+"/api/generate")

	got, err := execOllamaDistillAgent(context.Background(), t.TempDir(),
		[]string{"ollama", "gemma4:12b", "sys"}, []byte(prompt), 30*time.Second)
	if err != nil {
		t.Fatalf("fully-read prompt must be accepted: %v", err)
	}
	if !strings.Contains(got, "fact") {
		t.Fatalf("response text = %q", got)
	}
}

// When the window cannot be determined the check must stand down rather than
// guess, or an ollama build without the field would fail every chunk.
func TestOllamaDistillAgentSkipsTruncationCheckWithoutAWindow(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/api/ps") {
			fmt.Fprint(w, `{"models":[]}`) // model not resident: window unknown
			return
		}
		fmt.Fprint(w, `{"response":"kind\tpath\tfact\n","prompt_eval_count":32768,"eval_count":8}`)
	}))
	defer server.Close()
	t.Setenv("ENTIRE_BRAIN_OLLAMA_URL", server.URL+"/api/generate")

	if _, err := execOllamaDistillAgent(context.Background(), t.TempDir(),
		[]string{"ollama", "gemma4:12b", "sys"}, []byte("short"), 30*time.Second); err != nil {
		t.Fatalf("an undeterminable window must skip the check, not fail: %v", err)
	}
}

// The envelope allowance scales off a request size the caller chooses and
// --max-chunk-bytes has no upper bound, so the scaling needs an absolute
// ceiling or a hostile loopback endpoint could amplify a large chunk into an
// unbounded read.
func TestOllamaDistillAgentEnvelopeAllowanceIsCeilinged(t *testing.T) {
	// A prompt whose unclamped allowance (8x + slack) would exceed the ceiling.
	oversized := distillOllamaMaxEnvelopeBytes/distillOllamaEnvelopeFactor + (1 << 20)
	unclamped := int64(distillMaxOutputBytes) + int64(oversized)*distillOllamaEnvelopeFactor + distillOllamaEnvelopeSlack
	if unclamped <= int64(distillOllamaMaxEnvelopeBytes) {
		t.Fatalf("fixture must exceed the ceiling: unclamped %d <= ceiling %d", unclamped, distillOllamaMaxEnvelopeBytes)
	}

	// Written by the handler goroutine and read by the test goroutine, so it
	// must be synchronised or -race fails the test on the accounting rather
	// than on the behaviour under test.
	var served atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Stream past the ceiling; a correct client stops reading at it.
		chunk := strings.Repeat("a", 1<<20)
		w.Header().Set("Content-Type", "application/json")
		for served.Load() < int64(distillOllamaMaxEnvelopeBytes)+(8<<20) {
			n, err := io.WriteString(w, chunk)
			served.Add(int64(n))
			if err != nil {
				return
			}
		}
	}))
	defer server.Close()
	t.Setenv("ENTIRE_BRAIN_OLLAMA_URL", server.URL+"/api/generate")

	_, err := execOllamaDistillAgent(context.Background(), t.TempDir(),
		[]string{"ollama", "gemma4:12b", "sys"}, make([]byte, oversized), 60*time.Second)
	if err == nil {
		t.Fatal("an endpoint streaming past the ceiling must be refused")
	}
	if !strings.Contains(err.Error(), "envelope exceeds") {
		t.Fatalf("error must name the envelope bound, got: %v", err)
	}
	if got := served.Load(); got > int64(distillOllamaMaxEnvelopeBytes)+(4<<20) {
		t.Fatalf("read %d bytes, must stop near the %d ceiling rather than following the stream",
			got, distillOllamaMaxEnvelopeBytes)
	}
}

// The generate call runs under the whole-call timeout, and a chunk near the
// context window — the case the truncation guard exists to catch — spends most
// of that budget generating. If the /api/ps probe reuses the same context it
// gets no time, reports "cannot tell", and the guard stands down exactly when
// it is needed. The probe must carry its own budget.
func TestOllamaDistillAgentProbesTheWindowOnAFreshBudget(t *testing.T) {
	const callTimeout = 2 * time.Second
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/api/ps") {
			// Longer than whatever the generate call left on the shared context,
			// well inside the probe's own budget.
			time.Sleep(1200 * time.Millisecond)
			fmt.Fprint(w, `{"models":[{"name":"gemma4:12b","model":"gemma4:12b","context_length":32768}]}`)
			return
		}
		time.Sleep(callTimeout - 300*time.Millisecond) // burn nearly the whole call budget
		fmt.Fprint(w, `{"response":"kind\tpath\tfact\n","prompt_eval_count":32768,"eval_count":8}`)
	}))
	defer server.Close()
	t.Setenv("ENTIRE_BRAIN_OLLAMA_URL", server.URL+"/api/generate")

	_, err := execOllamaDistillAgent(context.Background(), t.TempDir(),
		[]string{"ollama", "gemma4:12b", "sys"}, []byte("a large chunk"), callTimeout)
	if err == nil {
		t.Fatal("truncation must still be detected when the generate call consumed the call budget")
	}
	if !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("the window probe must run on its own budget; got: %v", err)
	}
}
