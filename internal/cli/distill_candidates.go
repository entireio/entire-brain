package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// distillCandidateSchemaVersion versions the deterministic transcript
// normalization, candidate selection, and rendered card contract together.
// A change that can alter turn/card IDs or the bytes sent to the extraction
// agent must bump this value.
const (
	distillCandidateSchemaVersion = 4
	// Candidate calls deliberately use one bounded card each. This keeps the
	// existing six-fact output cap and one-anchor parser meaningful until a
	// future candidate-ID-framed output protocol can attribute multi-card packs.
	distillCandidateMinChunkBytes       = 4 * 1024
	distillCandidateTurnMaxRunes        = 1536
	distillCandidateMaxCards            = 512
	distillCandidateMaxInputBytes       = 16 << 20
	distillCandidateMaxRawBytes         = 64 << 20
	distillCandidateMaxRecordBytes      = 2 << 20
	distillCandidateMaxMessageParts     = 4096
	distillCandidateMaxDocumentParts    = 32768
	distillCandidateMaxTurns            = 8192
	distillCandidateMaxTurnBytes        = 16 << 20
	distillCandidateMaxSourceFieldBytes = 8 << 10
	// Preflight retains only rendered/redacted candidate inputs, never raw
	// transcripts. Bound the whole immutable run snapshot so complete-before-
	// egress validation cannot turn a large repository into unbounded memory.
	distillCandidateMaxPreflightBytes = 256 << 20
)

type distillTranscriptRoleV1 string

const (
	distillTranscriptRoleUserV1      distillTranscriptRoleV1 = "user"
	distillTranscriptRoleAssistantV1 distillTranscriptRoleV1 = "assistant"
)

type distillTranscriptOriginV1 string

const (
	distillOriginCodexAgentMessageV1       distillTranscriptOriginV1 = "codex.agent_message"
	distillOriginCodexEventMessageV1       distillTranscriptOriginV1 = "codex.event_message"
	distillOriginCodexResponseMessageV1    distillTranscriptOriginV1 = "codex.response_message"
	distillOriginClaudeMessageV1           distillTranscriptOriginV1 = "claude.message"
	distillOriginPiMessageV1               distillTranscriptOriginV1 = "pi.message"
	distillOriginOpenCodeDocumentMessageV1 distillTranscriptOriginV1 = "opencode.document_message"
)

// distillCandidateSourceV1 is the stable source identity carried by every card.
// LatestCheckpoint is provenance, not part of turn IDs: appending a checkpoint
// must not rename an unchanged earlier turn.
type distillCandidateSourceV1 struct {
	SessionID        string `json:"session_id,omitempty"`
	LatestCheckpoint string `json:"checkpoint_id,omitempty"`
	Branch           string `json:"branch,omitempty"`
	Transcript       string `json:"transcript,omitempty"`
}

// distillNormalizedTurnV1 is one visible user/assistant narrative turn. Origins
// and SourceLines are plural because Codex commonly exports the same semantic
// turn twice (event_msg plus response_item); normalization collapses that pair
// without discarding either original source anchor.
type distillNormalizedTurnV1 struct {
	ID          string                      `json:"id"`
	Role        distillTranscriptRoleV1     `json:"role"`
	Origins     []distillTranscriptOriginV1 `json:"origins"`
	SourceLines []int                       `json:"source_lines"`
	StartLine   int                         `json:"start_line"`
	EndLine     int                         `json:"end_line"`
	Text        string                      `json:"text"`
	DirectUser  bool                        `json:"direct_user,omitempty"`
}

type distillNormalizedTranscriptV1 struct {
	SchemaVersion int                       `json:"schema_version"`
	Recognized    bool                      `json:"recognized"`
	Unsupported   bool                      `json:"unsupported,omitempty"`
	Overflow      bool                      `json:"overflow,omitempty"`
	Source        distillCandidateSourceV1  `json:"source"`
	Turns         []distillNormalizedTurnV1 `json:"turns"`
	// Repository evidence is bounded metadata from the canonical session
	// manifest. It is never copied wholesale into provider input.
	CheckpointSuccess bool     `json:"-"`
	FilesTouched      []string `json:"-"`
	AgentReview       bool     `json:"-"`
}

type distillCandidateCueV1 string

const (
	distillCandidateCueRuleV1           distillCandidateCueV1 = "rule"
	distillCandidateCuePreferenceV1     distillCandidateCueV1 = "preference"
	distillCandidateCueCorrectionV1     distillCandidateCueV1 = "correction"
	distillCandidateCueDecisionV1       distillCandidateCueV1 = "decision"
	distillCandidateCueClosedNegativeV1 distillCandidateCueV1 = "closed_negative"
	distillCandidateCueInvariantV1      distillCandidateCueV1 = "invariant"
	distillCandidateCueGotchaV1         distillCandidateCueV1 = "gotcha"
	distillCandidateCueAcceptanceV1     distillCandidateCueV1 = "acceptance"
)

// distillCandidateTriggerV1 makes authority explicit. A user trigger is direct
// evidence. An assistant trigger is useful enough to review, but is never
// authoritative by default; it is marked as eligible for corroboration against
// another user turn, repository evidence, or an independent session.
type distillCandidateTriggerV1 struct {
	TurnID                string                  `json:"turn_id"`
	Role                  distillTranscriptRoleV1 `json:"role"`
	Cues                  []distillCandidateCueV1 `json:"cues"`
	Authoritative         bool                    `json:"authoritative"`
	CorroborationEligible bool                    `json:"corroboration_eligible"`
}

type distillCandidateCardV1 struct {
	SchemaVersion int                         `json:"schema_version"`
	ID            string                      `json:"id"`
	Source        distillCandidateSourceV1    `json:"source"`
	StartLine     int                         `json:"start_line"`
	EndLine       int                         `json:"end_line"`
	Triggers      []distillCandidateTriggerV1 `json:"triggers"`
	Evidence      distillCandidateEvidenceV1  `json:"evidence,omitempty"`
	Turns         []distillNormalizedTurnV1   `json:"turns"`
}

type distillCandidateEvidenceV1 struct {
	CheckpointSuccess bool `json:"checkpoint_success,omitempty"`
	SameLocus         bool `json:"same_locus,omitempty"`
}

// distillCandidateAnchorV1 survives packing so orchestration does not have to
// parse the rendered JSON merely to recover candidate provenance.
type distillCandidateAnchorV1 struct {
	CandidateID  string `json:"candidate_id"`
	SessionID    string `json:"session_id,omitempty"`
	CheckpointID string `json:"checkpoint_id,omitempty"`
	Branch       string `json:"branch,omitempty"`
	Transcript   string `json:"transcript,omitempty"`
	StartLine    int    `json:"start_line"`
	EndLine      int    `json:"end_line"`
}

// distillCandidateBatchV1 is an indivisible-card packing result. Chunk is ready
// for the existing extraction runner; CandidateIDs and Anchors retain the typed
// identity needed by dry-run/accounting and output attribution.
type distillCandidateBatchV1 struct {
	CandidateIDs []string                   `json:"candidate_ids"`
	Anchors      []distillCandidateAnchorV1 `json:"anchors"`
	Chunk        transcriptChunk            `json:"-"`
}

type rawDistillTurnV1 struct {
	Role        distillTranscriptRoleV1
	Origins     []distillTranscriptOriginV1
	SourceLines []int
	StartLine   int
	EndLine     int
	Text        string
	DirectUser  bool
}

