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
		// record.obj is nil for a format line, which has no "response_item".
		if obj := record.obj; jsonString(obj["type"]) == "response_item" {
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
	// Correlate exit codes, and failures an exit code does not show, to
	// their commands.
	for i := range commands {
		id := commands[i].callID
		if id == "" {
			continue
		}
		if code, ok := exitByCall[id]; ok {
			c := code
			commands[i].exitCode = &c
			commands[i].failed = code != 0
		}
		if failedByCall[id] {
			commands[i].failed = true
		}
	}
	return tools, commands, dedupeFileRefs(files)
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
			for _, ref := range fileChangeRefs(use.input, ln) {
				ref.path = repoRelativePath(ref.path, repoRoot)
				files = append(files, ref)
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
	return strings.Contains(text, `\u`) || strings.ContainsRune(text, '\u0130') || fileChangeNamePattern.MatchString(text)
}

// fileChangeRefs reads the file refs of a compact codex FileChange, the tool
// the cli's item path (a current Codex rollout) writes for an edit:
// {"changes":[{"path","kind","move_path","diff"}]}. kind is add, delete or
// update; an update with a move_path deletes path and adds move_path. Like an
// apply_patch, every change counts whatever its result status. Paths are
// trimmed but otherwise as written: toolUseOps makes them repo-relative, and
// codexFileChangePaths infers the session's root from these same paths.
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

// repoRelativePath is p (a trimmed path from fileChangeRefs) relative to
// repoRoot (a clean forward-slash path from codexSessionRoot) when p is inside
// it, so it matches the repo-relative paths apply_patch and the semantic index
// use. Any other path, including one with a ".." segment (see cleanAbsPath),
// is returned as written.
func repoRelativePath(p, repoRoot string) string {
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
// touched (files_touched), so a candidate root that a path is a touched file
// under is a match.
//
// The candidates start from the session's working directory. From cli 0.13.3
// a FileChange's result carries its turn's cwd, and the checkout is that
// directory or one of its parents: Entire's Codex hooks live in the
// checkout's .codex directory, a project config layer Codex loads only when it
// runs inside the checkout, so a session Entire recorded ran there. A path
// outside them, such as a scratch copy under /tmp, cannot give the root, and a
// checkout at the filesystem root can. A session written before cli 0.13.3
// carries no cwd, so every split of every path at a "/" is a candidate,
// except the filesystem root.
//
// Without a cwd, a candidate scores one for each distinct session path that
// is a touched file under it, and the highest score wins. Every candidate is
// scored, so a checkout named like one of its own directories (/work/repo with
// repo/main.go) still gets its full score, and repeated edits of one file
// count once. From a cwd, a candidate wins only when its matches include
// every other candidate's and add to them (see cwdRoot). Either way an
// unsettled contest is ambiguous, whatever else the competing roots hold, so
// it gives "" and the paths are stored as written rather than linked to the
// wrong files. So does a session with any ".." path (see
// codexFileChangePaths), and one with no FileChange or no path that is a
// touched file under a candidate.
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
	paths, cwds, resolved := codexFileChangePaths(transcript)
	if !resolved {
		return ""
	}
	if len(cwds) > 0 {
		return cwdRoot(paths, dirsFrom(cwds), touched)
	}
	return bestCodexRoot(splitRootScores(paths, touched))
}

// splitRootScores scores every split of every path at a "/" (the filesystem
// root aside) as a candidate root: one for each path that is a touched file
// under it.
func splitRootScores(paths []string, touched map[string]bool) map[string]int {
	score := map[string]int{}
	for _, p := range paths {
		for i := 1; i < len(p)-1; i++ {
			if p[i] == '/' && touched[p[i+1:]] {
				score[p[:i]]++
			}
		}
	}
	return score
}

// cwdRoot is the one of roots (the session's cwds and their parents) whose
// matches, the session paths that are a touched file under it, include every
// other root's matches and add at least one, or "" when no root's do.
//
// Counting matches cannot settle a contest where each root explains a path
// the other does not, because either side can be a coincidence of names: an
// edit outside the checkout matching under a parent (/tmp/a.go read as
// tmp/a.go under "/"), or an ignored or reverted edit in the checkout matching
// under a subdirectory cwd (sub/a.go read as a.go). Enough coincidences
// outvote any number of real matches, so such a session is ambiguous. Where
// one root explains everything the other does, the other's matches are no
// evidence against it: a parent of a subdirectory cwd that reads the cwd's
// edits as touched files, and edits beside the cwd too, is the checkout. An
// edit that is not a touched file under any root costs nothing.
func cwdRoot(paths, roots []string, touched map[string]bool) string {
	matches := map[string]map[string]bool{}
	var live []string
	for _, root := range roots {
		m := map[string]bool{}
		for _, p := range paths {
			if rel, ok := pathUnderRoot(p, root); ok && touched[rel] {
				m[p] = true
			}
		}
		if len(m) > 0 {
			matches[root] = m
			live = append(live, root)
		}
	}
	for _, root := range live {
		if coversAll(root, live, matches) {
			return root
		}
	}
	return ""
}

// coversAll reports whether root's matches strictly include those of every
// other root in roots.
func coversAll(root string, roots []string, matches map[string]map[string]bool) bool {
	for _, other := range roots {
		if other == root {
			continue
		}
		adds := countMissing(matches[root], matches[other]) > 0
		misses := countMissing(matches[other], matches[root]) > 0
		if !adds || misses {
			return false
		}
	}
	return true
}

// countMissing counts the paths in a that are not in b.
func countMissing(a, b map[string]bool) int {
	n := 0
	for p := range a {
		if !b[p] {
			n++
		}
	}
	return n
}

// bestCodexRoot is the root with the highest score, or "" when there is none
// or the highest is tied.
func bestCodexRoot(score map[string]int) string {
	best, bestScore, tied := "", 0, false
	for root, n := range score {
		switch {
		case best == "" || n > bestScore:
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
// cleanAbsPath, and every distinct working directory their results carry,
// cleaned the same way. A cwd that is not absolute or has a ".." segment is
// left out. resolved is false when any path has a ".." segment: where that
// path really is cannot be known here, and leaving it out could hand the root
// to whichever paths are left.
func codexFileChangePaths(transcript string) (paths, cwds []string, resolved bool) {
	var ps, cs distinctStrings
	resolved = true
	eachCodexFileChange(transcript, func(use toolUse) {
		for _, ref := range fileChangeRefs(use.input, 0) {
			if hasDotDotSegment(slashPath(ref.path)) {
				resolved = false
			}
			if s, ok := cleanAbsPath(ref.path); ok {
				ps.add(s)
			}
		}
		if s, ok := cleanAbsPath(use.cwd); ok {
			cs.add(s)
		}
	})
	return ps.list, cs.list, resolved
}

// eachCodexFileChange calls fn with each FileChange tool call in a
// transcript, in order, read through recordToolUses as the episode ops read
// them. A line that cannot hold one (mayHoldFileChange) is skipped without
// decoding it. Each call is handed over as its line is read, so a call's
// input, diffs included, is not kept past it.
func eachCodexFileChange(transcript string, fn func(toolUse)) {
	for _, line := range strings.Split(transcript, "\n") {
		if !mayHoldFileChange(line) {
			continue
		}
		record, ok := decodeTranscriptRecord(line)
		if !ok {
			continue
		}
		for _, use := range recordToolUses(record) {
			if use.name == fileChangeToolName {
				fn(use)
			}
		}
	}
}

// distinctStrings collects strings in first-seen order, each once.
type distinctStrings struct {
	seen map[string]bool
	list []string
}

func (d *distinctStrings) add(s string) {
	if d.seen[s] {
		return
	}
	if d.seen == nil {
		d.seen = map[string]bool{}
	}
	d.seen[s] = true
	d.list = append(d.list, s)
}

// dirsFrom is every distinct directory in dirs (clean absolute slash paths
// from cleanAbsPath) and each of their parents, up to and including the
// filesystem root: "/" for a rooted path, the drive ("C:") for a drive path.
// Directories that differ only in case under a drive are one directory (see
// rootKey), spelled as first seen, so they don't tie with each other.
func dirsFrom(dirs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, d := range dirs {
		for {
			if seen[rootKey(d)] {
				break
			}
			seen[rootKey(d)] = true
			out = append(out, d)
			parent := path.Dir(d)
			if parent == d || parent == "." {
				break
			}
			d = parent
		}
	}
	return out
}

// cleanAbsPath is p as a slashPath with "." segments and doubled slashes
// removed, when it is absolute. A path with a ".." segment is refused: the OS
// resolves ".." after symlinks, so root/link/../x.go need not be root/x.go,
// and the checkout it was written in may not be here to look at. A drive
// letter is made upper case: Windows ignores its case, and one session can
// spell it both ways (a cwd of c:/src/repo, a path of C:/src/repo/main.go).
func cleanAbsPath(p string) (string, bool) {
	s := slashPath(p)
	if !isAbsSlashPath(s) || hasDotDotSegment(s) {
		return "", false
	}
	s = path.Clean(s)
	if isDrivePath(s) {
		s = strings.ToUpper(s[:1]) + s[1:]
	}
	return s, true
}

// hasDotDotSegment reports whether a slashPath has a ".." segment.
func hasDotDotSegment(p string) bool {
	return strings.Contains("/"+p+"/", "/../")
}

// pathUnderRoot is p relative to root when p is strictly inside it. Both are
// clean slash paths, so a root that is only a string prefix (/work/repo2 for
// /work/repo) is not a parent. The filesystem root "/" is the parent of every
// other rooted path. Under a drive root the directories match in any case,
// because Windows ignores it and a cwd can be spelled C:/Src/Repo where the
// paths say C:/src/repo; the part of p under root keeps its case, to match
// the checkpoint's files. A rooted path compares exactly, because it may be
// on a filesystem that is case-sensitive.
func pathUnderRoot(p, root string) (string, bool) {
	prefix := root + "/"
	if root == "/" {
		prefix = root
	}
	if len(p) <= len(prefix) {
		return "", false
	}
	if head := p[:len(prefix)]; head == prefix || (isDrivePath(root) && strings.EqualFold(head, prefix)) {
		return p[len(prefix):], true
	}
	return "", false
}

// isDrivePath reports whether a clean slash path starts with a drive letter
// ("C:" or "C:/work").
func isDrivePath(p string) bool {
	return len(p) >= 2 && p[1] == ':' && ((p[0] >= 'a' && p[0] <= 'z') || (p[0] >= 'A' && p[0] <= 'Z'))
}

// rootKey is the spelling of root that roots naming the same directory share:
// root itself, or lower case under a drive, as pathUnderRoot compares it.
func rootKey(root string) string {
	if isDrivePath(root) {
		return strings.ToLower(root)
	}
	return root
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
	return isDrivePath(p) && len(p) >= 3 && p[2] == '/'
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
// A current Codex rollout (the cli's item path) writes, from cli 0.13.3, a
// command's exit code on its result ("exitCode"), which goes into exitByCall,
// and "declined" on a call the user declined. A declined call never ran, so it
// records nothing: it is not a failure and has no exit code. Any other status
// "error" on a command goes into failedByCall, as Codex's own verdict: it also
// marks a command failed that ended with no exit code (one that could not
// start, or was cut off). The item path's output is the command's own, with no
// wrapper, so it is never read for exit text (a test that logs "exited with
// code 1" is not the command's exit code).
//
// Older transcripts keep their fallbacks. The item path before cli 0.13.3
// writes no exit code and no declined flag, so status "error" is its only
// failure signal, and a declined command still counts as failed: its line
// cannot tell the two apart. A rollout of an older Codex (the cli's
// response_item path) marks every result status "success" and has its exit
// code only in the wrapper text Codex puts around the command's output, which
// is parsed when the result carries no exit code of its own.
func compactCodexResults(uses []toolUse, exitByCall map[string]int, failedByCall map[string]bool) {
	for _, use := range uses {
		if use.id == "" || use.declined {
			continue
		}
		if code, ok := compactCodexExitCode(use); ok {
			exitByCall[use.id] = code
		}
		if use.name == codexItemCommandTool && use.status == transcript.ToolStatusError {
			failedByCall[use.id] = true
		}
	}
}

// compactCodexExitCode is the exit code a compact codex call's result gives:
// its exitCode field, or else, for any call but an item-path command, the one
// in Codex's wrapper text.
func compactCodexExitCode(use toolUse) (int, bool) {
	if use.exitCode != nil {
		return *use.exitCode, true
	}
	if use.name == codexItemCommandTool {
		return 0, false
	}
	return parseExitCode(use.output)
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
