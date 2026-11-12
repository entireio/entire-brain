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
		record, ok := decodeTranscriptRecord(line)
		if !ok {
			continue
		}
		ln := i + 1
		uses := recordToolUses(record)
		if record.etf != nil && strings.EqualFold(record.etf.Agent, "codex") {
			compactCodexResults(uses, exitByCall, failedByCall)
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
		}
		t, c, f := toolUseOps(uses, repoRoot, ln)
		tools = append(tools, t...)
		commands = append(commands, c...)
		files = append(files, f...)
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

// recordToolUses returns the tool calls an assistant transcript line makes: a
// format line's tool_use blocks, or the tool_use and tool blocks of a native
// Claude Code or pi assistant message. Any other line makes none. The episode
// ops and the FileChange root (codexFileChangePaths) both read a session's
// tool calls through it, so they agree on which blocks are edits.
func recordToolUses(record transcriptRecord) []toolUse {
	if record.etf != nil {
		if !record.etfIs(transcript.TypeAssistant) {
			return nil
		}
		return etfToolUses(record.etf)
	}
	switch recordType := jsonString(record.obj["type"]); recordType {
	case "assistant", "message":
		msg := jsonMap(record.obj["message"])
		if recordType == "message" && jsonString(msg["role"]) != "assistant" {
			return nil
		}
		return nativeToolUses(msg["content"])
	}
	return nil
}