// normalizeDistillTranscriptV1 turns the supported Codex, Claude, Pi, and
// OpenCode transcript dialects into typed visible narrative. It intentionally
// has no plain-text fallback: an untyped line cannot prove its role or that it
// is not tool/system mechanics.
func normalizeDistillTranscriptV1(session exportSession, resolvedBranch, content string) distillNormalizedTranscriptV1 {
	branch := strings.TrimSpace(resolvedBranch)
	if branch == "" {
		branch = strings.TrimSpace(session.Branch)
	}
	sessionID := strings.TrimSpace(session.SessionID)
	checkpointID := strings.TrimSpace(session.LatestCheckpoint)
	transcript := strings.TrimSpace(session.TranscriptPath)
	if len(sessionID) > distillCandidateMaxSourceFieldBytes ||
		len(checkpointID) > distillCandidateMaxSourceFieldBytes ||
		len(branch) > distillCandidateMaxSourceFieldBytes ||
		len(transcript) > distillCandidateMaxSourceFieldBytes {
		return distillNormalizedTranscriptV1{SchemaVersion: distillCandidateSchemaVersion, Overflow: true}
	}
	source := distillCandidateSourceV1{
		SessionID:        sessionID,
		LatestCheckpoint: checkpointID,
		Branch:           branch,
		Transcript:       filepath.ToSlash(transcript),
	}
	evidence := distillNormalizedTranscriptV1{
		SchemaVersion:     distillCandidateSchemaVersion,
		Source:            source,
		CheckpointSuccess: distillCandidateCheckpointSucceededV1(session),
		FilesTouched:      normalizeDistillCandidateFilesTouchedV1(session.FilesTouched),
		AgentReview:       distillCandidateSessionIsAgentReviewV1(session),
	}
	// Candidate IDs must survive transcript relocation. A missing logical
	// session identity cannot meet that contract, so fail closed instead of
	// silently deriving durable/cache identity from a mutable path.
	if sessionID == "" {
		evidence.Unsupported = true
		return evidence
	}
	if len(content) > distillCandidateMaxRawBytes {
		evidence.Overflow = true
		return evidence
	}
	directUserAllowed := distillCandidateSessionHasDirectUserAuthorityV1(session)

	recognized := strings.TrimSpace(content) == ""
	unsupported, overflow := false, false
	var raw []rawDistillTurnV1
	turnBytes := 0
	appendTurn := func(turn rawDistillTurnV1) bool {
		turnBytes += len(turn.Text)
		if len(raw) >= distillCandidateMaxTurns || turnBytes > distillCandidateMaxTurnBytes {
			overflow = true
			return false
		}
		raw = append(raw, turn)
		return true
	}
	if messages, ok := parseCandidateDocumentConversationV1(content); ok {
		recognized = true
		for _, message := range messages {
			if !message.KnownShape {
				unsupported = true
				continue
			}
			role := distillTranscriptRoleV1(strings.ToLower(strings.TrimSpace(message.Role)))
			if role != distillTranscriptRoleUserV1 && role != distillTranscriptRoleAssistantV1 {
				continue
			}
			messageText := message.Text
			if role == distillTranscriptRoleUserV1 {
				messageText = stripDistillCandidateInjectedSpansV1(distillCandidateHumanCommandArgsV1(messageText))
			}
			if text := normalizeDistillCandidateTextV1(messageText); text != "" && !isDistillCandidateMechanicV1(role, text) {
				if !appendTurn(rawDistillTurnV1{
					Role:        role,
					Origins:     []distillTranscriptOriginV1{distillOriginOpenCodeDocumentMessageV1},
					SourceLines: []int{message.Line},
					StartLine:   message.Line,
					EndLine:     message.Line,
					Text:        text,
					DirectUser:  role == distillTranscriptRoleUserV1 && directUserAllowed,
				}) {
					break
				}
			}
		}
	} else {
		for index, start := 0, 0; start <= len(content); index++ {
			end := strings.IndexByte(content[start:], '\n')
			last := end < 0
			if last {
				end = len(content) - start
			}
			line := content[start : start+end]
			trimmed := strings.TrimSpace(line)
			if trimmed != "" {
				if len(trimmed) > distillCandidateMaxRecordBytes {
					if isRecognizedOversizedDistillCandidateMechanicV1(trimmed) {
						recognized = true
					} else {
						overflow = true
						break
					}
				}
				if len(trimmed) <= distillCandidateMaxRecordBytes && !strings.HasPrefix(trimmed, "{") {
					unsupported = true
				} else if len(trimmed) <= distillCandidateMaxRecordBytes {
					var obj map[string]any
					if json.Unmarshal([]byte(trimmed), &obj) != nil {
						unsupported = true
					} else {
						turn, turnOK := distillCandidateTurnFromJSONV1(obj, index+1, directUserAllowed)
						recordOK := isRecognizedDistillCandidateRecordV1(obj)
						if recordOK {
							recognized = true
						} else {
							// Candidate mode is complete-or-refused. An unknown record
							// may be narrative under a future dialect even when today's
							// field names do not reveal it.
							unsupported = true
						}
						if recordOK && turnOK && !appendTurn(turn) {
							break
						}
					}
				}
			}
			if overflow || last {
				break
			}
			start += end + 1
		}
	}

	raw = dedupeDistillCandidateTurnsV1(raw)
	turns := make([]distillNormalizedTurnV1, 0, len(raw))
	occurrences := map[string]int{}
	sourceIdentity := distillCandidateSourceIdentityV1(source)
	for _, turn := range raw {
		key := string(turn.Role) + "\x00" + turn.Text
		occurrences[key]++
		turns = append(turns, distillNormalizedTurnV1{
			ID: distillCandidateStableIDV1(
				"turn-v1:",
				sourceIdentity,
				string(turn.Role),
				turn.Text,
				strconv.Itoa(occurrences[key]),
			),
			Role:        turn.Role,
			Origins:     append([]distillTranscriptOriginV1(nil), turn.Origins...),
			SourceLines: append([]int(nil), turn.SourceLines...),
			StartLine:   turn.StartLine,
			EndLine:     turn.EndLine,
			Text:        turn.Text,
			DirectUser:  turn.DirectUser,
		})
	}

	evidence.Recognized = recognized
	evidence.Unsupported = unsupported
	evidence.Overflow = overflow
	evidence.Turns = turns
	return evidence
}

func distillCandidateTurnFromJSONV1(obj map[string]any, line int, directUserAllowed bool) (rawDistillTurnV1, bool) {
	if distillCandidateJSONBoolV1(obj["isMeta"]) ||
		distillCandidateJSONBoolV1(obj["isCompactSummary"]) ||
		distillCandidateJSONBoolV1(obj["isVisibleInTranscriptOnly"]) ||
		distillCandidateJSONBoolV1(obj["isApiErrorMessage"]) ||
		(jsonString(obj["type"]) == "assistant" && (strings.TrimSpace(jsonString(obj["error"])) != "" || obj["apiErrorStatus"] != nil)) {
		return rawDistillTurnV1{}, false
	}
	typeName := jsonString(obj["type"])
	payload := jsonMap(obj["payload"])
	var role distillTranscriptRoleV1
	var origin distillTranscriptOriginV1
	var text string

	switch typeName {
	case "":
		role = distillTranscriptRoleV1(strings.ToLower(strings.TrimSpace(jsonString(obj["role"]))))
		if role != distillTranscriptRoleUserV1 && role != distillTranscriptRoleAssistantV1 {
			return rawDistillTurnV1{}, false
		}
		message := jsonMap(obj["message"])
		if len(message) == 0 {
			return rawDistillTurnV1{}, false
		}
		if nestedRole := strings.ToLower(strings.TrimSpace(jsonString(message["role"]))); nestedRole != "" && nestedRole != string(role) {
			return rawDistillTurnV1{}, false
		}
		if distillCandidateJSONBoolV1(message["isMeta"]) || distillCandidateJSONBoolV1(message["isCompactSummary"]) || distillCandidateJSONBoolV1(message["isVisibleInTranscriptOnly"]) {
			return rawDistillTurnV1{}, false
		}
		origin = distillOriginClaudeMessageV1
		text = distillCandidateVisibleTextV1(message["content"], role)
	case "agent_message":
		role = distillTranscriptRoleAssistantV1
		origin = distillOriginCodexAgentMessageV1
		text = jsonString(obj["message"])
	case "event_msg":
		switch jsonString(payload["type"]) {
		case "user_message":
			role = distillTranscriptRoleUserV1
		case "agent_message":
			role = distillTranscriptRoleAssistantV1
		default:
			// task_complete repeats the final assistant narrative; task/tool/token
			// events are mechanics rather than additional turns.
			return rawDistillTurnV1{}, false
		}
		origin = distillOriginCodexEventMessageV1
		text = jsonString(payload["message"])
	case "response_item":
		if jsonString(payload["type"]) != "message" {
			return rawDistillTurnV1{}, false
		}
		role = distillTranscriptRoleV1(strings.ToLower(strings.TrimSpace(jsonString(payload["role"]))))
		if role != distillTranscriptRoleUserV1 && role != distillTranscriptRoleAssistantV1 {
			return rawDistillTurnV1{}, false
		}
		origin = distillOriginCodexResponseMessageV1
		text = distillCandidateVisibleTextV1(payload["content"], role)
	case "assistant", "user":
		message := jsonMap(obj["message"])
		if len(message) == 0 && isLegacyClaudeCandidateEnvelopeV1(obj) {
			role = distillTranscriptRoleV1(typeName)
			origin = distillOriginClaudeMessageV1
			var ok bool
			text, ok = legacyClaudeCandidateVisibleTextV1(obj["content"], role)
			if !ok {
				return rawDistillTurnV1{}, false
			}
			break
		}
		if distillCandidateJSONBoolV1(message["isMeta"]) ||
			distillCandidateJSONBoolV1(message["isCompactSummary"]) ||
			distillCandidateJSONBoolV1(message["isVisibleInTranscriptOnly"]) {
			return rawDistillTurnV1{}, false
		}
		if nestedRole := strings.ToLower(strings.TrimSpace(jsonString(message["role"]))); nestedRole != "" && nestedRole != typeName {
			return rawDistillTurnV1{}, false
		}
		role = distillTranscriptRoleV1(typeName)
		origin = distillOriginClaudeMessageV1
		text = distillCandidateVisibleTextV1(message["content"], role)
	case "message":
		message := jsonMap(obj["message"])
		if distillCandidateJSONBoolV1(message["isMeta"]) ||
			distillCandidateJSONBoolV1(message["isCompactSummary"]) ||
			distillCandidateJSONBoolV1(message["isVisibleInTranscriptOnly"]) {
			return rawDistillTurnV1{}, false
		}
		role = distillTranscriptRoleV1(strings.ToLower(strings.TrimSpace(jsonString(message["role"]))))
		if role != distillTranscriptRoleUserV1 && role != distillTranscriptRoleAssistantV1 {
			return rawDistillTurnV1{}, false
		}
		origin = distillOriginPiMessageV1
		text = distillCandidateVisibleTextV1(message["content"], role)
	default:
		return rawDistillTurnV1{}, false
	}

	if role == distillTranscriptRoleUserV1 {
		text = distillCandidateHumanCommandArgsV1(text)
		text = stripDistillCandidateInjectedSpansV1(text)
	}
	text = normalizeDistillCandidateTextV1(text)
	if text == "" || isDistillCandidateMechanicV1(role, text) {
		return rawDistillTurnV1{}, false
	}
	return rawDistillTurnV1{
		Role:        role,
		Origins:     []distillTranscriptOriginV1{origin},
		SourceLines: []int{line},
		StartLine:   line,
		EndLine:     line,
		Text:        text,
		DirectUser:  role == distillTranscriptRoleUserV1 && directUserAllowed && !distillCandidateRecordIsSidechainV1(obj),
	}, true
}

