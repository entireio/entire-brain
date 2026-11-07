package cli

import (
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/entireio/cli/transcript"
	clibase "github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
	patterncore "github.com/entireio/entire-brain/internal/cli/patterns"
)

// Pattern corpus enrichment (V2, Phase 2): turn episodes into brain events by
// linking the operational evidence — command exit/failure, files touched,
// way-of-working meta hits, and durable-fact links — so Phase 3 candidates can be
// corroborated across intent + method + evidence + outcome. All deterministic and
// token-free; missing optional sources (facts, semantic) degrade to empty.

type corpusCommand struct {
	head     string
	raw      string
	line     int
	callID   string
	exitCode *int
	failed   bool
}

type corpusFileRef struct {
	path   string
	action string // read | write | edit | add | delete
	line   int
}

var (
	reExitCode  = regexp.MustCompile(`(?i)(?:exited with code|exit code:?)\s*(\d+)`)
	rePatchFile = regexp.MustCompile(`(?m)^\*\*\* (Add|Update|Delete) File: (.+)$`)
	// git commit confirmation: "[branch hash] subject" — local evidence only.
	reCommitConfirm = regexp.MustCompile(`\[([A-Za-z0-9._/+-]+) ([0-9a-f]{7,40})\] (.+)`)
)

type corpusCommit struct {
	hash, branch, subject string
	line                  int
}

// corpusCommits extracts git commits an episode landed from the local
// `[branch hash] subject` confirmation lines in its work text. No network; the
// subject is redacted and truncated.
func corpusCommits(workText string) []corpusCommit {
	seen := map[string]bool{}
	var out []corpusCommit
	for i, line := range strings.Split(workText, "\n") {
		m := reCommitConfirm.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		hash := m[2]
		if seen[hash] {
			continue
		}
		seen[hash] = true
		out = append(out, corpusCommit{
			hash:    hash,
			branch:  m[1],
			subject: clibase.RedactText(clibase.TruncateString(strings.TrimSpace(m[3]), 160)),
			line:    i + 1,
		})
	}
	return out
}

// claudeFileTools maps Claude/pi file tool names to a file action.
var claudeFileTools = map[string]string{
	"read": "read", "view": "read", "cat": "read",
	"write": "write", "create": "write",
	"edit": "edit", "str_replace": "edit", "str_replace_editor": "edit", "apply_patch": "edit",
}

// corpusEpisodeOps extracts ordered tool calls, shell commands (with exit/failure
// correlated by call id), and file references from one episode's work segment.
func corpusEpisodeOps(workText string) (tools []corpusToolOp, commands []corpusCommand, files []corpusFileRef) {
	exitByCall := map[string]int{}
	for i, line := range strings.Split(workText, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		record, ok := decodeTranscriptRecord(line)
		if !ok {
			continue
		}
		ln := i + 1
		var uses []toolUse
		if record.etf != nil {
			if !record.etfIs(transcript.TypeAssistant) {
				continue
			}
			uses = etfToolUses(record.etf)
			if strings.EqualFold(record.etf.Agent, "codex") {
				compactCodexExitCodes(uses, exitByCall)
			}
		}
		obj := record.obj // nil for a format line, which matches no case below
		switch jsonString(obj["type"]) {
		case "response_item":
			p := jsonMap(obj["payload"])
			switch jsonString(p["type"]) {
			case "function_call":
				name := normalizeToolName(jsonString(p["name"]))
				tools = append(tools, corpusToolOp{name: name, line: ln})
				if cmd := shellCommandFromInput(name, p["arguments"]); cmd != "" {
					if h := normalizeCommand(cmd); h != "" {
						commands = append(commands, corpusCommand{head: h, raw: cmd, line: ln, callID: jsonString(p["call_id"])})
					}
				}
			case "custom_tool_call":
				name := normalizeToolName(jsonString(p["name"]))
				tools = append(tools, corpusToolOp{name: name, line: ln})
				files = append(files, patchFiles(jsonString(p["input"]), ln)...)
			case "function_call_output", "custom_tool_call_output":
				if id := jsonString(p["call_id"]); id != "" {
					if code, ok := parseExitCode(jsonString(p["output"])); ok {
						exitByCall[id] = code
					}
				}
			case "patch_apply_end":
				if id := jsonString(p["call_id"]); id != "" {
					if code, ok := parseExitCode(jsonString(p["stdout"])); ok {
						exitByCall[id] = code
					}
				}
			}
		case "assistant", "message":
			msg := jsonMap(obj["message"])
			if jsonString(obj["type"]) == "message" && jsonString(msg["role"]) != "assistant" {
				continue
			}
			uses = nativeToolUses(msg["content"])
		}
		t, c, f := toolUseOps(uses, ln)
		tools = append(tools, t...)
		commands = append(commands, c...)
		files = append(files, f...)
	}
	// Correlate exit codes to their commands.
	for i := range commands {
		if commands[i].callID == "" {
			continue
		}
		if code, ok := exitByCall[commands[i].callID]; ok {
			c := code
			commands[i].exitCode = &c
			commands[i].failed = code != 0
		}
	}
	return tools, commands, dedupeFileRefs(files)
}

