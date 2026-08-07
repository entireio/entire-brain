package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseCodexDistillOutput(t *testing.T) {
	raw := strings.Join([]string{
		`{"type":"thread.started","thread_id":"t1"}`,
		`{"type":"turn.completed","usage":{"input_tokens":100,"cached_input_tokens":40,"output_tokens":12,"reasoning_output_tokens":5}}`,
		`{"type":"item.completed","item":{"type":"agent_message","text":"first"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":25,"cached_input_tokens":10,"output_tokens":3,"reasoning_output_tokens":1}}`,
		`{"type":"item.completed","item":{"type":"agent_message","text":"preferences.coding.style\tConcise commits.\n"}}`,
	}, "\n")
	text, usage, err := parseCodexDistillOutput(raw)
	if err != nil {
		t.Fatal(err)
	}
	if text != "preferences.coding.style\tConcise commits.\n" {
		t.Fatalf("final text = %q", text)
	}
	if !usage.Reported || !usage.InputReported || !usage.OutputReported || usage.Source != distillUsageSourceCodex || usage.InputTokens != 125 || usage.OutputTokens != 15 || usage.CachedInputTokens != 50 || usage.ReasoningOutputTokens != 6 {
		t.Fatalf("usage = %+v", usage)
	}
}

func TestParseClaudeDistillOutput(t *testing.T) {
	raw := `{"type":"result","subtype":"success","is_error":false,"result":"workflow.testing.rules\tRun race tests.\n","usage":{"input_tokens":20,"cache_creation_input_tokens":30,"cache_read_input_tokens":40,"output_tokens":5}}`
	text, usage, err := parseClaudeDistillOutput(raw)
	if err != nil {
		t.Fatal(err)
	}
	if text != "workflow.testing.rules\tRun race tests.\n" {
		t.Fatalf("result = %q", text)
	}
	if !usage.Reported || !usage.InputReported || !usage.OutputReported || usage.Source != distillUsageSourceClaude || usage.InputTokens != 20 || usage.OutputTokens != 5 || usage.CacheCreationInputTokens != 30 || usage.CacheReadInputTokens != 40 {
		t.Fatalf("usage = %+v", usage)
	}

	_, failedUsage, err := parseClaudeDistillOutput(`{"type":"result","subtype":"success","is_error":true,"result":"Not logged in","usage":{"input_tokens":0,"output_tokens":0}}`)
	if err == nil || !strings.Contains(err.Error(), "Not logged in") {
		t.Fatalf("logical CLI error was not surfaced: %v", err)
	}
	if !failedUsage.Reported {
		t.Fatal("explicit zero-token provider usage should still count as reported")
	}
	if _, _, err := parseClaudeDistillOutput(`{"type":"result","is_error":false,"result":"ok","usage":{"input_tokens":-1,"output_tokens":2}}`); err == nil {
		t.Fatal("negative provider counts must be rejected")
	}
}

func TestStructuredDistillOutputAcceptsEmptyFactResult(t *testing.T) {
	codexRaw := strings.Join([]string{
		`{"type":"item.completed","item":{"type":"agent_message","text":""}}`,
		`{"type":"turn.completed","usage":{"input_tokens":8,"output_tokens":0}}`,
	}, "\n")
	text, usage, err := parseCodexDistillOutput(codexRaw)
	if err != nil || text != "" || !usage.Reported || usage.InputTokens != 8 || usage.OutputTokens != 0 {
		t.Fatalf("codex text=%q usage=%+v err=%v", text, usage, err)
	}

	claudeRaw := `{"type":"result","subtype":"success","is_error":false,"result":"","usage":{"input_tokens":8,"output_tokens":0}}`
	text, usage, err = parseClaudeDistillOutput(claudeRaw)
	if err != nil || text != "" || !usage.Reported || usage.InputTokens != 8 || usage.OutputTokens != 0 {
		t.Fatalf("claude text=%q usage=%+v err=%v", text, usage, err)
	}
}

func TestStructuredDistillOutputRejectsMissingFactResult(t *testing.T) {
	if _, _, err := parseCodexDistillOutput(`{"type":"turn.completed","usage":{"input_tokens":8,"output_tokens":0}}`); err == nil {
		t.Fatal("codex envelope without an agent message must fail")
	}
	if _, _, err := parseClaudeDistillOutput(`{"type":"result","subtype":"success","is_error":false,"usage":{"input_tokens":8,"output_tokens":0}}`); err == nil {
		t.Fatal("claude envelope without a result field must fail")
	}
}

