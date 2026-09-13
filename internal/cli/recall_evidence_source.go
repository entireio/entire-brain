package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const evidenceInputBytes = 128 * 1024

// Offsets address UTF-8 bytes of the decoded JSON string at JSONPointer, or
// the original file when JSONPointer is empty. SourceSHA256 binds the entire
// canonical file; ContentSHA256 binds that string. Line addresses the JSONL
// record (1 for a JSON document). No model-generated text enters this type.
type evidenceSpan struct {
	ID            string `json:"id"`
	SessionID     string `json:"session_id"`
	Branch        string `json:"branch"`
	Path          string `json:"path"`
	Line          int    `json:"line"`
	JSONPointer   string `json:"json_pointer,omitempty"`
	Role          string `json:"role,omitempty"`
	Timestamp     string `json:"timestamp,omitempty"`
	SourceSHA256  string `json:"source_sha256"`
	ContentSHA256 string `json:"content_sha256"`
	StartByte     int    `json:"start_byte"`
	EndByte       int    `json:"end_byte"`
	Text          string `json:"text"`
}

type evidenceField struct {
	text, pointer, role, timestamp string
	line                           int
}
type evidenceCandidates struct {
	Spans           []evidenceSpan
	Warnings        []string
	State           string
	InputTruncated  bool
	SessionsScanned int
	SessionsMatched int
}

func evidenceHash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// Empty-line boundaries preserve code fences, table rows and original CRLF.
func evidenceBlocks(text string) [][2]int {
	var out [][2]int
	start, offset := 0, 0
	fence := ""
	for _, line := range strings.SplitAfter(text, "\n") {
		trimmed := strings.TrimLeft(line, " ")
		indent := len(line) - len(trimmed)
		if indent <= 3 && len(trimmed) >= 3 && (strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~")) {
			n := 0
			for n < len(trimmed) && trimmed[n] == trimmed[0] {
				n++
			}
			if fence == "" {
				fence = trimmed[:n]
			} else if fence[0] == trimmed[0] && n >= len(fence) && strings.TrimSpace(trimmed[n:]) == "" {
				fence = ""
			}
		}
		if strings.TrimSpace(line) == "" && fence == "" {
			if strings.TrimSpace(text[start:offset]) != "" {
				out = append(out, [2]int{start, offset})
			}
			start = offset + len(line)
		}
		offset += len(line)
	}
	if strings.TrimSpace(text[start:offset]) != "" {
		out = append(out, [2]int{start, offset})
	}
	return out
}

// Explicit schemas prevent hidden reasoning, tool output and arbitrary metadata
// from becoming conversation evidence. String values are never normalized.
func evidenceJSONFields(obj map[string]any, prefix string, line int) []evidenceField {
	var fields []evidenceField
	timestamp := jsonString(obj["timestamp"])
	add := func(value any, pointer, role string) {
		if s, ok := value.(string); ok && strings.TrimSpace(s) != "" {
			fields = append(fields, evidenceField{s, prefix + pointer, role, timestamp, line})
		}
	}
	content := func(value any, pointer, role string) {
		if role != "user" && role != "assistant" {
			return
		}
		if _, ok := value.(string); ok {
			add(value, pointer, role)
			return
		}
		if list, ok := value.([]any); ok {
			for i, v := range list {
				block := jsonMap(v)
				switch jsonString(block["type"]) {
				case "text", "input_text", "output_text":
					add(block["text"], fmt.Sprintf("%s/%d/text", pointer, i), role)
				}
			}
		}
	}
	switch jsonString(obj["type"]) {
	case "event_msg":
		p := jsonMap(obj["payload"])
		switch jsonString(p["type"]) {
		case "user_message":
			add(p["message"], "/payload/message", "user")
		case "agent_message":
			add(p["message"], "/payload/message", "assistant")
		}
	case "response_item":
		p := jsonMap(obj["payload"])
		if jsonString(p["type"]) == "message" && jsonString(p["phase"]) != "analysis" {
			content(p["content"], "/payload/content", jsonString(p["role"]))
		}
	case "agent_message":
		add(obj["message"], "/message", "assistant")
	case "user", "assistant", "message":
		m := jsonMap(obj["message"])
		role := jsonString(m["role"])
		if role == "" {
			role = jsonString(obj["type"])
		}
		content(m["content"], "/message/content", role)
	case "":
		content(obj["content"], "/content", jsonString(obj["role"]))
		// OpenCode document messages use info.role and text parts.
		role := jsonString(jsonMap(obj["info"])["role"])
		if role == "user" || role == "assistant" {
			if parts, ok := obj["parts"].([]any); ok {
				for i, v := range parts {
					p := jsonMap(v)
					if jsonString(p["type"]) == "text" {
						add(p["text"], fmt.Sprintf("/parts/%d/text", i), role)
					}
				}
			}
		}
	}
	return fields
}