// toolUseOps returns the tool, command, and file operations of one record's
// tool calls. repoRoot is the session's checkout root for compact codex
// FileChange paths, or "" when unknown.
func toolUseOps(uses []toolUse, repoRoot string, ln int) (tools []corpusToolOp, commands []corpusCommand, files []corpusFileRef) {
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
		if use.name == fileChangeToolName {
			files = append(files, fileChangeFiles(use.input, repoRoot, ln)...)
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

// fileChangeToolName is a compact codex FileChange's tool name as toolUse
// carries it (lowercased).
const fileChangeToolName = "filechange"

// fileChangeNamePattern finds a FileChange tool name spelled out in any case.
var fileChangeNamePattern = regexp.MustCompile(`(?i)filechange`)

// mayHoldFileChange reports whether undecoded JSON text can hold a FileChange
// tool name once it is decoded and lowercased as toolUse names are
// (normalizeToolName). JSON writes a letter as itself or as a \u escape, and
// the only other rune strings.ToLower turns into one of the name's letters is
// U+0130, which it lowers to "i". So text with neither holds the name only
// where fileChangeNamePattern finds it, and that text is all a reader may skip
// before decoding without missing a FileChange the episode ops would read.
func mayHoldFileChange(text string) bool {
	return fileChangeNamePattern.MatchString(text) || strings.Contains(text, `\u`) || strings.ContainsRune(text, '\u0130')
}

// fileChangeRefs reads the file refs of a compact codex FileChange, the tool
// the cli's item path (a current Codex rollout) writes for an edit:
// {"changes":[{"path","kind","move_path","diff"}]}. kind is add, delete or
// update; an update with a move_path deletes path and adds move_path. Like an
// apply_patch, every change counts whatever its result status. Paths are
// trimmed but otherwise as written: fileChangeFiles makes them repo-relative,
// and codexFileChangePaths infers the session's root from these same paths.
func fileChangeRefs(input any, ln int) []corpusFileRef {
	changes, _ := asObject(input)["changes"].([]any)
	var out []corpusFileRef
	for _, c := range changes {
		cm := jsonMap(c)
		p := strings.TrimSpace(jsonString(cm["path"]))
		kind := strings.ToLower(strings.TrimSpace(jsonString(cm["kind"])))
		if move := strings.TrimSpace(jsonString(cm["move_path"])); move != "" && kind == "update" {
			if p != "" {
				out = append(out, corpusFileRef{path: p, action: "delete", line: ln})
			}
			out = append(out, corpusFileRef{path: move, action: "add", line: ln})
			continue
		}
		if p == "" {
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

// fileChangeFiles is fileChangeRefs with each path under repoRoot made
// relative to it.
func fileChangeFiles(input any, repoRoot string, ln int) []corpusFileRef {
	refs := fileChangeRefs(input, ln)
	for i := range refs {
		refs[i].path = repoRelativePath(refs[i].path, repoRoot)
	}
	return refs
}

// repoRelativePath is p relative to repoRoot (a clean forward-slash path from
// codexSessionRoot) when p is inside it, so it matches the repo-relative paths
// apply_patch and the semantic index use. Any other path, including one with
// a ".." segment (see cleanAbsPath), is returned trimmed but otherwise as
// written.
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
// touched (files_touched), so a path that ends in one of them, split at a
// "/", gives a candidate root.
//
// A candidate scores one for each distinct session path that is a touched
// file under it, and the highest score wins. Every split of every path counts,
// so a checkout named like one of its own directories (/work/repo with
// repo/main.go) still gets its full score, and repeated edits of one file
// count once. A tie is ambiguous, whatever else the tied roots hold, so it
// gives "" and the paths are stored as written rather than linked to the
// wrong files. So does a session with any ".." path (see
// codexFileChangePaths), and one with no FileChange or no path that ends in
// a touched file.
//
// The live checkout the corpus is built from is not used: worktrees of a repo
// share one corpus, and an unchanged session is not re-indexed, so the paths
// it stores must not depend on which checkout, subdirectory or symlinked
// spelling happened to build it first.
func codexSessionRoot(transcript string, filesTouched []string) string {
	if len(filesTouched) == 0 || !mayHoldFileChange(transcript) {
		return ""
	}
	touched := map[string]bool{}
	for _, f := range filesTouched {
		f = path.Clean(slashPath(f))
		if f != "." && !isAbsSlashPath(f) && f != ".." && !strings.HasPrefix(f, "../") {
			touched[f] = true
		}
	}
	paths, resolved := codexFileChangePaths(transcript)
	if !resolved {
		return ""
	}
	score := map[string]int{}
	for _, p := range paths {
		for i := 1; i < len(p)-1; i++ {
			if p[i] == '/' && touched[p[i+1:]] {
				score[p[:i]]++
			}
		}
	}
	best, bestScore, tied := "", 0, false
	for root, n := range score {
		switch {
		case n > bestScore:
			best, bestScore, tied = root, n, false
		case n == bestScore:
			tied = true
		}
	}
	if tied {
		return ""
	}
	return best
}

// codexFileChangePaths is every distinct absolute path of the file refs a
// compact codex transcript's FileChange calls give (fileChangeRefs, read
// through recordToolUses as the episode ops read them), cleaned by
// cleanAbsPath. resolved is false when any of them has a ".." segment: where
// that path really is cannot be known here, and leaving it out could hand the
// root to whichever paths are left.
func codexFileChangePaths(transcript string) (paths []string, resolved bool) {
	seen := map[string]bool{}
	var out []string
	resolved = true
	for _, line := range strings.Split(transcript, "\n") {
		if !mayHoldFileChange(line) {
			continue
		}
		record, ok := decodeTranscriptRecord(line)
		if !ok {
			continue
		}
		for _, use := range recordToolUses(record) {
			if use.name != fileChangeToolName {
				continue
			}
			for _, ref := range fileChangeRefs(use.input, 0) {
				if hasDotDotSegment(slashPath(ref.path)) {
					resolved = false
				}
				if s, ok := cleanAbsPath(ref.path); ok && !seen[s] {
					seen[s] = true
					out = append(out, s)
				}
			}
		}
	}
	return out, resolved
}

// cleanAbsPath is p as a slashPath with "." segments and doubled slashes
// removed, when it is absolute. A path with a ".." segment is refused: the OS
// resolves ".." after symlinks, so root/link/../x.go need not be root/x.go,
// and the checkout it was written in may not be here to look at.
func cleanAbsPath(p string) (string, bool) {
	s := slashPath(p)
	if !isAbsSlashPath(s) || hasDotDotSegment(s) {
		return "", false
	}
	return path.Clean(s), true
}

// hasDotDotSegment reports whether a slashPath has a ".." segment.
func hasDotDotSegment(p string) bool {
	return strings.Contains("/"+p+"/", "/../")
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

// compactCodexResults records what a compact v1 codex record's tool calls say
// about how their commands ended, keyed by call id for the same correlation
// the native function_call_output case feeds.
//
// For a rollout of an older Codex (the cli's response_item path) every result
// is status "success" and the exit code is only in the wrapper text Codex puts
// around the command's output, which goes into exitByCall. A current Codex
// rollout (the cli's item path) carries the command's own output with no
// wrapper and marks a failure only as status "error"; the exit code is not
// carried over. Such a call's output is not read for exit text (a test that
// logs "exited with code 1" is not the command's exit code), and its status
// "error" goes into failedByCall: the command failed, with no exit code to
// store. The cli also writes "error" for a command the user declined.
func compactCodexResults(uses []toolUse, exitByCall map[string]int, failedByCall map[string]bool) {
	for _, use := range uses {
		if use.id == "" {
			continue
		}
		if use.name == codexItemCommandTool {
			if use.status == transcript.ToolStatusError {
				failedByCall[use.id] = true
			}
			continue
		}
		if code, ok := parseExitCode(use.output); ok {
			exitByCall[use.id] = code
		}
	}
}

// codexItemCommandTool is the name the cli's item path gives every command,
// lowercased as toolUse names are. Codex's own tools, which the response_item
// path keeps, never use it. The input can't tell the two paths apart: the item
// path writes {"command": <script>}, but Codex's own shell_command tool also
// takes its command as a "command" string, and its output is the wrapper text
// that carries the exit code.
const codexItemCommandTool = "bash"

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