func distillCandidateSessionHasDirectUserAuthorityV1(session exportSession) bool {
	return !session.IsTask && strings.TrimSpace(session.ToolUseID) == "" && !distillCandidateSessionIsAgentReviewV1(session)
}

func distillCandidateSessionIsAgentReviewV1(session exportSession) bool {
	kind := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(session.Kind), "-", "_"))
	return kind == "agent_review"
}

func distillCandidateCheckpointSucceededV1(session exportSession) bool {
	if session.Summary == nil || strings.TrimSpace(session.LatestCheckpoint) == "" {
		return false
	}
	outcome := strings.ToLower(strings.TrimSpace(session.Summary.Outcome))
	switch outcome {
	case "success", "succeeded", "shipped", "completed", "passed":
		return true
	default:
		return false
	}
}

func normalizeDistillCandidateFilesTouchedV1(files []string) []string {
	seen := make(map[string]struct{}, len(files))
	out := make([]string, 0, len(files))
	for _, file := range files {
		file = filepath.ToSlash(strings.TrimSpace(file))
		if file == "" || len(file) > distillCandidateMaxSourceFieldBytes {
			continue
		}
		if _, duplicate := seen[file]; duplicate {
			continue
		}
		seen[file] = struct{}{}
		out = append(out, file)
	}
	sort.Strings(out)
	return out
}

func distillCandidateRecordIsSidechainV1(obj map[string]any) bool {
	if distillCandidateJSONBoolV1(obj["isSidechain"]) {
		return true
	}
	return distillCandidateJSONBoolV1(jsonMap(obj["message"])["isSidechain"])
}

// distillCandidateVisibleTextV1 is an allow-list, not the legacy permissive
// block reader: tool, reasoning, thinking, and unknown blocks never become
// candidate evidence merely because they happen to contain a text-like field.
func distillCandidateVisibleTextV1(content any, role distillTranscriptRoleV1) string {
	switch value := content.(type) {
	case string:
		return value
	case []any:
		var parts []string
		for _, item := range value {
			block := jsonMap(item)
			blockType := strings.ToLower(strings.TrimSpace(jsonString(block["type"])))
			allowed := blockType == "text" ||
				(role == distillTranscriptRoleUserV1 && blockType == "input_text") ||
				(role == distillTranscriptRoleAssistantV1 && blockType == "output_text")
			if !allowed {
				continue
			}
			if text := firstNonEmptyString(block["text"], block["input_text"], block["output_text"]); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, " ")
	default:
		return ""
	}
}

func normalizeDistillCandidateTextV1(text string) string {
	var normalized strings.Builder
	normalized.Grow(min(len(text), distillCandidateMaxRecordBytes))
	pendingSpace := false
	pendingNewlines := 0
	for _, char := range text {
		if unicode.IsSpace(char) {
			if normalized.Len() > 0 {
				pendingSpace = true
				if char == '\n' {
					pendingNewlines++
				}
			}
			continue
		}
		if pendingSpace {
			if pendingNewlines >= 2 {
				normalized.WriteString("\n\n")
			} else {
				normalized.WriteByte(' ')
			}
			pendingSpace = false
			pendingNewlines = 0
		}
		normalized.WriteRune(char)
	}
	return normalized.String()
}

var distillSlashCommandV1 = regexp.MustCompile(`^/[A-Za-z][A-Za-z0-9:_-]*(?:\s|$)`)
var distillCandidateCrossParagraphReferenceV1 = regexp.MustCompile(`(?i)\b(?:above|previous|preceding|following|former|latter|this|these|those)\b`)
var distillCommandArgsV1 = regexp.MustCompile(`(?s)<command-args>(.*?)</command-args>`)
var distillUserQueryV1 = regexp.MustCompile(`(?is)^\s*(?:<timestamp\b[^>]*>.*?</timestamp\s*>\s*)?<user_query\s*>\s*(.*?)\s*</user_query\s*>\s*$`)
var distillCandidateInjectedSpanPatternsV1 = []*regexp.Regexp{
	regexp.MustCompile(`(?is)<system-reminder\b[^>]*>.*?(?:</system-reminder\s*>|$)`),
	regexp.MustCompile(`(?is)<task-notification\b[^>]*>.*?(?:</task-notification\s*>|$)`),
	regexp.MustCompile(`(?is)<environment_context\b[^>]*>.*?(?:</environment_context\s*>|$)`),
}

// distillCandidateHumanCommandArgsV1 removes slash-command mechanics while
// retaining text the human actually supplied. Expanded command bodies remain
// excluded by isMeta/isVisibleInTranscriptOnly; a bare command without
// arguments contributes no evidence.
func distillCandidateHumanCommandArgsV1(text string) string {
	trimmed := strings.TrimSpace(text)
	if match := distillUserQueryV1.FindStringSubmatch(trimmed); len(match) == 2 {
		return strings.TrimSpace(match[1])
	}
	if match := distillCommandArgsV1.FindStringSubmatch(trimmed); len(match) == 2 {
		return strings.TrimSpace(match[1])
	}
	if loc := distillSlashCommandV1.FindStringIndex(trimmed); loc != nil {
		return strings.TrimSpace(trimmed[loc[1]:])
	}
	return text
}

// stripDistillCandidateInjectedSpansV1 removes known harness-owned blocks even
// when they are appended to a genuine human request. Whole-message wrapper
// detection is insufficient: Claude and Codex both emit mixed records whose
// human prefix is followed by system reminders, subagent notifications, or an
// environment envelope containing rule-like language.
func stripDistillCandidateInjectedSpansV1(text string) string {
	for _, pattern := range distillCandidateInjectedSpanPatternsV1 {
		text = pattern.ReplaceAllString(text, " ")
	}
	return text
}

// isRecognizedOversizedDistillCandidateMechanicV1 classifies only envelope
// fields into a fixed-size struct. encoding/json scans unknown payload fields
// without materializing their arrays/maps, so multi-megabyte tool output can be
// dropped safely while an oversized human/assistant narrative still refuses
// the candidate run.
func isRecognizedOversizedDistillCandidateMechanicV1(record string) bool {
	var envelope struct {
		Type                    string    `json:"type"`
		CustomType              string    `json:"customType"`
		SourceToolAssistantUUID string    `json:"sourceToolAssistantUUID"`
		ToolUseResult           *struct{} `json:"toolUseResult"`
		Payload                 struct {
			Type string `json:"type"`
			Role string `json:"role"`
		} `json:"payload"`
		Message struct {
			Role string `json:"role"`
		} `json:"message"`
	}
	decoder := json.NewDecoder(strings.NewReader(record))
	if err := decoder.Decode(&envelope); err != nil {
		return false
	}
	if _, err := decoder.Token(); err != io.EOF {
		return false
	}
	typeName := strings.TrimSpace(envelope.Type)
	switch typeName {
	case "event_msg":
		return isRecognizedDistillCandidateEventMechanicV1(strings.TrimSpace(envelope.Payload.Type))
	case "response_item":
		payloadType := strings.TrimSpace(envelope.Payload.Type)
		if payloadType == "message" {
			switch strings.ToLower(strings.TrimSpace(envelope.Payload.Role)) {
			case "developer", "system", "tool":
				return true
			default:
				return false
			}
		}
		return isRecognizedDistillCandidateResponseMechanicV1(payloadType)
	case "message":
		switch strings.ToLower(strings.TrimSpace(envelope.Message.Role)) {
		case "bashexecution", "toolresult", "developer", "system", "tool":
			return true
		default:
			return false
		}
	case "user":
		// Claude transports tool results as generated user-role records. The
		// source UUID plus structured toolUseResult binds the envelope to the
		// preceding tool call; none of its payload is direct-human authority.
		return strings.TrimSpace(envelope.SourceToolAssistantUUID) != "" && envelope.ToolUseResult != nil
	case "custom":
		switch strings.TrimSpace(envelope.CustomType) {
		case "review-settle-monitor-state", "session-pr-observed", "plannotator", "plannotator-execute":
			return true
		}
	case "custom_message":
		switch strings.TrimSpace(envelope.CustomType) {
		case "context-percent-compaction", "entire-context", "review-settle-monitor",
			"subagent-compaction-resume", "subagent-notify", "subagent-wait-subscription",
			"subagent_control_notice", "subagent_supervisor_request", "intercom_message", "plannotator-complete":
			return true
		}
	case "session_info", "relocated":
		return true
	default:
		return isRecognizedDistillCandidateTopLevelMechanicV1(typeName)
	}
	return false
}

