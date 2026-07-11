package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

const (
	distillUsageSourceCodex  = "codex_exec_jsonl"
	distillUsageSourceClaude = "claude_cli_json"
	distillUsageSourceOllama = "ollama_generate_json"
)

// distillTokenUsageSummary is persisted with the fact-source manifest. Calls
// includes successful and failed provider attempts, matching TotalAgentCalls.
// Input and output totals are provider-reported; cached/reasoning fields remain
// separate because providers disagree on whether they are subsets.
type distillTokenUsageSummary struct {
	Source                   string `json:"source,omitempty"`
	Calls                    int    `json:"calls"`
	CallsReported            int    `json:"calls_reported"` // both input and output present
	CallsPartial             int    `json:"calls_partial"`  // at least one, but not both
	CallsMissing             int    `json:"calls_missing"`  // no provider token fields
	Complete                 bool   `json:"complete"`
	InputTokens              int64  `json:"input_tokens"`
	OutputTokens             int64  `json:"output_tokens"`
	CacheCreationInputTokens int64  `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int64  `json:"cache_read_input_tokens,omitempty"`
	CachedInputTokens        int64  `json:"cached_input_tokens,omitempty"`
	ReasoningOutputTokens    int64  `json:"reasoning_output_tokens,omitempty"`
	// Total excludes CachedInputTokens and ReasoningOutputTokens because they
	// are subsets of input/output. Claude's separately billed cache creation/read
	// categories are included alongside uncached input and output.
	TotalTokens int64 `json:"total_tokens"`
}

type distillProviderUsage struct {
	Source                   string
	Reported                 bool
	InputReported            bool
	OutputReported           bool
	InputTokens              int64
	OutputTokens             int64
	CacheCreationInputTokens int64
	CacheReadInputTokens     int64
	CachedInputTokens        int64
	ReasoningOutputTokens    int64
}

type distillUsageCollector struct {
	mu            sync.Mutex
	usage         distillProviderUsage
	callsAny      int
	callsComplete int
	sources       map[string]int
}

type distillUsageContextKey struct{}

func withDistillUsageCollector(ctx context.Context) (context.Context, *distillUsageCollector) {
	collector := &distillUsageCollector{sources: map[string]int{}}
	return context.WithValue(ctx, distillUsageContextKey{}, collector), collector
}

func recordDistillProviderUsage(ctx context.Context, usage distillProviderUsage) {
	if !usage.Reported {
		return
	}
	collector, _ := ctx.Value(distillUsageContextKey{}).(*distillUsageCollector)
	if collector == nil {
		return
	}
	collector.mu.Lock()
	defer collector.mu.Unlock()
	collector.callsAny++
	if usage.InputReported && usage.OutputReported {
		collector.callsComplete++
	}
	collector.sources[usage.Source]++
	collector.usage.InputTokens += usage.InputTokens
	collector.usage.OutputTokens += usage.OutputTokens
	collector.usage.CacheCreationInputTokens += usage.CacheCreationInputTokens
	collector.usage.CacheReadInputTokens += usage.CacheReadInputTokens
	collector.usage.CachedInputTokens += usage.CachedInputTokens
	collector.usage.ReasoningOutputTokens += usage.ReasoningOutputTokens
}

func (collector *distillUsageCollector) summary(totalCalls int) *distillTokenUsageSummary {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	sources := make([]string, 0, len(collector.sources))
	for source := range collector.sources {
		sources = append(sources, source)
	}
	sort.Strings(sources)
	source := strings.Join(sources, "+")
	missing := totalCalls - collector.callsAny
	if missing < 0 {
		missing = 0
	}
	return &distillTokenUsageSummary{
		Source:                   source,
		Calls:                    totalCalls,
		CallsReported:            collector.callsComplete,
		CallsPartial:             collector.callsAny - collector.callsComplete,
		CallsMissing:             missing,
		Complete:                 collector.callsComplete == totalCalls && collector.callsAny == totalCalls,
		InputTokens:              collector.usage.InputTokens,
		OutputTokens:             collector.usage.OutputTokens,
		CacheCreationInputTokens: collector.usage.CacheCreationInputTokens,
		CacheReadInputTokens:     collector.usage.CacheReadInputTokens,
		CachedInputTokens:        collector.usage.CachedInputTokens,
		ReasoningOutputTokens:    collector.usage.ReasoningOutputTokens,
		TotalTokens: collector.usage.InputTokens + collector.usage.OutputTokens +
			collector.usage.CacheCreationInputTokens + collector.usage.CacheReadInputTokens,
	}
}

type distillUsageJSON struct {
	InputTokens              *int64 `json:"input_tokens"`
	OutputTokens             *int64 `json:"output_tokens"`
	CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     *int64 `json:"cache_read_input_tokens"`
	CachedInputTokens        *int64 `json:"cached_input_tokens"`
	ReasoningOutputTokens    *int64 `json:"reasoning_output_tokens"`
}

func (raw distillUsageJSON) validate() error {
	fields := []struct {
		name  string
		value *int64
	}{
		{"input_tokens", raw.InputTokens},
		{"output_tokens", raw.OutputTokens},
		{"cache_creation_input_tokens", raw.CacheCreationInputTokens},
		{"cache_read_input_tokens", raw.CacheReadInputTokens},
		{"cached_input_tokens", raw.CachedInputTokens},
		{"reasoning_output_tokens", raw.ReasoningOutputTokens},
	}
	for _, field := range fields {
		if field.value != nil && *field.value < 0 {
			return fmt.Errorf("%s must be non-negative", field.name)
		}
	}
	return nil
}

func (raw distillUsageJSON) addTo(usage *distillProviderUsage) {
	for _, value := range []*int64{
		raw.InputTokens,
		raw.OutputTokens,
		raw.CacheCreationInputTokens,
		raw.CacheReadInputTokens,
		raw.CachedInputTokens,
		raw.ReasoningOutputTokens,
	} {
		if value != nil {
			usage.Reported = true
			break
		}
	}
	if raw.InputTokens != nil {
		usage.InputReported = true
		usage.InputTokens += *raw.InputTokens
	}
	if raw.OutputTokens != nil {
		usage.OutputReported = true
		usage.OutputTokens += *raw.OutputTokens
	}
	if raw.CacheCreationInputTokens != nil {
		usage.CacheCreationInputTokens += *raw.CacheCreationInputTokens
	}
	if raw.CacheReadInputTokens != nil {
		usage.CacheReadInputTokens += *raw.CacheReadInputTokens
	}
	if raw.CachedInputTokens != nil {
		usage.CachedInputTokens += *raw.CachedInputTokens
	}
	if raw.ReasoningOutputTokens != nil {
		usage.ReasoningOutputTokens += *raw.ReasoningOutputTokens
	}
}

func parseCodexDistillOutput(raw string) (string, distillProviderUsage, error) {
	usage := distillProviderUsage{Source: distillUsageSourceCodex}
	var finalText string
	var agentErrors []string
	malformedEvent := false
	for lineNumber, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var event struct {
			Type  string           `json:"type"`
			Usage distillUsageJSON `json:"usage"`
			Item  struct {
				Type    string `json:"type"`
				Text    string `json:"text"`
				Message string `json:"message"`
			} `json:"item"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			malformedEvent = true
			continue
		}
		switch event.Type {
		case "item.completed":
			switch event.Item.Type {
			case "agent_message":
				finalText = event.Item.Text
			case "error":
				agentErrors = append(agentErrors, event.Item.Message)
			}
		case "turn.completed":
			if err := event.Usage.validate(); err != nil {
				usage.InputReported = false
				usage.OutputReported = false
				return "", usage, fmt.Errorf("codex usage on line %d: %w", lineNumber+1, err)
			}
			event.Usage.addTo(&usage)
		}
	}
	if malformedEvent {
		// Fact output can still be valid, but an unreadable event may have held
		// additional usage. Preserve observed counts without claiming completeness.
		usage.InputReported = false
		usage.OutputReported = false
	}
	if finalText == "" {
		if len(agentErrors) > 0 {
			return "", usage, fmt.Errorf("codex returned an error: %s", strings.Join(agentErrors, "; "))
		}
		return "", usage, fmt.Errorf("codex JSONL contained no final agent message")
	}
	return finalText, usage, nil
}

