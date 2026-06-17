package cli

import (
	"encoding/json"
	"path"
	"regexp"
	"sort"
	"strings"
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
			subject: redactText(truncateString(strings.TrimSpace(m[3]), 160)),
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
		var obj map[string]any
		if json.Unmarshal([]byte(line), &obj) != nil {
			continue
		}
		ln := i + 1
		switch jsonString(obj["type"]) {
		case "response_item":
			p := jsonMap(obj["payload"])
			switch jsonString(p["type"]) {
			case "function_call":
				name := strings.ToLower(strings.TrimSpace(jsonString(p["name"])))
				tools = append(tools, corpusToolOp{name: name, line: ln})
				if cmd := shellCommandFromInput(name, p["arguments"]); cmd != "" {
					if h := normalizeCommand(cmd); h != "" {
						commands = append(commands, corpusCommand{head: h, raw: cmd, line: ln, callID: jsonString(p["call_id"])})
					}
				}
			case "custom_tool_call":
				name := strings.ToLower(strings.TrimSpace(jsonString(p["name"])))
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
			t, c, f := claudeContentOps(msg["content"], ln)
			tools = append(tools, t...)
			commands = append(commands, c...)
			files = append(files, f...)
		}
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

func claudeContentOps(content any, ln int) (tools []corpusToolOp, commands []corpusCommand, files []corpusFileRef) {
	blocks, ok := content.([]any)
	if !ok {
		return nil, nil, nil
	}
	for _, b := range blocks {
		bm := jsonMap(b)
		switch jsonString(bm["type"]) {
		case "tool_use", "tool":
			name := strings.ToLower(strings.TrimSpace(firstNonEmptyString(bm["name"], bm["tool"])))
			if name == "" {
				continue
			}
			tools = append(tools, corpusToolOp{name: name, line: ln})
			if cmd := shellCommandFromInput(name, bm["input"]); cmd != "" {
				if h := normalizeCommand(cmd); h != "" {
					commands = append(commands, corpusCommand{head: h, raw: cmd, line: ln, callID: jsonString(bm["id"])})
				}
			}
			if action, isFile := claudeFileTools[name]; isFile {
				if p := filePathFromInput(bm["input"]); p != "" {
					files = append(files, corpusFileRef{path: p, action: action, line: ln})
				}
			}
		}
	}
	return tools, commands, files
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

func parseExitCode(output string) (int, bool) {
	m := reExitCode.FindStringSubmatch(output)
	if m == nil {
		return 0, false
	}
	n := 0
	for _, c := range m[1] {
		n = n*10 + int(c-'0')
	}
	return n, true
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
		hits = append(hits, corpusMetaHit{metaID: id, quote: redactText(truncateString(quote, 160))})
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

var validationCommandHeads = map[string]bool{
	"go test": true, "go vet": true, "mise test": true, "mise run": true, "npm test": true,
	"pnpm test": true, "yarn test": true, "cargo test": true, "pytest": true, "gofmt": true,
	"make test": true, "make check": true,
}

func episodeHasValidationCommand(commands []corpusCommand) bool {
	for _, c := range commands {
		if validationCommandHeads[c.head] {
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
		out = append(out, corpusFactLink{factID: m.f.ID, branch: m.f.Branch, kind: factKindOrInferred(m.f), paths: strings.Join(m.f.Paths, ","), weight: float64(m.overlap)})
	}
	return out
}
