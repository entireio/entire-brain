package cli

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"

	"github.com/entireio/cli/transcript"
)

// transcriptRecord is one line of a JSONL transcript, decoded once.
//
// A line in the Entire Transcript Format (the compact transcript.jsonl the CLI
// writes for every agent, also called ETF v1) is decoded by the CLI's own
// transcript package into etf, so the format's rules live in one place: content
// at the record's top level, string or null content, user text blocks with no
// "type", tool results inlined into their tool_use block. Any other line is an
// agent's native dialect (Claude Code's "message" wrapper, Codex rollout
// records, pi's "message" records) and is kept as generic JSON in obj. At most
// one of the two is set.
type transcriptRecord struct {
	etf *transcript.Line
	obj map[string]any
}

// etfLinePrefix is how every line the CLI writes begins: the format's version
// field comes first.
var etfLinePrefix = []byte(`{"v":`)

// claudeMessageKey is the field Claude Code nests a native turn under.
var claudeMessageKey = []byte(`"message"`)

// decodeTranscriptRecord decodes one transcript line. ok is false when the line
// is not JSON that decodes into an object (JSON null decodes as an empty native
// record, as it always has).
//
// A line is in the Entire Transcript Format when it has an integer "v" and a
// non-empty string "type", the test transcript.Parse applies, and the
// transcript package can decode it. A line that also carries a non-empty
// Claude Code "message" wrapper is read as a native record: the wrapped turn
// wins over top-level content, as it did before the format had its own
// reader. A "user" or "assistant" record with no wrapped turn ("message" null,
// empty or absent) that is not a format line, because it has no "v" or the
// package rejects it, still has its top-level content read as the format's,
// as the readers always have (see decodeUnversionedTurn). Anything else is a
// native record.
func decodeTranscriptRecord(line string) (transcriptRecord, bool) {
	raw := bytes.TrimSpace([]byte(line))
	// Lines the CLI wrote start with "v" and carry no wrapper, so decode them
	// directly and skip the generic decode a native line needs. Any line that
	// mentions "message" takes the generic route, which checks for the
	// wrapper exactly.
	if bytes.HasPrefix(raw, etfLinePrefix) && !bytes.Contains(raw, claudeMessageKey) {
		if etf, ok := decodeETFLine(raw); ok {
			return transcriptRecord{etf: etf}, true
		}
	}
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil {
		return transcriptRecord{}, false
	}
	if len(jsonMap(obj["message"])) == 0 {
		if hasETFHead(obj) {
			if etf, ok := decodeETFLine(raw); ok {
				return transcriptRecord{etf: etf}, true
			}
		}
		if etf, ok := decodeUnversionedTurn(obj); ok {
			return transcriptRecord{etf: etf}, true
		}
	}
	return transcriptRecord{obj: obj}, true
}

// decodeUnversionedTurn reads a "user" or "assistant" record with no wrapped
// turn as a format line of that type, from its top-level content and agent
// alone: a record with no "v", or one the transcript package rejects (another
// "v", or a field other than content of the wrong JSON type). No agent writes
// these shapes, but the readers fell back to a record's top-level content
// whenever its "message" was null, empty or absent, whatever else the record
// held (history extraction revision 13 pins the null and empty cases), and
// they keep doing so. A non-string agent is dropped, as reading it as a string
// gave "" before.
func decodeUnversionedTurn(obj map[string]any) (*transcript.Line, bool) {
	recordType := jsonString(obj["type"])
	if recordType != transcript.TypeUser && recordType != transcript.TypeAssistant {
		return nil, false
	}
	line := map[string]any{"v": 1, "type": recordType, "content": obj["content"]}
	if agent, ok := obj["agent"].(string); ok {
		line["agent"] = agent
	}
	raw, err := json.Marshal(line)
	if err != nil {
		return nil, false
	}
	return decodeETFLine(raw)
}

func decodeETFLine(raw []byte) (*transcript.Line, bool) {
	lines, err := transcript.Decode(raw)
	if err != nil || len(lines) != 1 {
		return nil, false
	}
	return &lines[0], true
}

func hasETFHead(obj map[string]any) bool {
	v, ok := obj["v"].(float64)
	if !ok || v != math.Trunc(v) {
		return false
	}
	recordType, ok := obj["type"].(string)
	return ok && recordType != ""
}

// etfIs reports whether the record is a format line of the given type
// (transcript.TypeUser or transcript.TypeAssistant).
func (r transcriptRecord) etfIs(lineType string) bool {
	return r.etf != nil && r.etf.Type == lineType
}

// etfTexts returns the text of each text block of a format line, untrimmed and
// in order. String content arrives as one text block, and user text blocks,
// which have no "type" in the format, arrive typed as text.
func etfTexts(line *transcript.Line) []string {
	var out []string
	for _, block := range line.Blocks {
		if block.Type == transcript.BlockText && block.Text != "" {
			out = append(out, block.Text)
		}
	}
	return out
}

// toolUse is one tool call a transcript record makes, in the shape every
// dialect shares: the tool's name, lowercased and trimmed; the call ID; the
// input as generic JSON; and, for a format line, the inlined result's output
// and status.
type toolUse struct {
	name   string
	id     string
	input  any
	output string
	status string
}

// etfToolUses returns the tool calls of a format line's tool_use blocks.
func etfToolUses(line *transcript.Line) []toolUse {
	var out []toolUse
	for _, block := range line.Blocks {
		if block.Type != transcript.BlockToolUse {
			continue
		}
		use := toolUse{name: normalizeToolName(block.Name), id: block.ID, input: etfJSONValue(block.Input)}
		if block.Result != nil {
			use.output = block.Result.Output
			use.status = block.Result.Status
		}
		out = append(out, use)
	}
	return out
}

// nativeToolUses returns the tool calls in a native message's content blocks:
// Claude Code's and pi's "tool_use" blocks, and "tool" blocks that name the
// tool under "tool".
func nativeToolUses(content any) []toolUse {
	blocks, ok := content.([]any)
	if !ok {
		return nil
	}
	var out []toolUse
	for _, item := range blocks {
		block := jsonMap(item)
		if blockType := jsonString(block["type"]); blockType != "tool_use" && blockType != "tool" {
			continue
		}
		out = append(out, toolUse{
			name:  normalizeToolName(firstNonEmptyString(block["name"], block["tool"])),
			id:    jsonString(block["id"]),
			input: block["input"],
		})
	}
	return out
}

func normalizeToolName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// etfJSONValue decodes a format block's raw JSON (a tool input) into the
// generic value the native readers see, so both dialects share one input
// reader. Absent or invalid JSON gives nil.
func etfJSONValue(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return nil
	}
	return value
}