func parseClaudeDistillOutput(raw string) (string, distillProviderUsage, error) {
	usage := distillProviderUsage{Source: distillUsageSourceClaude}
	var result struct {
		Type    string           `json:"type"`
		Subtype string           `json:"subtype"`
		IsError bool             `json:"is_error"`
		Result  string           `json:"result"`
		Usage   distillUsageJSON `json:"usage"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &result); err != nil {
		return "", usage, fmt.Errorf("parse claude JSON: %w", err)
	}
	if err := result.Usage.validate(); err != nil {
		return "", usage, fmt.Errorf("claude usage: %w", err)
	}
	result.Usage.addTo(&usage)
	if result.IsError {
		message := strings.TrimSpace(result.Result)
		if message == "" {
			message = result.Subtype
		}
		return "", usage, fmt.Errorf("claude returned an error: %s", message)
	}
	if strings.TrimSpace(result.Result) == "" {
		return "", usage, fmt.Errorf("claude JSON contained no result")
	}
	return result.Result, usage, nil
}

func decodeStructuredDistillOutput(args []string, raw string) (string, distillProviderUsage, error) {
	if len(args) == 0 {
		return raw, distillProviderUsage{}, nil
	}
	executable := strings.TrimSuffix(strings.ToLower(filepath.Base(args[0])), ".exe")
	if executable == "codex" && containsDistillArg(args, "--json") {
		return parseCodexDistillOutput(raw)
	}
	if executable == "claude" && containsDistillArgPair(args, "--output-format", "json") {
		return parseClaudeDistillOutput(raw)
	}
	return raw, distillProviderUsage{}, nil
}

func structuredDistillOutput(args []string) bool {
	if len(args) == 0 {
		return false
	}
	executable := strings.TrimSuffix(strings.ToLower(filepath.Base(args[0])), ".exe")
	return (executable == "codex" && containsDistillArg(args, "--json")) ||
		(executable == "claude" && containsDistillArgPair(args, "--output-format", "json"))
}

func distillStdoutLimit(args []string) int {
	if structuredDistillOutput(args) {
		return distillMaxStructuredOutputBytes
	}
	return distillMaxOutputBytes
}

func containsDistillArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func containsDistillArgPair(args []string, key, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == key && args[i+1] == value {
			return true
		}
	}
	return false
}