func evidenceFields(data []byte, path string) ([]evidenceField, bool) {
	if !utf8.Valid(data) {
		return nil, true
	}
	text := string(data)
	trimmed := strings.TrimSpace(text)
	structured := strings.HasSuffix(strings.ToLower(path), ".json") || strings.HasSuffix(strings.ToLower(path), ".jsonl") || strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[")
	if !structured {
		return []evidenceField{{text: text, line: 1}}, false
	}
	var doc map[string]any
	if json.Unmarshal(data, &doc) == nil {
		if messages, ok := doc["messages"].([]any); ok {
			var fields []evidenceField
			for i, msg := range messages {
				fields = append(fields, evidenceJSONFields(jsonMap(msg), fmt.Sprintf("/messages/%d", i), 1)...)
			}
			return fields, len(fields) == 0
		}
		fields := evidenceJSONFields(doc, "", 1)
		return fields, len(fields) == 0
	}
	var fields []evidenceField
	incomplete := false
	for i, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var obj map[string]any
		if json.Unmarshal(line, &obj) != nil {
			incomplete = true
			continue
		}
		fields = append(fields, evidenceJSONFields(obj, "", i+1)...)
	}
	return fields, incomplete || len(fields) == 0
}

func collectEvidence(ctx context.Context, brainDir, branch, query string, limit int) (evidenceCandidates, error) {
	c := evidenceCandidates{Spans: []evidenceSpan{}, State: "available"}
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		c.State = "unavailable"
		c.Warnings = append(c.Warnings, "session manifest unavailable")
		return c, nil
	}
	if manifest == nil || manifest.Sources == nil || manifest.Sources.Sessions == nil {
		c.State = "unavailable"
		c.Warnings = append(c.Warnings, "no canonical sessions declared; absence of evidence is not evidence of absence")
		return c, nil
	}
	guard, err := loadSessionReadGuard(brainDir, manifest)
	if err != nil {
		return c, err
	}
	// Facts are retrieval handles only. Never substitute distilled claims for bytes.
	anchored := map[string]bool{}
	facts, err := loadFacts(brainDir, branch)
	if err != nil {
		c.Warnings = append(c.Warnings, "fact store unavailable; searching canonical sessions only")
	} else {
		for _, f := range rankFacts(guardFactRecords(guard, facts), query, limit, false) {
			for _, a := range f.Provenance {
				if !guard.blocksFactAnchor(a) {
					anchored[a.SessionID] = true
				}
			}
		}
	}
	if manifest.Sources.Facts != nil {
		if warning := missingFactStoreWarningForBranch(brainDir, manifest.Sources.Facts, branch); warning != "" {
			c.Warnings = append(c.Warnings, warning)
		}
	}
	type doc struct {
		fields  []evidenceField
		session exportSession
		hash    string
		score   int
	}
	var docs []doc
	seen := map[string]exportSession{}
	total := 0
	for _, s := range manifest.Sources.Sessions.Sessions {
		b := s.Branch
		if b == "" {
			b = manifest.DefaultBranch
		}
		if b == "" {
			b = distillDefaultBranch
		}
		if b != branch || guard.blocksExportSession(s) {
			continue
		}
		if prev, ok := seen[s.SessionID]; ok {
			if prev.TranscriptPath != s.TranscriptPath || !prev.CreatedAt.Equal(s.CreatedAt) {
				return c, fmt.Errorf("ambiguous session identity in evidence branch")
			}
			continue
		}
		seen[s.SessionID] = s
		if c.SessionsScanned >= 128 || total >= 32*1024*1024 {
			c.InputTruncated = true
			break
		}
		if err := ctx.Err(); err != nil {
			return c, err
		}
		c.SessionsScanned++
		data, err := readCanonicalHistoryTranscript(ctx, brainDir, s.TranscriptPath)
		if err != nil {
			if ctx.Err() != nil {
				return c, ctx.Err()
			}
			c.State = "partial"
			c.Warnings = append(c.Warnings, fmt.Sprintf("canonical session %s unavailable", s.SessionID))
			continue
		}
		total += len(data)
		fields, incomplete := evidenceFields(data, s.TranscriptPath)
		if incomplete {
			c.State = "partial"
			c.Warnings = append(c.Warnings, fmt.Sprintf("session %s contains unreadable or unsupported conversation content", s.SessionID))
		}
		var visible strings.Builder
		for _, f := range fields {
			visible.WriteString(f.text)
			visible.WriteByte('\n')
		}
		score := evidenceQueryScore(visible.String(), query)
		if anchored[s.SessionID] {
			score++
		}
		if score > 0 && len(fields) > 0 {
			docs = append(docs, doc{fields, s, evidenceHash(data), score})
		}
	}
	sort.SliceStable(docs, func(i, j int) bool { return docs[i].score > docs[j].score })
	c.SessionsMatched = len(docs)
	if len(docs) > limit {
		docs = docs[:limit]
		c.InputTruncated = true
	}
	// Rank bounded per-session pools before applying the global input ceiling.
	// Round-robin admission prevents a long first session from consuming every
	// candidate slot. No gold labels, answer strings or inferred facts are used.
	pools := make([][]evidenceRankedSpan, len(docs))
	for di, d := range docs {
		scanned := 0
		for _, f := range d.fields {
			timestamp := f.timestamp
			if _, err := time.Parse(time.RFC3339, timestamp); err != nil {
				timestamp = ""
				if !d.session.CreatedAt.IsZero() {
					timestamp = d.session.CreatedAt.UTC().Format(time.RFC3339)
				}
			}
			contentHash := evidenceHash([]byte(f.text))
			for _, r := range evidenceBlocks(f.text) {
				if scanned >= 4096 {
					c.InputTruncated = true
					break
				}
				scanned++
				span := evidenceSpan{SessionID: d.session.SessionID, Branch: branch, Path: filepath.ToSlash(d.session.TranscriptPath), Line: f.line, JSONPointer: f.pointer, Role: f.role, Timestamp: timestamp, SourceSHA256: d.hash, ContentSHA256: contentHash, StartByte: r[0], EndByte: r[1], Text: f.text[r[0]:r[1]]}
				cost := evidenceCandidateCost(span)
				if cost > evidenceCandidateBytes {
					c.InputTruncated = true
					continue
				}
				identity, _ := json.Marshal([]any{branch, span.SessionID, span.Path, span.SourceSHA256, span.Line, span.JSONPointer, span.ContentSHA256, r[0], r[1]})
				span.ID = "span:" + evidenceHash(identity)
				score := float64(evidenceQueryScore(span.Text, query) + 1)
				// Query coverage per length keeps lengthy generic advice from dominating
				// concise user evidence, while still retaining relevant assistant answers.
				length := len(span.Text)
				if length < 512 {
					length = 512
				}
				score /= math.Sqrt(float64(length))
				if span.Role == "user" {
					score *= 1.5
				}
				ranked := evidenceRankedSpan{span, score, scanned, cost}
				pool := pools[di]
				if len(pool) < 128 {
					pool = append(pool, ranked)
				} else {
					c.InputTruncated = true
					worst := 0
					for i := range pool {
						if evidenceRankedBefore(pool[worst], pool[i]) {
							worst = i
						}
					}
					if evidenceRankedBefore(ranked, pool[worst]) {
						pool[worst] = ranked
					}
				}
				pools[di] = pool
			}
			if scanned >= 4096 {
				c.InputTruncated = true
				break
			}
		}
		sort.SliceStable(pools[di], func(i, j int) bool { return evidenceRankedBefore(pools[di][i], pools[di][j]) })
		pools[di] = evidenceInterleaveRoles(pools[di])
	}
	inputSize := 0
	for round := 0; round < 128; round++ {
		for _, pool := range pools {
			if round >= len(pool) {
				continue
			}
			item := pool[round]
			if len(c.Spans) >= 128 || inputSize+item.cost > evidenceCandidateBytes {
				c.InputTruncated = true
				continue
			}
			c.Spans = append(c.Spans, item.span)
			inputSize += item.cost
		}
	}
	if c.InputTruncated {
		c.Warnings = append(c.Warnings, "candidate limits omitted context before selection")
	}
	if c.State == "partial" && len(c.Spans) == 0 {
		c.State = "unavailable"
	}
	return c, nil
}