func isRecognizedDistillCandidateEventMechanicV1(typeName string) bool {
	switch typeName {
	case "token_count", "item_completed", "agent_reasoning", "patch_apply_begin", "patch_apply_end",
		"exec_command_begin", "exec_command_end", "sub_agent_activity", "task_started", "task_complete",
		"thread_settings_applied", "thread_goal_updated", "mcp_tool_call_begin", "mcp_tool_call_end", "context_compacted",
		"web_search_begin", "web_search_end", "turn_aborted", "exited_review_mode", "entered_review_mode",
		"collab_waiting_end", "collab_agent_spawn_end", "collab_close_end", "collab_agent_interaction_end", "thread_name_updated",
		"thread_rolled_back", "view_image_tool_call", "dynamic_tool_call_request", "dynamic_tool_call_response":
		return true
	default:
		return false
	}
}

func isRecognizedDistillCandidateResponseMechanicV1(typeName string) bool {
	switch typeName {
	case "reasoning", "agent_message", "custom_tool_call", "custom_tool_call_output", "function_call", "function_call_output",
		"web_search_call", "tool_search_call", "tool_search_output", "computer_tool_call", "local_shell_call":
		return true
	default:
		return false
	}
}

func isRecognizedDistillCandidateTopLevelMechanicV1(typeName string) bool {
	switch typeName {
	case "session_meta", "turn_context", "system", "summary", "queue_operation",
		"token_usage_record",
		"permission-mode", "progress", "file-history-snapshot", "queue-operation",
		"session", "model_change", "thinking_level_change", "compaction", "compacted", "branch_summary", "result",
		"attachment", "last-prompt", "ai-title", "pr-link", "mode", "agent-name", "bridge-session",
		"custom-title", "worktree-state", "agent-color", "inter_agent_communication_metadata",
		"file-history-delta", "world_state", "frame-link", "atis-latch":
		return true
	default:
		return false
	}
}

func isRecognizedDistillCandidateRecordV1(obj map[string]any) bool {
	switch jsonString(obj["type"]) {
	case "":
		role := strings.ToLower(strings.TrimSpace(jsonString(obj["role"])))
		if role != string(distillTranscriptRoleUserV1) && role != string(distillTranscriptRoleAssistantV1) {
			return false
		}
		message, ok := obj["message"].(map[string]any)
		if !ok {
			return false
		}
		if nestedRole := strings.ToLower(strings.TrimSpace(jsonString(message["role"]))); nestedRole != "" && nestedRole != role {
			return false
		}
		return isRecognizedDistillContentV1(message["content"])
	case "agent_message":
		_, ok := obj["message"].(string)
		return ok
	case "event_msg":
		payload, ok := obj["payload"].(map[string]any)
		if !ok {
			return false
		}
		switch jsonString(payload["type"]) {
		case "user_message", "agent_message":
			_, ok := payload["message"].(string)
			return ok
		default:
			return isRecognizedDistillCandidateEventMechanicV1(jsonString(payload["type"]))
		}
	case "response_item":
		payload, ok := obj["payload"].(map[string]any)
		if !ok {
			return false
		}
		switch jsonString(payload["type"]) {
		case "message":
			role := strings.ToLower(strings.TrimSpace(jsonString(payload["role"])))
			return isRecognizedDistillContentV1(payload["content"]) && (role == string(distillTranscriptRoleUserV1) || role == string(distillTranscriptRoleAssistantV1) || role == "developer" || role == "system" || role == "tool")
		default:
			return isRecognizedDistillCandidateResponseMechanicV1(jsonString(payload["type"]))
		}
	case "assistant", "user":
		message, ok := obj["message"].(map[string]any)
		if !ok {
			if !isLegacyClaudeCandidateEnvelopeV1(obj) {
				return false
			}
			_, contentOK := legacyClaudeCandidateVisibleTextV1(obj["content"], distillTranscriptRoleV1(jsonString(obj["type"])))
			return contentOK
		}
		if nestedRole := strings.ToLower(strings.TrimSpace(jsonString(message["role"]))); nestedRole != "" && nestedRole != jsonString(obj["type"]) {
			return false
		}
		return isRecognizedDistillContentV1(message["content"])
	case "message":
		message, ok := obj["message"].(map[string]any)
		if !ok {
			return false
		}
		role := strings.ToLower(strings.TrimSpace(jsonString(message["role"])))
		return (role == "bashexecution" && message["content"] == nil) ||
			(isRecognizedDistillContentV1(message["content"]) && (role == string(distillTranscriptRoleUserV1) || role == string(distillTranscriptRoleAssistantV1) || role == "toolresult" || role == "developer" || role == "system" || role == "tool"))
	case "custom", "custom_message", "session_info":
		return isRecognizedPiCandidateMechanicV1(obj)
	case "relocated":
		_, cwdOK := obj["relocatedCwd"].(string)
		_, sessionOK := obj["sessionId"].(string)
		return cwdOK && sessionOK
	default:
		return isRecognizedDistillCandidateTopLevelMechanicV1(jsonString(obj["type"]))
	}
}

func isRecognizedPiCandidateMechanicV1(obj map[string]any) bool {
	subtype := strings.TrimSpace(jsonString(obj["customType"]))
	switch jsonString(obj["type"]) {
	case "custom":
		if _, ok := obj["data"].(map[string]any); !ok {
			return false
		}
		switch subtype {
		case "review-settle-monitor-state", "session-pr-observed", "plannotator", "plannotator-execute":
			return true
		default:
			return false
		}
	case "custom_message":
		if _, ok := obj["content"].(string); !ok {
			return false
		}
		if _, ok := obj["display"].(bool); !ok {
			return false
		}
		switch subtype {
		case "context-percent-compaction", "entire-context", "review-settle-monitor",
			"subagent-compaction-resume", "subagent-notify", "subagent-wait-subscription",
			"subagent_control_notice", "subagent_supervisor_request", "intercom_message", "plannotator-complete":
			return true
		default:
			return false
		}
	case "session_info":
		_, ok := obj["name"].(string)
		return ok
	default:
		return false
	}
}

func isLegacyClaudeCandidateEnvelopeV1(obj map[string]any) bool {
	if jsonString(obj["agent"]) != "claude-code" || strings.TrimSpace(jsonString(obj["cli_version"])) == "" {
		return false
	}
	_, versionOK := obj["v"].(float64)
	return versionOK
}

// legacyClaudeCandidateVisibleTextV1 recognizes the older exported Claude
// envelope without loosening generic user/assistant records. Direct user text
// lived in untyped {id,text} blocks; assistant narrative used typed text blocks.
func legacyClaudeCandidateVisibleTextV1(content any, role distillTranscriptRoleV1) (string, bool) {
	blocks, ok := content.([]any)
	if !ok {
		return "", false
	}
	var parts []string
	for _, item := range blocks {
		block, ok := item.(map[string]any)
		if !ok {
			return "", false
		}
		blockType := strings.ToLower(strings.TrimSpace(jsonString(block["type"])))
		if role == distillTranscriptRoleUserV1 && blockType == "" {
			if strings.TrimSpace(jsonString(block["id"])) == "" {
				return "", false
			}
			text, textOK := block["text"].(string)
			if !textOK {
				return "", false
			}
			parts = append(parts, text)
			continue
		}
		switch blockType {
		case "text":
			text, textOK := block["text"].(string)
			if !textOK {
				return "", false
			}
			parts = append(parts, text)
		case "tool_use", "tool_result", "thinking", "reasoning", "redacted_thinking":
			continue
		default:
			return "", false
		}
	}
	return strings.Join(parts, " "), true
}

func isRecognizedDistillContentV1(content any) bool {
	switch value := content.(type) {
	case string:
		return true
	case []any:
		for _, item := range value {
			block, ok := item.(map[string]any)
			if !ok {
				return false
			}
			switch strings.ToLower(strings.TrimSpace(jsonString(block["type"]))) {
			case "text", "input_text", "output_text", "thinking", "reasoning", "redacted_thinking", "tool_use", "tool_result",
				"toolcall", "input_image", "image", "document", "server_tool_use", "web_search_tool_result", "encrypted_content", "attachment", "fallback":
			default:
				return false
			}
		}
		return true
	default:
		return false
	}
}

