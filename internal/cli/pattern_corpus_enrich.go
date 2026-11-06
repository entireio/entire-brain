package cli

import (
	"encoding/json"
	"path"
	"regexp"
	"sort"
	"strings"

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
// repoRoot is the session's checkout root from codexSessionRoot, or "" when
// unknown; a compact codex FileChange's absolute path under it is stored
// relative to it.
func corpusEpisodeOps(workText, repoRoot string) (tools []corpusToolOp, commands []corpusCommand, files []corpusFileRef) {
	exitByCall := map[string]int{}
	failedByCall := map[string]bool{}
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
			content := msg["content"]
			if len(msg) == 0 {
				// ETF compact records carry the content blocks at the record's
				// top level (see distillConversationText).
				content = obj["content"]
				if strings.EqualFold(jsonString(obj["agent"]), "codex") {
					compactCodexResults(content, exitByCall, failedByCall)
				}
			}
			t, c, f := claudeContentOps(content, repoRoot, ln)
			tools = append(tools, t...)
			commands = append(commands, c...)
			files = append(files, f...)
		}
	}
	// Correlate exit codes, and failures that carry none, to their commands.
	for i := range commands {
		if commands[i].callID == "" {
			continue
		}
		if code, ok := exitByCall[commands[i].callID]; ok {
			c := code
			commands[i].exitCode = &c
			commands[i].failed = code != 0
		} else if failedByCall[commands[i].callID] {
			commands[i].failed = true
		}
	}
	return tools, commands, dedupeFileRefs(files)
}

func claudeContentOps(content any, repoRoot string, ln int) (tools []corpusToolOp, commands []corpusCommand, files []corpusFileRef) {
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
				} else if name == "apply_patch" {
					// Compact codex has no path: the converter wraps the
					// custom_tool_call patch text as {"input": <patch>}, so read
					// it the way the native custom_tool_call case does.
					files = append(files, patchFiles(jsonString(asObject(bm["input"])["input"]), ln)...)
				}
			}
			if name == "filechange" {
				files = append(files, fileChangeFiles(bm["input"], repoRoot, ln)...)
			}
		}
	}
	return tools, commands, files
}

// fileChangeFiles reads the file refs of a compact codex FileChange, the tool
// the cli's item path (a current Codex rollout) writes for an edit:
// {"changes":[{"path","kind","move_path","diff"}]}. kind is add, delete or
// update; an update with a move_path deletes path and adds move_path. Like an
// apply_patch, every change counts whatever its result status.
func fileChangeFiles(input any, repoRoot string, ln int) []corpusFileRef {
	changes, _ := asObject(input)["changes"].([]any)
	var out []corpusFileRef
	for _, c := range changes {
		cm := jsonMap(c)
		p := repoRelativePath(jsonString(cm["path"]), repoRoot)
		if p == "" {
			continue
		}
		kind := strings.ToLower(strings.TrimSpace(jsonString(cm["kind"])))
		if move := repoRelativePath(jsonString(cm["move_path"]), repoRoot); move != "" && kind == "update" {
			out = append(out, corpusFileRef{path: p, action: "delete", line: ln}, corpusFileRef{path: move, action: "add", line: ln})
			continue
		}
		action := "edit"
		if kind == "add" || kind == "delete" {
			action = kind
		}
		out = append(out, corpusFileRef{path: p, action: action, line: ln})
	}
	return out
}

// repoRelativePath is p relative to repoRoot (a clean forward-slash path from
// codexSessionRoot) when p is inside it, so it matches the repo-relative paths
// apply_patch and the semantic index use. Dot segments are resolved first, so
// root/dir/../main.go is main.go and root/../x.go is outside. Any other path
// is returned trimmed but otherwise as written.
func repoRelativePath(p, repoRoot string) string {
	p = strings.TrimSpace(p)
	if repoRoot == "" {
		return p
	}
	if s, ok := cleanAbsPath(p); ok {
		if rel, ok := pathUnderRoot(s, repoRoot); ok {
			return rel
		}
	}
	return p
}

// codexSessionRoot is the checkout a compact codex session's FileChange paths
// are under, worked out from the session itself: a FileChange path is
// absolute, and the session's checkpoint records the repo-relative files it
// touched (files_touched), so an absolute path that ends in one of them gives
// a candidate root.
//
// Every candidate is scored against all of the session's distinct paths: the
// root under which the most of them are touched files wins, then the root
// that holds the most of them. Scoring every candidate, not one per path,
// handles a checkout named like one of its own directories (/work/repo with
// repo/main.go), and counting distinct paths keeps repeated edits of one file
// from outvoting the rest. A tie that is still left is ambiguous, so it gives
// "" and the paths are stored as written rather than linked to the wrong
// files. "" also when the session has no FileChange or no path ends in a
// touched file.
//
// The live checkout the corpus is built from is not used: worktrees of a repo
// share one corpus, and an unchanged session is not re-indexed, so the paths
// it stores must not depend on which checkout, subdirectory or symlinked
// spelling happened to build it first.
func codexSessionRoot(transcript string, filesTouched []string) string {
	if len(filesTouched) == 0 || !strings.Contains(transcript, `"FileChange"`) {
		return ""
	}
	touched := map[string]bool{}
	for _, f := range filesTouched {
		f = path.Clean(slashPath(f))
		if f != "." && !isAbsSlashPath(f) && f != ".." && !strings.HasPrefix(f, "../") {
			touched[f] = true
		}
	}
	paths := codexFileChangePaths(transcript)
	candidates := map[string]bool{}
	for _, p := range paths {
		for t := range touched {
			if strings.HasSuffix(p, "/"+t) && len(p) > len(t)+1 {
				candidates[p[:len(p)-len(t)-1]] = true
			}
		}
	}
	best, bestTouched, bestInside, tied := "", 0, 0, false
	for root := range candidates {
		nTouched, nInside := 0, 0
		for _, p := range paths {
			if rel, ok := pathUnderRoot(p, root); ok {
				nInside++
				if touched[rel] {
					nTouched++
				}
			}
		}
		switch {
		case nTouched > bestTouched || (nTouched == bestTouched && nInside > bestInside):
			best, bestTouched, bestInside, tied = root, nTouched, nInside, false
		case nTouched == bestTouched && nInside == bestInside:
			tied = true
		}
	}
	if tied {
		return ""
	}
	return best
}