const evidenceCandidateBytes = 24 * 1024

type evidenceRankedSpan struct {
	span  evidenceSpan
	score float64
	order int
	cost  int
}

func evidenceRankedBefore(a, b evidenceRankedSpan) bool {
	if a.score != b.score {
		return a.score > b.score
	}
	return a.order < b.order
}

func evidenceCandidateCost(s evidenceSpan) int {
	// Upper-bound a compact block plus its field/source metadata. Public output
	// still carries full anchors and is charged against the separate output cap.
	b, _ := json.Marshal([2]string{"b127", s.Text})
	cost := len(b) + 160
	// RFC3339 permits arbitrarily long fractional seconds. Charge actual field
	// metadata when it exceeds the ordinary allowance instead of undercounting it.
	field, _ := json.Marshal(evidenceWireField{Line: s.Line, Role: s.Role, At: s.Timestamp, Blocks: [][2]string{{"b127", s.Text}}})
	if actual := len(field) + 64; actual > cost {
		cost = actual
	}
	return cost
}

// Conversation matching uses positive term coverage. History's specialist
// code heuristics and long-record penalty can otherwise cancel real matches.
// Singular/plural expansion affects retrieval only, never original source text.
func evidenceQueryScore(text, query string) int {
	normalized := normalizeHistorySearchText(text)
	score := 0
	for _, term := range historyQueryTerms(query) {
		if strings.Contains(normalized, term) {
			score++
			continue
		}
		if len(term) > 3 && strings.HasSuffix(term, "s") && !strings.HasSuffix(term, "ss") && strings.Contains(normalized, strings.TrimSuffix(term, "s")) {
			score++
		}
	}
	if score == 0 && historyTextMatchesQuery(text, query) {
		return 1
	}
	return score
}

// Give both speakers an opportunity before filling the remaining slots.
// Two user blocks then one assistant/unknown block preserve user histories
// without excluding answers that exist only in assistant messages.
func evidenceInterleaveRoles(pool []evidenceRankedSpan) []evidenceRankedSpan {
	var users, others []evidenceRankedSpan
	for _, item := range pool {
		if item.span.Role == "user" {
			users = append(users, item)
		} else {
			others = append(others, item)
		}
	}
	out := make([]evidenceRankedSpan, 0, len(pool))
	u, a := 0, 0
	for u < len(users) || a < len(others) {
		for n := 0; n < 2 && u < len(users); n++ {
			out = append(out, users[u])
			u++
		}
		if a < len(others) {
			out = append(out, others[a])
			a++
		}
	}
	return out
}