func isDistillCandidateMechanicV1(role distillTranscriptRoleV1, text string) bool {
	text = strings.TrimSpace(text)
	if text == "" || isWrapperRequest(text) {
		return true
	}
	if role == distillTranscriptRoleUserV1 {
		return isConversationInjectedRequest(text)
	}
	return isConversationNoiseNarrative(text)
}

func distillCandidateJSONBoolV1(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		parsed, err := strconv.ParseBool(strings.TrimSpace(typed))
		return err == nil && parsed
	default:
		return false
	}
}

func dedupeDistillCandidateTurnsV1(turns []rawDistillTurnV1) []rawDistillTurnV1 {
	out := make([]rawDistillTurnV1, 0, len(turns))
	for _, turn := range turns {
		if len(out) == 0 {
			out = append(out, turn)
			continue
		}
		last := &out[len(out)-1]
		if last.Role != turn.Role || last.Text != turn.Text || !isCodexRepresentationDuplicateV1(last.Origins, turn.Origins) {
			out = append(out, turn)
			continue
		}
		last.Origins = appendUniqueDistillOriginsV1(last.Origins, turn.Origins...)
		last.SourceLines = appendUniqueSortedIntsV1(last.SourceLines, turn.SourceLines...)
		last.StartLine = min(last.StartLine, turn.StartLine)
		last.EndLine = max(last.EndLine, turn.EndLine)
	}
	return out
}

func isCodexRepresentationDuplicateV1(left, right []distillTranscriptOriginV1) bool {
	allCodex := func(origins []distillTranscriptOriginV1) bool {
		if len(origins) == 0 {
			return false
		}
		for _, origin := range origins {
			if !strings.HasPrefix(string(origin), "codex.") {
				return false
			}
		}
		return true
	}
	if !allCodex(left) || !allCodex(right) {
		return false
	}
	seen := map[distillTranscriptOriginV1]bool{}
	for _, origin := range left {
		seen[origin] = true
	}
	for _, origin := range right {
		if !seen[origin] {
			return true
		}
	}
	return false
}

func appendUniqueDistillOriginsV1(current []distillTranscriptOriginV1, values ...distillTranscriptOriginV1) []distillTranscriptOriginV1 {
	seen := make(map[distillTranscriptOriginV1]bool, len(current)+len(values))
	for _, value := range current {
		seen[value] = true
	}
	for _, value := range values {
		if !seen[value] {
			current = append(current, value)
			seen[value] = true
		}
	}
	sort.Slice(current, func(i, j int) bool { return current[i] < current[j] })
	return current
}

func appendUniqueSortedIntsV1(current []int, values ...int) []int {
	seen := make(map[int]bool, len(current)+len(values))
	for _, value := range current {
		seen[value] = true
	}
	for _, value := range values {
		if !seen[value] {
			current = append(current, value)
			seen[value] = true
		}
	}
	sort.Ints(current)
	return current
}

type distillCandidateCueMatcherV1 struct {
	Cue     distillCandidateCueV1
	Pattern *regexp.Regexp
}

var distillCandidateCueRulesV1 = []distillCandidateCueMatcherV1{
	// "make sure" is deliberately absent here. It overwhelmingly introduces a
	// current-task request; the durable forms remain covered by always/never and
	// the other explicit temporal markers below.
	{distillCandidateCueRuleV1, regexp.MustCompile(`(?i)\b(?:always|never|from now on|going forward|whenever|every time|do not|don't|requirement|source of truth)\b`)},
	{distillCandidateCuePreferenceV1, regexp.MustCompile(`(?i)\b(?:i|we)\s+(?:prefer|would rather|do not want|don't want|like)\b|\bi(?:'d| would) rather\b|\b(?:my|our) preference is\b|\bi want\b|\buse\b.{0,80}\b(?:over|rather than|instead of|not)\b`)},
	{distillCandidateCueCorrectionV1, regexp.MustCompile(`(?i)^no[,!.:]?\s+(?:use|keep|do|make|choose|prefer|replace|instead)\b|^(?:actually|instead|correction)\b|\b(?:i meant|not what i asked|that's wrong|that is wrong|you missed|stop doing|undo that|revert that|ignore the previous|replace\b.{0,80}\bwith)\b`)},
	{distillCandidateCueDecisionV1, regexp.MustCompile(`(?i)\b(?:decided|decision is|we agreed|agreed to|settled on|chosen|chose|go with|we'll use|we will use|we'll stick with|we will stick with|let's use|lets use|we no longer|instead of|tradeoff|replace\b.{0,80}\bwith)\b`)},
	{distillCandidateCueClosedNegativeV1, regexp.MustCompile(`(?i)\b(?:rejected|ruled out|abandoned|rolled back|dead end|revisit only|within noise|didn't work|did not work|doesn't work|does not work|failed because|no effect|not worth|won't use|will not use)\b|\btried\b.{0,120}\b(?:failed|but|however|no effect)\b|\bavoid\b.{0,80}\bbecause\b`)},
	{distillCandidateCueInvariantV1, regexp.MustCompile(`(?i)\b(?:invariant|contract requires|only if|never|always|must not|cannot|must be|must remain|must stay|source of truth)\b|\b(?:the|this|that|all|every)\b.{0,80}\bmust\b|\b(?:[a-z][a-z-]*\s+){0,3}[a-z][a-z-]*s\s+must\b|\b(?:is|are) required (?:to|for)\b`)},
	{distillCandidateCueGotchaV1, regexp.MustCompile(`(?i)\b(?:gotcha|footgun|beware|watch out|non-obvious|surprising|edge case|root cause|turns out|only when|unless|otherwise|silent(?:ly)?|hangs?|races?|stale|breaks if|breaks when|fails if|fails when)\b`)},
	{distillCandidateCueAcceptanceV1, regexp.MustCompile(`(?i)^(?:sounds good|approved|exactly|that's right|that is right|agreed|nice catch|ship it)(?:[.!]|$)|^yes[,!.]?\s+(?:do|use|keep|make|that's|that is)\b`)},
}

var distillCandidateQuestionV1 = regexp.MustCompile(`(?i)^(?:should|must|can|could|would|do|does|did|is|are|what|how|why|when)\b.*\?\s*$`)
var distillCandidateStrongQuestionDirectiveV1 = regexp.MustCompile(`(?i)\b(?:always|never|from now on|going forward|whenever|every time)\b`)
var distillCandidateAdjacentAcceptanceV1 = regexp.MustCompile(`(?i)^(?:yes|correct|that works|go ahead|do that)[.!]?\s*$`)
var distillCandidateBareYesV1 = regexp.MustCompile(`(?i)^yes[.!]?\s*$`)
var distillCandidateOneOffWantV1 = regexp.MustCompile(`(?i)\bi want\s+(?:you\s+)?to\b`)
var distillCandidateOneOffMutationDirectiveV1 = regexp.MustCompile(`(?i)\b(?:do not|don't)\s+(?:change|modify|edit|write|touch|create|delete|remove|commit|push)\s+(?:(?:any|the|these|those)\s+)?(?:files?|code|repository|repo|branch|commits?)\b`)

// An imperative plan/spec header scopes the rest of its turn to a one-off
// implementation request. Such a turn often contains words like "must",
// "invariant", acceptance criteria, and verification commands, but those are
// task requirements rather than durable repository knowledge.
var distillCandidateOneOffPlanSpecRequestV1 = regexp.MustCompile(`(?i)^\s*(?:please\s+)?(?:implement|execute|apply|carry out|complete)\s+(?:this|the following|the attached)\s+(?:plan|spec(?:ification)?|request)\s*[:.]`)

// These are action-shaped requests even when they do not use the more formal
// "Implement this plan:" heading above. They are intentionally limited to
// task nouns and operational targets, leaving lasting user decisions and
// invariants for the extraction gate.
var distillCandidateOneOffActionAssignmentV1 = regexp.MustCompile(`(?i)^\s*(?:please\s+)?(?:implement|test|verify|execute|apply|carry out|complete)\s+(?:(?:the|this)\s+)?(?:whole\s+)?(?:plan|spec(?:ification)?|request)\b|^\s*(?:please\s+)?(?:review|audit)\s+(?:(?:entire\s+)?trails?\s+[0-9][0-9,\s-]*|(?:pr|pull request)\s*#?\d+|(?:the\s+)?(?:named\s+)?audit\b)`)

var distillCandidateOneOffPlanningRequestV1 = regexp.MustCompile(`(?i)^\s*(?:(?:alright|ok(?:ay)?|then|now|please|so|great|good|fine|got it|sounds good)\b[\s,]*)*(?:make|create|write|draft)\s+(?:a\s+)?(?:[a-z][a-z0-9_-]*\s+){0,3}plan\b`)