func TestDistillUsageCollectorConcurrentSummary(t *testing.T) {
	ctx, collector := withDistillUsageCollector(context.Background())
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			recordDistillProviderUsage(ctx, distillProviderUsage{
				Source:         distillUsageSourceCodex,
				Reported:       true,
				InputReported:  true,
				OutputReported: true,
				InputTokens:    10,
				OutputTokens:   2,
			})
		}()
	}
	wg.Wait()
	summary := collector.summary(21)
	if summary.CallsReported != 20 || summary.CallsMissing != 1 || summary.Complete || summary.InputTokens != 200 || summary.OutputTokens != 40 || summary.TotalTokens != 240 {
		t.Fatalf("summary = %+v", summary)
	}
}

func TestDistillUsageCollectorMarksPartialCallIncomplete(t *testing.T) {
	ctx, collector := withDistillUsageCollector(context.Background())
	recordDistillProviderUsage(ctx, distillProviderUsage{
		Source:         distillUsageSourceOllama,
		Reported:       true,
		OutputReported: true,
		OutputTokens:   7,
	})
	summary := collector.summary(1)
	if summary.Complete || summary.CallsReported != 0 || summary.CallsPartial != 1 || summary.CallsMissing != 0 || summary.OutputTokens != 7 {
		t.Fatalf("summary = %+v", summary)
	}
}

func TestCodexMalformedEventPreservesOutputButDowngradesUsage(t *testing.T) {
	raw := strings.Join([]string{
		`{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":2}}`,
		`{not-json}`,
		`{"type":"item.completed","item":{"type":"agent_message","text":"ok"}}`,
	}, "\n")
	text, usage, err := parseCodexDistillOutput(raw)
	if err != nil || text != "ok" {
		t.Fatalf("text=%q usage=%+v err=%v", text, usage, err)
	}
	if !usage.Reported || usage.InputReported || usage.OutputReported || usage.InputTokens != 10 || usage.OutputTokens != 2 {
		t.Fatalf("malformed event must retain observed counts without claiming completeness: %+v", usage)
	}
}

func TestDistillStdoutLimitSeparatesEnvelopeFromFactResult(t *testing.T) {
	if got := distillStdoutLimit([]string{"codex", "exec", "--json"}); got != distillMaxStructuredOutputBytes {
		t.Fatalf("structured limit = %d", got)
	}
	if got := distillStdoutLimit([]string{"custom-agent"}); got != distillMaxOutputBytes {
		t.Fatalf("plain limit = %d", got)
	}
}

func TestDecodeStructuredDistillOutputLeavesCustomCommandUntouched(t *testing.T) {
	want := "preferences.coding.style\tConcise commits.\n"
	got, usage, err := decodeStructuredDistillOutput([]string{"custom-agent"}, want)
	if err != nil || got != want || usage.Reported {
		t.Fatalf("got=%q usage=%+v err=%v", got, usage, err)
	}
}

func TestDecodeStructuredDistillOutputRecognizesAbsoluteBinary(t *testing.T) {
	raw := `{"type":"result","subtype":"success","is_error":false,"result":"ok","usage":{"input_tokens":1,"output_tokens":2}}`
	got, usage, err := decodeStructuredDistillOutput([]string{"/opt/tools/claude", "--output-format", "json"}, raw)
	if err != nil || got != "ok" || !usage.InputReported || !usage.OutputReported {
		t.Fatalf("got=%q usage=%+v err=%v", got, usage, err)
	}
}

func TestOllamaDistillUsageIsCollected(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"response":"project.tooling.stack\tUses Go.\n","prompt_eval_count":123,"eval_count":17}`))
	}))
	defer server.Close()
	t.Setenv("ENTIRE_BRAIN_OLLAMA_URL", server.URL)
	ctx, collector := withDistillUsageCollector(context.Background())
	out, err := execOllamaDistillAgent(ctx, t.TempDir(), []string{"ollama", "test-model", "system prompt"}, []byte("transcript"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if out != "project.tooling.stack\tUses Go.\n" {
		t.Fatalf("response = %q", out)
	}
	summary := collector.summary(1)
	if !summary.Complete || summary.Source != distillUsageSourceOllama || summary.InputTokens != 123 || summary.OutputTokens != 17 || summary.TotalTokens != 140 {
		t.Fatalf("summary = %+v", summary)
	}
}
