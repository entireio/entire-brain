package cli

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
)

// The Entire CLI's compact transcript (Entire Transcript Format) carries the
// content blocks at the RECORD's top level, with no "message" wrapper:
//
//	{"v":1,"agent":"claude-code","type":"user","content":[{"id":"u1","text":"..."}]}
//	{"v":1,"agent":"claude-code","type":"assistant","content":[{"type":"text",...},{"type":"tool_use",...}]}
//
// It is the DEFAULT export flavor, so the conversation and history dialect
// switches must parse it (customer report: compact exports produced zero
// searchable exchanges; COR-2289).
const (
	etfUserLine      = `{"v":1,"agent":"claude-code","cli_version":"0.12.2","type":"user","ts":"2026-10-26T02:30:20.496Z","content":[{"id":"u1","text":"please add a healthcheck endpoint"}]}`
	etfToolLine      = `{"v":1,"agent":"claude-code","cli_version":"0.12.2","type":"assistant","ts":"2026-10-26T02:30:22.000Z","content":[{"type":"text","text":"Adding the endpoint now."},{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"go test ./..."},"result":{"output":"ok"}}]}`
	etfAssistantLine = `{"v":1,"agent":"claude-code","cli_version":"0.12.2","type":"assistant","ts":"2026-10-26T02:30:25.000Z","content":[{"type":"text","text":"Added /healthz returning 200."}]}`
)

func etfTranscript() string {
	return etfUserLine + "\n" + etfToolLine + "\n" + etfAssistantLine + "\n"
}

func TestScanConversationTranscriptParsesCompactETF(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(path, []byte(etfTranscript()), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := scanConversationTranscript(path)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(result.Exchanges) != 1 {
		t.Fatalf("exchanges = %d, want 1 (%+v)", len(result.Exchanges), result)
	}
	exchange := result.Exchanges[0]
	if !strings.Contains(exchange.Request, "healthcheck endpoint") {
		t.Errorf("request = %q, want the ETF user text", exchange.Request)
	}
	if !strings.Contains(exchange.Response, "Added /healthz") {
		t.Errorf("response = %q, want the ETF assistant narrative", exchange.Response)
	}
	if len(exchange.ToolNames) != 1 || exchange.ToolNames[0] != "Bash" {
		t.Errorf("tool names = %v, want [Bash]", exchange.ToolNames)
	}
}

func TestExtractHistoryJSONFragmentsCompactETF(t *testing.T) {
	var user, assistant map[string]any
	if err := json.Unmarshal([]byte(etfUserLine), &user); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(etfToolLine), &assistant); err != nil {
		t.Fatal(err)
	}

	userFragments := extractHistoryJSONFragments(user)
	if len(userFragments) != 1 || userFragments[0].Source != "user_prompt" {
		t.Fatalf("user fragments = %+v, want one user_prompt", userFragments)
	}

	sources := map[string]bool{}
	for _, fragment := range extractHistoryJSONFragments(assistant) {
		sources[fragment.Source] = true
	}
	if !sources["assistant_message"] {
		t.Errorf("assistant fragments missing assistant_message: %v", sources)
	}
	if !sources[historyToolCallSource("Bash")] {
		t.Errorf("assistant fragments missing tool call: %v", sources)
	}
}

// TestHistoryScanCachePreETFEntriesIgnored covers the upgrade path for an
// existing brain: a cache written before compact (ETF) parsing holds the empty
// results of a scanner that saw nothing in a compact transcript, and the
// content digest is the sole reuse authority, so unchanged transcripts would
// keep those empty results forever. Any version at or after the ETF bump must
// reject them so the next refresh rescans (COR-2289).
func TestHistoryScanCachePreETFEntriesIgnored(t *testing.T) {
	if historyScanCacheVersion < 11 {
		t.Fatalf("test guards the pre-ETF -> ETF cache migration; any later version must also reject v10 entries (version = %d)", historyScanCacheVersion)
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, brainstore.HistoryDirName), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	stale := historyScanCache{Version: 10, Files: map[string]historyScanCacheEntry{
		"sessions/main/compact.jsonl": {ContentDigest: "sha256:unchanged", Records: nil},
	}}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if err := json.NewEncoder(gz).Encode(stale); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	path := filepath.Join(dir, filepath.FromSlash(historyScanCachePath))
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write cache: %v", err)
	}

	cache := loadHistoryScanCache(dir)
	if len(cache.Files) != 0 {
		t.Fatalf("pre-ETF cache entries were reused: %+v", cache.Files)
	}
}