// A campaign/runnable work block is useful only for the current execution.
// This requires the conventional header, not an incidental use of the word
// "goal" in an otherwise durable statement.
var distillCandidateOneOffCampaignRunbookV1 = regexp.MustCompile(`(?is)^\s*(?:#{1,6}\s*)?(?:goal|runbook|campaign)\s*:\s*.*\b(?:phase\s*\d+|deliverable|current\s+measured\s+standing|do\s+not\s+restart)\b`)

// "for now" and imperative "don't run" are current-run constraints. A
// direct "I want" project goal is left to extraction even when it happens to
// include those words: admission is deliberately broader than final memory.
var distillCandidateTemporaryRunConstraintV1 = regexp.MustCompile(`(?i)\b(?:for\s+now|right\s+now)\b|\b(?:do\s+not|don't)\s+(?:run|rush|hurry)\b|^\s*(?:please\s+)?(?:implement|test|verify|review|audit|run|re-?run|fix|update|create|delete|merge|push)\b.{0,240}\bnow\b`)

// Bare Yes is only a decision confirmation when the preceding assistant
// proposal is itself a decision. This recognizes operational offers without
// treating a generic "Want me to?" as a reason to discard a durable choice.
var distillCandidateOneOffAssistantOfferV1 = regexp.MustCompile(`(?is)\b(?:want\s+me\s+to|shall\s+i|should\s+i|would\s+you\s+like\s+me\s+to|may\s+i)\b.{0,240}\b(?:re-?run|run|push|commit|merge|review|audit|implement|test|verify|fix|update|create|delete|close)\b|\b(?:re-?run|run|push|commit|merge|review|audit|implement|test|verify|fix|update|create|delete|close)\b.{0,240}\b(?:want\s+me\s+to|shall\s+i|should\s+i|would\s+you\s+like\s+me\s+to|may\s+i)\b`)

// Task briefs and continuation commands are another high-precision one-off
// shape. They commonly contain "must", "make sure", acceptance criteria, and
// pasted review prose, but those words govern the current implementation task;
// they are not standing repository memory. Keep the prefix list deliberately
// narrow so an ordinary human rule or correction still reaches the quality
// gate.
var distillCandidateOneOffTaskBriefV1 = regexp.MustCompile(`(?i)^\s*(?:#{1,6}\s*)?(?:task(?:(?:\s+for\s+[a-z0-9_-]+(?:\s+in\s+[^:\n]+)?)|\s+[a-z])?\s*(?::|[-—])|fix\s*:|create\s+a\s+new\s+branch\s+to\b|fix\s+only\s+the\s+selected\s+review\s+findings\b|now\s+(?:also\s+)?(?:add|retry|implement|fix|run|test|verify|continue)\b|yes[,!.]?\s+keep\s+going\b)`)

func distillCandidateCuesV1(text string) []distillCandidateCueV1 {
	trimmed := strings.TrimSpace(text)
	if distillCandidateQuestionV1.MatchString(trimmed) && !distillCandidateStrongQuestionDirectiveV1.MatchString(trimmed) {
		return nil
	}
	if distillCandidateOneOffMutationDirectiveV1.MatchString(trimmed) && !distillCandidateStrongQuestionDirectiveV1.MatchString(trimmed) {
		return nil
	}
	if distillCandidateOneOffPlanSpecRequestV1.MatchString(trimmed) || distillCandidateOneOffTaskBriefV1.MatchString(trimmed) {
		return nil
	}
	if distillCandidateOneOffActionAssignmentV1.MatchString(trimmed) || distillCandidateOneOffPlanningRequestV1.MatchString(trimmed) || distillCandidateOneOffCampaignRunbookV1.MatchString(trimmed) {
		return nil
	}
	if distillCandidateTemporaryRunConstraintV1.MatchString(trimmed) &&
		!strings.Contains(strings.ToLower(trimmed), "i want") &&
		!distillCandidateStrongQuestionDirectiveV1.MatchString(trimmed) {
		return nil
	}
	var cues []distillCandidateCueV1
	for _, rule := range distillCandidateCueRulesV1 {
		if rule.Pattern.MatchString(text) {
			if rule.Cue == distillCandidateCuePreferenceV1 && distillCandidateOneOffWantV1.MatchString(text) &&
				!strings.Contains(strings.ToLower(text), " instead of ") && !strings.Contains(strings.ToLower(text), " rather than ") {
				continue
			}
			cues = append(cues, rule.Cue)
		}
	}
	return cues
}

func distillCandidateAssistantCuesV1(text string) []distillCandidateCueV1 {
	all := distillCandidateCuesV1(text)
	out := make([]distillCandidateCueV1, 0, len(all))
	for _, cue := range all {
		switch cue {
		case distillCandidateCueDecisionV1, distillCandidateCueClosedNegativeV1, distillCandidateCueInvariantV1, distillCandidateCueGotchaV1:
			out = append(out, cue)
		}
	}
	return out
}

func distillCandidateAssistantSameLocusV1(text string, files []string) bool {
	normalizedText := filepath.ToSlash(text)
	for _, file := range files {
		if distillCandidateContainsPathV1(normalizedText, file) {
			return true
		}
	}
	return false
}

func distillCandidateContainsPathV1(text, path string) bool {
	path = filepath.ToSlash(strings.TrimSpace(path))
	if path == "" {
		return false
	}
	for offset := 0; offset <= len(text)-len(path); {
		index := strings.Index(text[offset:], path)
		if index < 0 {
			return false
		}
		start := offset + index
		end := start + len(path)
		leftOK := start == 0 || !distillCandidatePathByteV1(text[start-1])
		rightOK := end == len(text) || !distillCandidatePathByteV1(text[end])
		if leftOK && rightOK {
			return true
		}
		offset = start + 1
	}
	return false
}

