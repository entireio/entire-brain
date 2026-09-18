package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestDocumentConversationExpansionHonorsIndexedRangeAndDigest(t *testing.T) {
	message := func(role, text string) map[string]any {
		return map[string]any{"info": map[string]string{"role": role}, "parts": []map[string]string{{"type": "text", "text": text}, {"type": "reasoning", "text": "HIDDEN_REASONING"}}}
	}
	messages := []map[string]any{
		message("user", "OUTSIDE_BEFORE"),
		message("user", "   "),
		message("user", "Explain cache invalidation"),
		message("assistant", "Decision: invalidate entries before publishing."),
		message("user", "SECOND_REQUEST_MUST_NOT_REPLACE_FIRST"),
		message("assistant", "Keep the prior generation available on failure."),
		message("assistant", "OUTSIDE_AFTER"),
	}
	data, err := json.MarshalIndent(map[string]any{"messages": messages}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	brainDir := t.TempDir()
	rel := "sessions/main/document.json"
	path := filepath.Join(brainDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	parsed, ok := parseDocumentConversation(string(data))
	if !ok || len(parsed) != len(messages) {
		t.Fatalf("document parse = %v/%d", ok, len(parsed))
	}
	record := historyRecord{Path: rel, Line: parsed[1].Line, EndLine: parsed[5].Line, SourceDigest: conversationDigest(data)}
	got, err := expandConversationExchange(brainDir, record)
	if err != nil {
		t.Fatal(err)
	}
	if got.Request != "Explain cache invalidation" || got.Response != "Decision: invalidate entries before publishing.\nKeep the prior generation available on failure." || got.Truncated {
		t.Fatalf("expansion = %+v", got)
	}
	record.Line = parsed[2].Line
	record.EndLine = 0
	single, err := expandConversationExchange(brainDir, record)
	if err != nil || single.Request != got.Request || single.Response != "" {
		t.Fatalf("single-message fallback=%+v err=%v", single, err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if stale, err := expandConversationExchange(brainDir, record); !errors.Is(err, errConversationSourceStale) || stale.Request != "" || stale.Response != "" {
		t.Fatalf("stale content escaped: %+v %v", stale, err)
	}
}

func TestDocumentConversationExpansionBoundsUTF8RequestAndResponse(t *testing.T) {
	messages := []documentMessage{
		{Role: "user", Line: 1, Text: strings.Repeat("réquest ", conversationRequestMaxBytes)},
		{Role: "assistant", Line: 2, Text: strings.Repeat("réponse ", conversationExpansionMaxBytes)},
	}
	got := expandDocumentConversationRange(messages, historyRecord{Line: 1, EndLine: 2})
	if !got.Truncated || got.Request == "" || got.Response == "" || len(got.Request) > conversationRequestMaxBytes || len(got.Request)+len(got.Response) > conversationExpansionMaxBytes || !utf8.ValidString(got.Request) || !utf8.ValidString(got.Response) {
		t.Fatalf("invalid bounds: request=%d response=%d truncated=%v", len(got.Request), len(got.Response), got.Truncated)
	}
}