// codexFileChangePaths is every distinct absolute path (path and move_path)
// in a compact codex transcript's FileChange blocks, cleaned by cleanAbsPath.
func codexFileChangePaths(transcript string) []string {
	seen := map[string]bool{}
	var out []string
	for _, line := range strings.Split(transcript, "\n") {
		if !strings.Contains(line, `"FileChange"`) {
			continue
		}
		var obj map[string]any
		if json.Unmarshal([]byte(strings.TrimSpace(line)), &obj) != nil {
			continue
		}
		blocks, _ := obj["content"].([]any)
		for _, b := range blocks {
			bm := jsonMap(b)
			if jsonString(bm["name"]) != "FileChange" {
				continue
			}
			changes, _ := asObject(bm["input"])["changes"].([]any)
			for _, c := range changes {
				cm := jsonMap(c)
				for _, p := range []string{jsonString(cm["path"]), jsonString(cm["move_path"])} {
					if s, ok := cleanAbsPath(p); ok && !seen[s] {
						seen[s] = true
						out = append(out, s)
					}
				}
			}
		}
	}
	return out
}

// cleanAbsPath is p as a slashPath with its dot segments resolved, when it is
// absolute. A path that is not absolute, or stops being so once cleaned, is
// not.
func cleanAbsPath(p string) (string, bool) {
	s := slashPath(p)
	if !isAbsSlashPath(s) {
		return "", false
	}
	s = path.Clean(s)
	return s, isAbsSlashPath(s)
}

// pathUnderRoot is p relative to root when p is strictly inside it. Both are
// clean slash paths, so a root that is only a string prefix (/work/repo2 for
// /work/repo) is not a parent.
func pathUnderRoot(p, root string) (string, bool) {
	if strings.HasPrefix(p, root+"/") && len(p) > len(root)+1 {
		return p[len(root)+1:], true
	}
	return "", false
}

// slashPath is p trimmed and with backslashes as slashes, so a Windows
// session's paths compare with the forward-slash paths a checkpoint records,
// whichever OS builds the corpus.
func slashPath(p string) string {
	return strings.ReplaceAll(strings.TrimSpace(p), `\`, "/")
}

// isAbsSlashPath reports whether a slashPath is absolute on any OS: rooted
// ("/work/repo", "//server/share") or with a drive letter ("C:/work/repo").
func isAbsSlashPath(p string) bool {
	if strings.HasPrefix(p, "/") {
		return true
	}
	return len(p) >= 3 && p[1] == ':' && p[2] == '/' && ((p[0] >= 'a' && p[0] <= 'z') || (p[0] >= 'A' && p[0] <= 'Z'))
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

// compactCodexResults records what a compact v1 codex record's tool_use blocks
// say about how their commands ended, keyed by call id for the same
// correlation the native function_call_output case feeds.
//
// For a rollout of an older Codex (the cli's response_item path) every result
// is status "success" and the exit code is only in the inlined output text,
// which goes into exitByCall. A current Codex rollout (the cli's item path)
// names every command "Bash", inlines the command's own output with no exit
// text, and marks a failure only as status "error"; the exit code is not
// carried over. So a Bash block's output is not read for exit text (a test
// that logs "exited with code 1" is not the command's exit code), and its
// status "error" goes into failedByCall: the command failed, with no exit code
// to store. The cli also writes "error" for a command the user declined.
func compactCodexResults(content any, exitByCall map[string]int, failedByCall map[string]bool) {
	blocks, _ := content.([]any)
	for _, b := range blocks {
		bm := jsonMap(b)
		if t := jsonString(bm["type"]); t != "tool_use" && t != "tool" {
			continue
		}
		id := jsonString(bm["id"])
		if id == "" {
			continue
		}
		result := jsonMap(bm["result"])
		if jsonString(bm["name"]) == "Bash" {
			if jsonString(result["status"]) == "error" {
				failedByCall[id] = true
			}
			continue
		}
		if code, ok := parseExitCode(jsonString(result["output"])); ok {
			exitByCall[id] = code
		}
	}
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