func distillCandidatePathByteV1(value byte) bool {
	return value == '/' || value == '\\' || value == '.' || value == '-' || value == '_' ||
		(value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z') || (value >= '0' && value <= '9')
}

// selectDistillCandidateCardsV1 creates one bounded context card per
// cue-bearing user turn. Assistant text is context, never direct authority; an
// accepted assistant proposal is admitted by the following user's explicit
// acceptance cue. Keeping cards independent prevents a cue-dense transcript
// from transitively becoming one provider-sized mega-card.
func selectDistillCandidateCardsV1(transcript distillNormalizedTranscriptV1) []distillCandidateCardV1 {
	cards, _ := selectDistillCandidateCardsLimitedV1(transcript, 0)
	return cards
}

// selectDistillCandidateCardsLimitedV1 stops during trigger discovery, before
// card/window copies are allocated, when limit is exceeded. limit<=0 is
// unbounded and is reserved for focused tests/local helpers; provider planning
// always supplies the per-session admission cap.
func selectDistillCandidateCardsLimitedV1(transcript distillNormalizedTranscriptV1, limit int) ([]distillCandidateCardV1, bool) {
	type triggerAt struct {
		index    int
		cues     []distillCandidateCueV1
		evidence distillCandidateEvidenceV1
		// turn is set only when one oversized source turn was safely divided at
		// explicit paragraph boundaries. Its source lines remain those of the
		// original JSONL record; only its content-derived identity is narrower.
		turn *distillNormalizedTurnV1
	}
	var triggers []triggerAt
	for index, turn := range transcript.Turns {
		var trigger triggerAt
		switch turn.Role {
		case distillTranscriptRoleUserV1:
			if !turn.DirectUser {
				continue
			}
			cues := distillCandidateCuesV1(turn.Text)
			if len(cues) == 0 && index > 0 && transcript.Turns[index-1].Role == distillTranscriptRoleAssistantV1 && distillCandidateAdjacentAcceptanceV1.MatchString(strings.TrimSpace(turn.Text)) {
				if !distillCandidateBareYesV1.MatchString(strings.TrimSpace(turn.Text)) || !distillCandidateOneOffAssistantOfferV1.MatchString(transcript.Turns[index-1].Text) {
					cues = []distillCandidateCueV1{distillCandidateCueAcceptanceV1}
				}
			}
			if len(cues) == 0 {
				continue
			}
			pieces := splitOversizedDistillCandidateTriggerV1(turn)
			if len(pieces) > 1 {
				for pieceIndex := range pieces {
					piece := pieces[pieceIndex]
					pieceCues := distillCandidateCuesV1(piece.Text)
					triggers = append(triggers, triggerAt{index: index, cues: pieceCues, turn: &piece})
					if limit > 0 && len(triggers) > limit {
						return nil, true
					}
				}
				continue
			}
			trigger = triggerAt{index: index, cues: cues}
		case distillTranscriptRoleAssistantV1:
			if transcript.AgentReview || !transcript.CheckpointSuccess {
				continue
			}
			cues := distillCandidateAssistantCuesV1(turn.Text)
			if len(cues) == 0 || !distillCandidateAssistantSameLocusV1(turn.Text, transcript.FilesTouched) {
				continue
			}
			// Prefer a direct-user card whenever the adjacent human text already
			// carries the authority or explicitly resolves this claim.
			if index > 0 && transcript.Turns[index-1].Role == distillTranscriptRoleUserV1 && transcript.Turns[index-1].DirectUser && len(distillCandidateCuesV1(transcript.Turns[index-1].Text)) > 0 {
				continue
			}
			if index+1 < len(transcript.Turns) && transcript.Turns[index+1].Role == distillTranscriptRoleUserV1 && transcript.Turns[index+1].DirectUser {
				nextCues := distillCandidateCuesV1(transcript.Turns[index+1].Text)
				if len(nextCues) == 0 && distillCandidateAdjacentAcceptanceV1.MatchString(strings.TrimSpace(transcript.Turns[index+1].Text)) {
					if !distillCandidateBareYesV1.MatchString(strings.TrimSpace(transcript.Turns[index+1].Text)) || !distillCandidateOneOffAssistantOfferV1.MatchString(turn.Text) {
						nextCues = []distillCandidateCueV1{distillCandidateCueAcceptanceV1}
					}
				}
				if distillCandidateCueSliceContainsV1(nextCues, distillCandidateCueAcceptanceV1) || distillCandidateCueSliceContainsV1(nextCues, distillCandidateCueCorrectionV1) {
					continue
				}
			}
			trigger = triggerAt{
				index: index,
				cues:  cues,
				evidence: distillCandidateEvidenceV1{
					CheckpointSuccess: true,
					SameLocus:         true,
				},
			}
		default:
			continue
		}
		triggers = append(triggers, trigger)
		if limit > 0 && len(triggers) > limit {
			return nil, true
		}
	}
	if len(triggers) == 0 {
		return nil, false
	}

	cards := make([]distillCandidateCardV1, 0, len(triggers))
	for _, trigger := range triggers {
		start, end := trigger.index, trigger.index
		triggerRole := transcript.Turns[trigger.index].Role
		if triggerRole == distillTranscriptRoleUserV1 {
			if trigger.index > 0 && transcript.Turns[trigger.index-1].Role == distillTranscriptRoleAssistantV1 {
				start--
			}
			if trigger.index+1 < len(transcript.Turns) && transcript.Turns[trigger.index+1].Role == distillTranscriptRoleAssistantV1 {
				end++
			}
		} else {
			if trigger.index > 0 && transcript.Turns[trigger.index-1].Role == distillTranscriptRoleUserV1 {
				start--
			}
			if trigger.index+1 < len(transcript.Turns) && transcript.Turns[trigger.index+1].Role == distillTranscriptRoleUserV1 {
				end++
			}
		}
		turns := append([]distillNormalizedTurnV1(nil), transcript.Turns[start:end+1]...)
		if trigger.turn != nil {
			turns[trigger.index-start] = *trigger.turn
		}
		acceptance := distillCandidateCueSliceContainsV1(trigger.cues, distillCandidateCueAcceptanceV1)
		for index := range turns {
			var cues []distillCandidateCueV1
			if start+index == trigger.index {
				cues = trigger.cues
			}
			redacted := redactText(turns[index].Text)
			if start+index == trigger.index {
				// Never silently remove a second rule/qualifier from the same
				// authoritative turn. Packing shrinks adjacent context first and
				// refuses the card if this complete redacted trigger cannot fit.
				turns[index].Text = redacted
			} else if acceptance && turns[index].Role == distillTranscriptRoleAssistantV1 && start+index == trigger.index-1 {
				turns[index].Text = boundDistillCandidateTextHeadTailV1(redacted, distillCandidateTurnMaxRunes)
			} else {
				turns[index].Text = boundDistillCandidateTextV1(redacted, cues, distillCandidateTurnMaxRunes)
			}
		}
		triggerTurn := transcript.Turns[trigger.index]
		if trigger.turn != nil {
			triggerTurn = *trigger.turn
		}
		cardTriggers := []distillCandidateTriggerV1{{
			TurnID:                triggerTurn.ID,
			Role:                  triggerTurn.Role,
			Cues:                  append([]distillCandidateCueV1(nil), trigger.cues...),
			Authoritative:         triggerTurn.Role == distillTranscriptRoleUserV1,
			CorroborationEligible: triggerTurn.Role == distillTranscriptRoleAssistantV1,
		}}
		startLine, endLine := turns[0].StartLine, turns[0].EndLine
		idParts := []string{distillCandidateSourceIdentityV1(transcript.Source)}
		for _, turn := range turns {
			startLine = min(startLine, turn.StartLine)
			endLine = max(endLine, turn.EndLine)
			idParts = append(idParts, turn.ID)
		}
		for _, trigger := range cardTriggers {
			idParts = append(idParts, trigger.TurnID)
			for _, cue := range trigger.Cues {
				idParts = append(idParts, string(cue))
			}
		}
		if trigger.evidence.CheckpointSuccess {
			idParts = append(idParts, "checkpoint_success")
		}
		if trigger.evidence.SameLocus {
			idParts = append(idParts, "same_locus")
		}
		if trigger.turn != nil {
			// Distinguish paragraph cards while keeping the parent turn ID as the
			// private quality source-map join key.
			idParts = append(idParts, distillCandidateStableIDV1("paragraph-v1:", triggerTurn.ID, triggerTurn.Text))
		}
		cards = append(cards, distillCandidateCardV1{
			SchemaVersion: distillCandidateSchemaVersion,
			ID:            distillCandidateStableIDV1("candidate-v1:", idParts...),
			Source:        transcript.Source,
			StartLine:     startLine,
			EndLine:       endLine,
			Triggers:      cardTriggers,
			Evidence:      trigger.evidence,
			Turns:         turns,
		})
	}
	return cards, false
}

// splitOversizedDistillCandidateTriggerV1 safely narrows a very large direct
// user turn only when every non-empty paragraph independently carries a
// durable cue and fits well below the provider ceiling. That condition avoids
// promoting neutral pasted prose to direct authority and avoids silently
// dropping connective or qualifying paragraphs. An indivisible or ambiguous
// turn is returned unchanged so the existing bounded renderer rejects it.
//
// A JSONL message has one source-line anchor even when its decoded text has
// embedded newlines. Split pieces therefore retain the original line metadata
// and derive their IDs from the parent turn plus complete paragraph text.
func splitOversizedDistillCandidateTriggerV1(turn distillNormalizedTurnV1) []distillNormalizedTurnV1 {
	const maxParagraphBytes = 24 << 10
	redacted := redactText(turn.Text)
	if len(redacted) <= maxParagraphBytes {
		return []distillNormalizedTurnV1{turn}
	}
	paragraphs := regexp.MustCompile(`\n[\t ]*\n+`).Split(redacted, -1)
	if len(paragraphs) < 2 {
		return []distillNormalizedTurnV1{turn}
	}
	pieces := make([]distillNormalizedTurnV1, 0, len(paragraphs))
	for _, paragraph := range paragraphs {
		paragraph = strings.TrimSpace(paragraph)
		if paragraph == "" {
			continue
		}
		if len(paragraph) > maxParagraphBytes || len(distillCandidateCuesV1(paragraph)) == 0 || distillCandidateCrossParagraphReferenceV1.MatchString(paragraph) {
			return []distillNormalizedTurnV1{turn}
		}
		piece := turn
		piece.Text = paragraph
		piece.Origins = append([]distillTranscriptOriginV1(nil), turn.Origins...)
		piece.SourceLines = append([]int(nil), turn.SourceLines...)
		pieces = append(pieces, piece)
	}
	if len(pieces) < 2 {
		return []distillNormalizedTurnV1{turn}
	}
	return pieces
}

func boundDistillCandidateTextV1(text string, cues []distillCandidateCueV1, maxRunes int) string {
	runes := []rune(text)
	if maxRunes <= 0 || len(runes) <= maxRunes {
		return text
	}
	center := 0
	cueSet := make(map[distillCandidateCueV1]bool, len(cues))
	for _, cue := range cues {
		cueSet[cue] = true
	}
	for _, rule := range distillCandidateCueRulesV1 {
		if !cueSet[rule.Cue] {
			continue
		}
		if loc := rule.Pattern.FindStringIndex(text); loc != nil {
			center = len([]rune(text[:loc[0]]))
			break
		}
	}
	before := maxRunes / 3
	start := max(0, center-before)
	if start+maxRunes > len(runes) {
		start = len(runes) - maxRunes
	}
	prefix, suffix := "", ""
	if start > 0 {
		prefix = "…"
	}
	if start+maxRunes < len(runes) {
		suffix = "…"
	}
	return prefix + string(runes[start:start+maxRunes]) + suffix
}

func boundDistillCandidateTextHeadTailV1(text string, maxRunes int) string {
	runes := []rune(text)
	if maxRunes <= 0 || len(runes) <= maxRunes {
		return text
	}
	head := maxRunes / 2
	tail := maxRunes - head
	return string(runes[:head]) + "…" + string(runes[len(runes)-tail:])
}

func distillCandidateCueSliceContainsV1(cues []distillCandidateCueV1, want distillCandidateCueV1) bool {
	for _, cue := range cues {
		if cue == want {
			return true
		}
	}
	return false
}

type renderedDistillCandidateHeaderV1 struct {
	Type          string                      `json:"type"`
	SchemaVersion int                         `json:"schema_version"`
	CandidateID   string                      `json:"candidate_id"`
	Triggers      []distillCandidateTriggerV1 `json:"triggers"`
	Evidence      distillCandidateEvidenceV1  `json:"evidence,omitempty"`
}

type renderedDistillCandidateTurnV1 struct {
	Type            string                  `json:"type"`
	CandidateID     string                  `json:"candidate_id"`
	TurnID          string                  `json:"turn_id"`
	Role            distillTranscriptRoleV1 `json:"role"`
	Authority       string                  `json:"authority"`
	AcceptanceScope string                  `json:"acceptance_scope,omitempty"`
	Text            string                  `json:"text"`
}

func renderDistillCandidateCardV1(card distillCandidateCardV1) string {
	var b strings.Builder
	header, _ := json.Marshal(renderedDistillCandidateHeaderV1{
		Type:          "distill_candidate_v1",
		SchemaVersion: distillCandidateSchemaVersion,
		CandidateID:   card.ID,
		Triggers:      card.Triggers,
		Evidence:      card.Evidence,
	})
	b.Write(header)
	b.WriteByte('\n')
	for index, turn := range card.Turns {
		authority := "context_only"
		if turn.Role == distillTranscriptRoleUserV1 {
			if distillCandidateTurnIsTriggerV1(card, turn.ID) && turn.DirectUser {
				authority = "direct_user"
			} else if !turn.DirectUser {
				authority = "untrusted_user_context"
			}
		}
		acceptanceScope := ""
		if turn.Role == distillTranscriptRoleAssistantV1 {
			authority = "assistant_claim"
			if index+1 < len(card.Turns) && card.Turns[index+1].Role == distillTranscriptRoleUserV1 && distillCandidateTurnIsTriggerWithCueV1(card, card.Turns[index+1].ID, distillCandidateCueAcceptanceV1) {
				acceptanceScope = "decision_only"
			}
		}
		line, _ := json.Marshal(renderedDistillCandidateTurnV1{
			Type:            "distill_candidate_turn_v1",
			CandidateID:     card.ID,
			TurnID:          turn.ID,
			Role:            turn.Role,
			Authority:       authority,
			AcceptanceScope: acceptanceScope,
			Text:            redactText(turn.Text),
		})
		b.Write(line)
		b.WriteByte('\n')
	}
	return b.String()
}

func distillCandidateHasCueV1(text string, cue distillCandidateCueV1) bool {
	for _, got := range distillCandidateCuesV1(text) {
		if got == cue {
			return true
		}
	}
	return false
}

func distillCandidateTurnIsTriggerWithCueV1(card distillCandidateCardV1, turnID string, cue distillCandidateCueV1) bool {
	for _, trigger := range card.Triggers {
		if trigger.TurnID == turnID && distillCandidateCueSliceContainsV1(trigger.Cues, cue) {
			return true
		}
	}
	return false
}

func renderBoundedDistillCandidateCardV1(card distillCandidateCardV1, maxBytes int) (string, error) {
	bounded := card
	bounded.Turns = append([]distillNormalizedTurnV1(nil), card.Turns...)
	for maxRunes := distillCandidateTurnMaxRunes; maxRunes >= 8; maxRunes /= 2 {
		for index := range bounded.Turns {
			if distillCandidateTurnIsTriggerV1(card, bounded.Turns[index].ID) {
				bounded.Turns[index].Text = card.Turns[index].Text
				continue
			}
			var cues []distillCandidateCueV1
			for _, trigger := range card.Triggers {
				if trigger.TurnID == bounded.Turns[index].ID {
					cues = trigger.Cues
					break
				}
			}
			if bounded.Turns[index].Role == distillTranscriptRoleAssistantV1 && distillCandidateCardHasCueV1(card, distillCandidateCueAcceptanceV1) {
				bounded.Turns[index].Text = boundDistillCandidateTextHeadTailV1(card.Turns[index].Text, maxRunes)
			} else {
				bounded.Turns[index].Text = boundDistillCandidateTextV1(card.Turns[index].Text, cues, maxRunes)
			}
		}
		if rendered := renderDistillCandidateCardV1(bounded); len(rendered) <= maxBytes {
			return rendered, nil
		}
	}
	return "", fmt.Errorf("candidate %s cannot fit within %d rendered bytes", card.ID, maxBytes)
}

func distillCandidateTurnIsTriggerV1(card distillCandidateCardV1, turnID string) bool {
	for _, trigger := range card.Triggers {
		if trigger.TurnID == turnID {
			return true
		}
	}
	return false
}

func distillCandidateCardHasCueV1(card distillCandidateCardV1, cue distillCandidateCueV1) bool {
	for _, trigger := range card.Triggers {
		if distillCandidateCueSliceContainsV1(trigger.Cues, cue) {
			return true
		}
	}
	return false
}

// packDistillCandidateCardsV1 deterministically sorts cards and emits exactly
// one bounded card per batch. It never exceeds maxBytes: context is reduced
// around the trigger until the rendered JSON fits, or planning fails closed.
func packDistillCandidateCardsV1(cards []distillCandidateCardV1, maxBytes int) ([]distillCandidateBatchV1, error) {
	if len(cards) == 0 {
		return nil, nil
	}
	if maxBytes <= 0 {
		maxBytes = defaultDistillChunkSize
	}
	if maxBytes < distillCandidateMinChunkBytes {
		return nil, fmt.Errorf("candidate pipeline requires at least %d --max-chunk-bytes", distillCandidateMinChunkBytes)
	}
	ordered := append([]distillCandidateCardV1(nil), cards...)
	sort.SliceStable(ordered, func(i, j int) bool {
		left, right := ordered[i], ordered[j]
		leftSource, rightSource := distillCandidateSourceIdentityV1(left.Source), distillCandidateSourceIdentityV1(right.Source)
		if leftSource != rightSource {
			return leftSource < rightSource
		}
		if left.StartLine != right.StartLine {
			return left.StartLine < right.StartLine
		}
		return left.ID < right.ID
	})

	batches := make([]distillCandidateBatchV1, 0, len(ordered))
	for _, card := range ordered {
		rendered, err := renderBoundedDistillCandidateCardV1(card, maxBytes)
		if err != nil {
			return nil, err
		}
		anchor := distillCandidateAnchorV1{
			CandidateID:  card.ID,
			SessionID:    card.Source.SessionID,
			CheckpointID: card.Source.LatestCheckpoint,
			Branch:       card.Source.Branch,
			Transcript:   card.Source.Transcript,
			StartLine:    card.StartLine,
			EndLine:      card.EndLine,
		}
		triggerLine := distillCandidateTriggerLineV1(card)
		triggerTurnID := distillCandidateTriggerTurnIDV1(card)
		anchorStart, anchorEnd := triggerLine, triggerLine
		if distillCandidateCardHasCueV1(card, distillCandidateCueAcceptanceV1) {
			anchorStart, anchorEnd = card.StartLine, triggerLine
		}
		batches = append(batches, distillCandidateBatchV1{
			CandidateIDs: []string{card.ID},
			Anchors:      []distillCandidateAnchorV1{anchor},
			Chunk: transcriptChunk{
				StartLine:     anchorStart,
				EndLine:       anchorEnd,
				DistillTurnID: triggerTurnID,
				Text:          rendered,
			},
		})
	}
	return batches, nil
}

func distillCandidateTriggerTurnIDV1(card distillCandidateCardV1) string {
	if len(card.Triggers) > 0 {
		return card.Triggers[0].TurnID
	}
	return ""
}

func distillCandidateTriggerLineV1(card distillCandidateCardV1) int {
	for _, trigger := range card.Triggers {
		for _, turn := range card.Turns {
			if turn.ID == trigger.TurnID {
				return turn.StartLine
			}
		}
	}
	return card.StartLine
}

// renderDistillCandidateCardsV1 is the direct adapter for the existing distill
// runner. Call packDistillCandidateCardsV1 instead when candidate IDs/anchors
// are also needed by execution accounting or dry-run output.
func renderDistillCandidateCardsV1(cards []distillCandidateCardV1, maxBytes int) ([]transcriptChunk, error) {
	batches, err := packDistillCandidateCardsV1(cards, maxBytes)
	if err != nil {
		return nil, err
	}
	chunks := make([]transcriptChunk, len(batches))
	for index := range batches {
		chunks[index] = batches[index].Chunk
	}
	return chunks, nil
}

func distillCandidateSourceIdentityV1(source distillCandidateSourceV1) string {
	if source.SessionID != "" {
		return "session:" + source.SessionID
	}
	return "transcript:" + source.Transcript
}

func distillCandidateStableIDV1(prefix string, parts ...string) string {
	material := strings.Join(append([]string{prefix}, parts...), "\x00")
	sum := sha256.Sum256([]byte(material))
	return prefix + hex.EncodeToString(sum[:])
}
