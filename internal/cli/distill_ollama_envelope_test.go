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