// toolUseOps returns the tool, command, and file operations of one record's
// tool calls.
func toolUseOps(uses []toolUse, ln int) (tools []corpusToolOp, commands []corpusCommand, files []corpusFileRef) {
	for _, use := range uses {
		if use.name == "" {
			continue
		}
		tools = append(tools, corpusToolOp{name: use.name, line: ln})
		if cmd := shellCommandFromInput(use.name, use.input); cmd != "" {
			if h := normalizeCommand(cmd); h != "" {
				commands = append(commands, corpusCommand{head: h, raw: cmd, line: ln, callID: use.id})
			}
		}
		if action, isFile := claudeFileTools[use.name]; isFile {
			if p := filePathFromInput(use.input); p != "" {
				files = append(files, corpusFileRef{path: p, action: action, line: ln})
			} else if use.name == "apply_patch" {
				// Compact codex has no path: the patch text is the input,
				// read the way the native custom_tool_call case reads it.
				files = append(files, patchFiles(patchInputText(use.input), ln)...)
			}
		}
	}
	return tools, commands, files
}

// patchInputText returns the patch text of an apply_patch call that names no
// path. The CLI wraps a custom_tool_call's text input as {"input": <patch>}
// (as an object, or JSON-encoded in a string); any other string input is read
// as the patch itself, so a converter that stops wrapping it still yields the
// patch's files.
func patchInputText(input any) string {
	if m := asObject(input); m != nil {
		return jsonString(m["input"])
	}
	text, _ := input.(string)
	return text
}

func patchFiles(patch string, ln int) []corpusFileRef {
	var out []corpusFileRef
	for _, m := range rePatchFile.FindAllStringSubmatch(patch, -1) {
		action := map[string]string{"Add": "add", "Update": "edit", "Delete": "delete"}[m[1]]
		out = append(out, corpusFileRef{path: strings.TrimSpace(m[2]), action: action, line: ln})
	}
	return out
}

func filePathFromInput(raw any) string {
	m := asObject(raw)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(firstNonEmptyString(m["file_path"], m["path"], m["filename"]))
}

// parseExitCode reads the exit code from a command result's text. Codex's
// command wrapper writes a header (exit code, wall time) and then an "Output:"
// line that starts the command's own output; when that line is there, only the
// header is searched, so exit-like text the command printed cannot shadow the
// wrapper's own line, or stand in for one the header lacks (a command still
// running reports a session ID, not an exit code).
func parseExitCode(output string) (int, bool) {
	m := reExitCode.FindStringSubmatch(codexWrapperHeader(output))
	if m == nil {
		return 0, false
	}
	n := 0
	for _, c := range m[1] {
		n = n*10 + int(c-'0')
	}
	return n, true
}

// codexWrapperHeader returns the text before the first line that is exactly
// "Output:" (surrounding spaces aside), or all of text when there is none.
func codexWrapperHeader(text string) string {
	for start := 0; start < len(text); {
		line := text[start:]
		end := strings.IndexByte(line, '\n')
		if end >= 0 {
			line = line[:end]
		}
		if strings.TrimSpace(line) == "Output:" {
			return text[:start]
		}
		if end < 0 {
			break
		}
		start += end + 1
	}
	return text
}

// compactCodexExitCodes records the exit codes a compact v1 codex record
// carries in its tool calls' inlined result output, keyed by call id for the
// same correlation the native function_call_output case feeds. For a rollout
// of an older Codex (the cli's response_item path) every result is status
// "success" and the exit code is only in the wrapper text Codex puts around
// the command's output. A current Codex rollout (the cli's item path) carries
// the command's own output with no wrapper and marks a failure only as status
// "error", which, like other agents' status "error", is not read here: it is
// not the same as a non-zero exit and has no exit code. Such a call's output is
// skipped, so exit text the command printed (a test that logs "exited with
// code 1", say) is not taken for its exit code.
func compactCodexExitCodes(uses []toolUse, exitByCall map[string]int) {
	for _, use := range uses {
		if use.id == "" || isCodexItemCommand(use.input) {
			continue
		}
		if code, ok := parseExitCode(use.output); ok {
			exitByCall[use.id] = code
		}
	}
}

