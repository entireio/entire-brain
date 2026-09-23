package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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

// Ollama truncates a prompt that exceeds the context window the model was
// loaded with, and reports the truncation only as a prompt_eval_count smaller
// than the prompt warrants. Distilling part of a chunk while reporting success
// puts partial extraction into a store that is read as authoritative.
func TestOllamaDistillAgentRejectsSilentlyTruncatedPrompt(t *testing.T) {
	prompt := strings.Repeat("real transcript text that tokenises normally. ", 4000)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The server read far fewer tokens than a prompt this size contains.
		fmt.Fprintf(w, `{"response":"kind\tpath\tfact\n","prompt_eval_count":32768,"eval_count":8}`)
	}))
	defer server.Close()
	t.Setenv("ENTIRE_BRAIN_OLLAMA_URL", server.URL+"/api/generate")

	_, err := execOllamaDistillAgent(context.Background(), t.TempDir(),
		[]string{"ollama", "gemma4:12b", "system prompt"}, []byte(prompt), 30*time.Second)
	if err == nil {
		t.Fatalf("a %d-byte prompt read as only 32768 tokens is truncated and must not report success", len(prompt))
	}
	if !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("error must name truncation, got: %v", err)
	}
}

// A prompt the server read in full must pass, including dense input that
// tokenises far below the nominal ratio — the floor exists to avoid exactly
// this false positive.
func TestOllamaDistillAgentAcceptsFullyReadPrompt(t *testing.T) {
	prompt := strings.Repeat("dense", 2000) // 10000 bytes
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 2500 tokens for 10000 bytes is 4 chars/token — normal, not truncated.
		fmt.Fprint(w, `{"response":"kind\tpath\tfact\n","prompt_eval_count":2500,"eval_count":8}`)
	}))
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