// isCodexItemCommand reports whether a compact codex tool input is the item
// path's command input, {"command": <script>}, keyed on that shape rather than
// on the tool's name. Codex's own tools, which the response_item path keeps,
// give a command as "cmd" (exec_command) or as an argv array (shell), never as
// a "command" string.
func isCodexItemCommand(input any) bool {
	_, ok := jsonMap(input)["command"].(string)
	return ok
}

func dedupeFileRefs(in []corpusFileRef) []corpusFileRef {
	seen := map[string]bool{}
	var out []corpusFileRef
	for _, f := range in {
		if f.path == "" {
			continue
		}
		key := f.path + "\x00" + f.action
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, f)
	}
	return out
}

// --- way-of-working meta hits ---

type corpusMetaHit struct {
	metaID string
	quote  string
	line   int
}

// metaCueSet maps a meta_id to high-precision intent cues.
var metaCueSet = []struct {
	id   string
	cues []string
}{
	{"review_request", []string{"review the", "review this", "code review", "review the changes", "review the codebase"}},
	{"handoff_resume", []string{"continue", "resume", "pick up", "keep going", "carry on", "wrap up"}},
	{"user_preference", []string{"i prefer", "always ", "never ", "please use", "from now on", "going forward"}},
	{"no_egress_constraint", []string{"no egress", "no-egress", "offline", "local only", "local-only", "do not send", "without network"}},
	{"correction_followup", correctionCues},
}

var readOnlyCues = []string{"how does", "why does", "what happens", "investigate", "understand how", "explain", "where is", "what is", "did you", "analyze"}

// classifyMetaHits returns deterministic way-of-working hits for an episode from
// its intent, the work segment, and whether it ran commands.
func classifyMetaHits(intent, workText string, nCmds int, validationCmd bool) []corpusMetaHit {
	li := strings.ToLower(intent)
	var hits []corpusMetaHit
	add := func(id, quote string) {
		hits = append(hits, corpusMetaHit{metaID: id, quote: clibase.RedactText(clibase.TruncateString(quote, 160))})
	}
	for _, set := range metaCueSet {
		for _, cue := range set.cues {
			if strings.Contains(li, cue) {
				add(set.id, intent)
				break
			}
		}
	}
	if validationCmd {
		add("validation_loop", intent)
	}
	if nCmds == 0 {
		for _, cue := range readOnlyCues {
			if strings.Contains(li, cue) {
				add("read_only_diagnosis", intent)
				break
			}
		}
	}
	// Dedupe by meta id.
	seen := map[string]bool{}
	var out []corpusMetaHit
	for _, h := range hits {
		if seen[h.metaID] {
			continue
		}
		seen[h.metaID] = true
		out = append(out, h)
	}
	return out
}

func episodeHasValidationCommand(commands []corpusCommand) bool {
	for _, c := range commands {
		if patterncore.ValidationCommandHeads[c.head] {
			return true
		}
	}
	return false
}

// --- durable fact linking (branch-scoped, graceful when absent) ---

type corpusFactLink struct {
	factID string
	branch string
	kind   string
	paths  string
	weight float64
}

// linkEpisodeFacts links an episode to active durable facts on its branch whose
// text/locus overlaps the episode's intent terms and file basenames. Capped;
// branch-scoped so facts never leak across branches.
func linkEpisodeFacts(facts []factRecord, intent string, files []corpusFileRef) []corpusFactLink {
	if len(facts) == 0 {
		return nil
	}
	terms := map[string]bool{}
	for _, t := range brainBriefFileMatchTerms(intent) {
		terms[t] = true
	}
	for _, f := range files {
		base := strings.ToLower(path.Base(f.path))
		base = strings.TrimSuffix(base, path.Ext(base))
		if len(base) >= 3 {
			terms[base] = true
		}
	}
	if len(terms) == 0 {
		return nil
	}
	type scored struct {
		f       factRecord
		overlap int
	}
	var matched []scored
	for _, f := range facts {
		if f.Status != "active" {
			continue
		}
		overlap := 0
		seen := map[string]bool{}
		for _, w := range brainBriefTaskWordPattern.FindAllString(strings.ToLower(f.Text), -1) {
			if terms[w] && !seen[w] {
				seen[w] = true
				overlap++
			}
		}
		for _, l := range f.Locus {
			if terms[strings.ToLower(l)] {
				overlap += 2 // locus match is stronger evidence
			}
		}
		if overlap > 0 {
			matched = append(matched, scored{f, overlap})
		}
	}
	sort.SliceStable(matched, func(i, j int) bool { return matched[i].overlap > matched[j].overlap })
	var out []corpusFactLink
	for _, m := range matched {
		if len(out) >= 5 {
			break
		}
		out = append(out, corpusFactLink{factID: m.f.ID, branch: m.f.Branch, kind: factstore.KindOrInferred(m.f), paths: strings.Join(m.f.Paths, ","), weight: float64(m.overlap)})
	}
	return out
}
