package cli

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	entirebrain "github.com/ashtom/entire-brain"
)

const (
	distillTemplateName             = "templates/entire-brain-distill.md"
	distillTaxonomyMarker           = "${TAXONOMY_BLOCK}"
	distillMaxOutputBytes           = 256 * 1024
	distillMaxStructuredOutputBytes = 4 * 1024 * 1024
	distillFactMaxTextSize          = 2000
)

// distillTemplate returns the distillation prompt body shipped with the binary,
// with its YAML frontmatter stripped. The frontmatter is metadata for skill
// installers, not part of the prompt — and crucially the agent runners pass the
// prompt as a positional CLI argument, so a leading "---" would be parsed as an
// unknown flag and rejected. A missing template is a build error, not runtime.
func distillTemplate() (string, error) {
	data, err := entirebrain.Templates.ReadFile(distillTemplateName)
	if err != nil {
		return "", fmt.Errorf("read distill template: %w", err)
	}
	return stripTemplateFrontmatter(string(data)), nil
}

// stripTemplateFrontmatter removes a leading YAML frontmatter block
// ("---\n...\n---\n") and returns the body. Content without frontmatter is
// returned unchanged.
func stripTemplateFrontmatter(content string) string {
	if !strings.HasPrefix(content, "---\n") && !strings.HasPrefix(content, "---\r\n") {
		return content
	}
	lines := strings.Split(content, "\n")
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			// Trim leading CR as well as LF: on Windows the template is checked out
			// with CRLF endings, so splitting on "\n" leaves a stray "\r" on the
			// blank line after the closing delimiter — trimming only "\n" would
			// leave the body starting with "\r\n" (breaking the prefix the agent
			// runners expect and re-introducing a leading dash risk).
			return strings.TrimLeft(strings.Join(lines[i+1:], "\n"), "\r\n")
		}
	}
	return content // no closing delimiter; leave as-is
}

// renderDistillPrompt substitutes the active taxonomy into the template's
// ${TAXONOMY_BLOCK} marker. The rendered prompt is what gets passed to the seed
// agent as its system prompt; the line-numbered transcript chunk is the input.
func renderDistillPrompt(taxonomy factTaxonomy) (string, error) {
	template, err := distillTemplate()
	if err != nil {
		return "", err
	}
	if !strings.Contains(template, distillTaxonomyMarker) {
		return "", fmt.Errorf("distill template missing %s marker", distillTaxonomyMarker)
	}
	return strings.ReplaceAll(template, distillTaxonomyMarker, factTaxonomyBlock(taxonomy)), nil
}

// factTaxonomyBlock renders the taxonomy as the prompt section the agent uses to
// classify facts: the fixed top-level categories and the known three-level
// paths with descriptions. Output is deterministic (sorted) so the rendered
// prompt is stable across runs.
func factTaxonomyBlock(taxonomy factTaxonomy) string {
	var b strings.Builder
	b.WriteString("## Taxonomy\n\n")
	b.WriteString("Top-level categories are fixed. You may invent a new three-level path only under one of these categories:\n\n")

	categories := make([]string, 0, len(taxonomy.Categories))
	for category := range taxonomy.Categories {
		categories = append(categories, category)
	}
	sort.Strings(categories)
	for _, category := range categories {
		fmt.Fprintf(&b, "- `%s` — %s\n", category, taxonomy.Categories[category])
	}

	if len(taxonomy.Paths) > 0 {
		b.WriteString("\nKnown paths (prefer these):\n\n")
		paths := append([]factPathDef(nil), taxonomy.Paths...)
		sort.Slice(paths, func(i, j int) bool { return paths[i].Path < paths[j].Path })
		for _, def := range paths {
			if def.Description != "" {
				fmt.Fprintf(&b, "- `%s` — %s\n", def.Path, def.Description)
			} else {
				fmt.Fprintf(&b, "- `%s`\n", def.Path)
			}
		}
	}
	return b.String()
}

// distillAgentCommandArgs builds the argv that runs the seed agent with the
// rendered distillation prompt. It mirrors seedAgentCommandArgs: the prompt is
// the agent's system prompt and the transcript chunk is supplied on stdin. The
// no-agent mode ("none") has no command — distillation is agent-required.
func distillAgentCommandArgs(agent string, agentCommand []string, prompt string) ([]string, error) {
	if err := rejectAgentForNoEgress(agent); err != nil {
		return nil, err
	}
	switch agent {
	case "command":
		if len(agentCommand) == 0 {
			return nil, errors.New("--agent-command is required when --agent command")
		}
		return append([]string(nil), agentCommand...), nil
	case "codex":
		return []string{"codex", "exec", "--json", "--skip-git-repo-check", "--ephemeral", "--ignore-user-config", "--ignore-rules", "--sandbox", "read-only", prompt}, nil
	case "claude-code":
		return []string{"claude", "--print", "--output-format", "json", "--no-session-persistence", "--setting-sources", "user", "--strict-mcp-config", "--mcp-config", "{\"mcpServers\":{}}", "--disable-slash-commands", "--permission-mode", "dontAsk", "--tools", "", "--system-prompt", prompt}, nil
	case "ollama":
		return []string{"ollama", "", prompt}, nil
	case "none", "":
		return nil, errors.New("distillation requires an agent; --agent none has nothing to run")
	default:
		return nil, fmt.Errorf("unsupported --agent %q", agent)
	}
}

// injectAgentModel inserts a `--model <model>` flag into a codex/claude-code argv
// so the distill run can pin a specific model (e.g. gpt-5.3-codex-spark). It is a
// no-op for an empty model or any other agent (the `command` agent carries its
// own argv). The flag is inserted early — after `codex exec` / after `claude` —
// where both CLIs accept it, ahead of the trailing prompt argument.
func injectAgentModel(args []string, agent, model string) []string {
	if model == "" {
		return args
	}
	switch agent {
	case "codex":
		if len(args) >= 2 && args[0] == "codex" && args[1] == "exec" {
			return append(args[:2:2], append([]string{"--model", model}, args[2:]...)...)
		}
	case "claude-code":
		if len(args) >= 1 && args[0] == "claude" {
			return append(args[:1:1], append([]string{"--model", model}, args[1:]...)...)
		}
	case "ollama":
		if len(args) >= 3 && args[0] == "ollama" {
			out := append([]string(nil), args...)
			out[1] = model
			return out
		}
	}
	return args
}

// injectAgentEffort pins the reasoning effort for a codex/claude-code argv built by
// distillAgentCommandArgs/seedAgentCommandArgs — codex via `--config model_reasoning_effort=<e>`,
// claude-code via `--effort <e>` (the same forms the benchmark harness uses). It mirrors
// injectAgentModel: a no-op for an empty effort, the `command` agent, or an unrecognized shape,
// and inserts early so the trailing prompt argument stays last.
func injectAgentEffort(args []string, agent, effort string) []string {
	if effort == "" {
		return args
	}
	switch agent {
	case "codex":
		if len(args) >= 2 && args[0] == "codex" && args[1] == "exec" {
			return append(args[:2:2], append([]string{"--config", "model_reasoning_effort=" + effort}, args[2:]...)...)
		}
	case "claude-code":
		if len(args) >= 1 && args[0] == "claude" {
			return append(args[:1:1], append([]string{"--effort", effort}, args[1:]...)...)
		}
	}
	return args
}

// distilledFactsFromOutput parses the agent's line-based output for one
// transcript chunk into fact records. Each emitted line is
// `[kind<TAB>]path[,path]<TAB>fact` — the leading KIND column is optional and
// falls back to deterministic inference when absent or invalid. Lines are
// dropped (with a warning) when they have no
// tab, no syntactically valid path, or no path whose top-level category exists
// in the active taxonomy — the taxonomy's top levels are fixed, so a path under
// an unknown category cannot be trusted. Output is capped at factsMaxPerChunk;
// extra lines are dropped with a warning so a runaway agent cannot flood the
// store.
//
// Every produced record cites the supplied anchor as its provenance and is
// stamped origin=distilled, status=active. Ids are content-derived, so the same
// statement from the same chunk is idempotent across reruns.
func distilledFactsFromOutput(output string, taxonomy factTaxonomy, anchor factAnchor, branch string, now time.Time) ([]factRecord, []string) {
	var records []factRecord
	var warnings []string
	capped := false
	sawContent := false
	for _, raw := range strings.Split(output, "\n") {
		line := strings.TrimRight(raw, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		sawContent = true
		// Some models (seen with gpt-5.3-codex-spark) emit the separator as a
		// literal backslash-t escape instead of a real tab character. Recover the
		// first literal `\t` (the leading structural separator); when that leading
		// field is a KIND there is a second structural separator (kind|path) to
		// recover too. Any further literal `\t` belongs to the fact text and is
		// left intact — so a legacy `path\tfact-with-\t` keeps its in-text escape
		// (only one structural tab) while a new `kind\tpath\tfact` recovers both.
		if !strings.ContainsRune(line, '\t') && strings.Contains(line, `\t`) {
			line = strings.Replace(line, `\t`, "\t", 1)
			if fields := strings.SplitN(line, "\t", 2); len(fields) == 2 && validFactKind(fields[0]) {
				line = fields[0] + "\t" + strings.Replace(fields[1], `\t`, "\t", 1)
			}
		}
		if !strings.ContainsRune(line, '\t') {
			// Prose / preamble the template forbids; ignore quietly unless it
			// looks like an attempted fact (contains a path-like token).
			if strings.Contains(line, ".") && strings.Contains(line, " ") {
				warnings = append(warnings, "dropped line without tab separator: "+distillUntrustedExcerpt(line))
			}
			continue
		}
		kind, rawPaths, text := parseFactLine(line)
		if text == "" {
			continue
		}
		paths := normalizeFactPaths(rawPaths)
		paths = filterFactPathsByTaxonomy(paths, taxonomy, &warnings)
		if len(paths) == 0 {
			warnings = append(warnings, "dropped fact with no valid taxonomy path: "+distillUntrustedExcerpt(text))
			continue
		}
		if len(records) >= factsMaxPerChunk {
			capped = true
			continue
		}
		text = truncateString(text, distillFactMaxTextSize)
		// The model is an extractor, not an authority: its text is printed
		// verbatim by recall/search/tree/get/brief and the MCP surfaces, so a
		// control byte it emits is a control byte the reader's terminal
		// executes. Strip before the id is derived, so the stored id matches the
		// stored text.
		if sanitized, stripped := sanitizeDistilledFactText(text); stripped {
			if sanitized == "" {
				warnings = append(warnings, "dropped fact that was entirely control characters: "+distillUntrustedExcerpt(text))
				continue
			}
			warnings = append(warnings, "stripped control characters from fact text: "+distillUntrustedExcerpt(sanitized))
			text = sanitized
		}
		if kind == "" {
			kind = inferFactKind(paths, text) // agent omitted/violated kind → deterministic fallback
		}
		records = append(records, factRecord{
			ID:         factRecordID(text, paths),
			Paths:      paths,
			Kind:       kind,
			Locus:      factLocus(text),
			Text:       text,
			Branch:     branch,
			Origin:     factOriginDistilled,
			Status:     factStatusActive,
			Provenance: []factAnchor{anchor},
			CreatedAt:  now,
			UpdatedAt:  now,
		})
	}
	if capped {
		warnings = append(warnings, fmt.Sprintf("chunk produced more than %d facts; extra lines dropped", factsMaxPerChunk))
	}
	// The agent said SOMETHING and none of it became a fact, yet nothing above
	// found it worth explaining — the per-line drops are deliberately quiet
	// about preamble that does not look like an attempted fact. Reported as-is,
	// that chunk is indistinguishable from a transcript with nothing in it: a
	// model that refused, answered in prose, or was simply the wrong model reads
	// as "0 facts found", no warnings, exit 0, after the call was already paid
	// for. Say it once, with the line, so the cause is visible.
	//
	// Genuinely empty output stays silent: the agent was asked and correctly
	// found nothing, and warning about that would bury a clean corpus in noise.
	if len(records) == 0 && sawContent && len(warnings) == 0 {
		warnings = append(warnings, "agent returned output but no line parsed as a fact: "+distillUntrustedExcerpt(firstNonBlankLine(output)))
	}
	return records, warnings
}

// firstNonBlankLine returns the first line of s that is not blank, for use in a
// diagnostic that has to show what the agent actually said.
func firstNonBlankLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			return line
		}
	}
	return ""
}

// parseFactLine parses one distill output line into (kind, raw paths, fact
// text), tolerating three shapes the agent actually produces:
//
//	path[,path]<TAB>fact                  — legacy 2-field, kind inferred ("")
//	kind<TAB>path[,path]<TAB>fact         — the new column, kind honored
//	<word><TAB>path[,path]<TAB>fact       — an unrecognized leading word (a kind
//	                                        synonym): dropped, kind inferred
//
// The grammar is "an optional single leading non-path token, then path-blocks,
// then text". It first tries reading path-blocks from the front (the legacy
// case); only if that yields no path does it drop one leading field and retry,
// so a genuine path-first line is never misread and a malformed line (no path
// either way) still falls through to be dropped by the caller. This recovers
// the fact whether the agent emits an exact kind, a near-miss synonym, or no
// kind at all — instead of silently dropping a line whose leading token is not
// in the closed set.
func parseFactLine(line string) (string, []string, string) {
	if paths, text := splitFactLine(line); len(normalizeFactPaths(paths)) > 0 {
		return "", paths, text // path-block leads: legacy form, no kind column
	}
	// Leading field is not a path-block — it may be a kind (or a synonym). Drop
	// exactly one field and retry; accept only if that reveals a valid path.
	fields := strings.SplitN(line, "\t", 2)
	if len(fields) == 2 {
		if paths, text := splitFactLine(fields[1]); len(normalizeFactPaths(paths)) > 0 {
			if lead := strings.ToLower(strings.TrimSpace(fields[0])); validFactKind(lead) {
				return lead, paths, text
			}
			return "", paths, text // unrecognized leading word → infer the kind
		}
	}
	// No path either way: return the legacy parse so the caller drops it with the
	// existing "no valid taxonomy path" warning.
	paths, text := splitFactLine(line)
	return "", paths, text
}

// splitFactLine separates a distill output line into its path tokens and fact
// text. The template specifies `path[,path]<TAB>fact`, but agents sometimes
// separate multiple paths with tabs instead of commas (`path<TAB>path<TAB>fact`),
// which would otherwise leak a path into the fact text. It therefore consumes
// leading tab-separated fields for as long as each is a path-block (comma-joined
// valid paths, no prose), and treats the remainder as the fact. Prose cannot
// match the strict path pattern (it contains spaces), so a fact is never
// mistaken for a path.
func splitFactLine(line string) ([]string, string) {
	fields := strings.Split(line, "\t")
	var paths []string
	i := 0
	for i < len(fields)-1 { // always leave at least one field for the text
		field := strings.TrimSpace(fields[i])
		if !fieldLooksLikePaths(field) {
			break
		}
		paths = append(paths, strings.Split(field, ",")...)
		i++
	}
	text := strings.TrimSpace(strings.Join(fields[i:], " "))
	return paths, text
}

// fieldLooksLikePaths reports whether a tab-separated field is entirely a
// path-block: one or more comma-joined, syntactically valid taxonomy paths.
func fieldLooksLikePaths(field string) bool {
	if field == "" {
		return false
	}
	for _, part := range strings.Split(field, ",") {
		if !validFactPath(strings.ToLower(strings.TrimSpace(part))) {
			return false
		}
	}
	return true
}

// filterFactPathsByTaxonomy keeps only paths whose top-level category exists in
// the taxonomy, recording a warning for each dropped path. Paths are already
// syntactically valid (normalizeFactPaths ran first).
func filterFactPathsByTaxonomy(paths []string, taxonomy factTaxonomy, warnings *[]string) []string {
	tops := factTaxonomyTopLevels(taxonomy)
	kept := paths[:0:0]
	for _, path := range paths {
		if _, ok := tops[factTopLevel(path)]; ok {
			kept = append(kept, path)
			continue
		}
		*warnings = append(*warnings, fmt.Sprintf("dropped path under unknown category: %s", path))
	}
	return kept
}

// isDisplayControlRune reports whether a rune steers how following text is
// DISPLAYED rather than carrying content: the Unicode bidirectional overrides
// and isolates, which can silently reverse the reading order of a fact, and the
// zero-width/BOM characters that can hide content inside one. They are not
// "control characters" by unicode.IsControl (they are category Cf), so they
// need naming explicitly.
func isDisplayControlRune(r rune) bool {
	switch {
	case r >= 0x200B && r <= 0x200F: // ZWSP, ZWNJ, ZWJ, LRM, RLM
		return true
	case r >= 0x202A && r <= 0x202E: // LRE, RLE, PDF, LRO, RLO
		return true
	case r >= 0x2066 && r <= 0x2069: // LRI, RLI, FSI, PDI
		return true
	case r == 0xFEFF: // zero-width no-break space / BOM
		return true
	}
	return false
}

// distillFactTextRuneIsDroppable reports whether the rune at byte offset i of
// text must not reach the fact store. The offset is needed to tell a genuine
// U+FFFD (three bytes, real content a model may legitimately emit) from a
// decode failure over one invalid byte, which range yields as the same rune.
func distillFactTextRuneIsDroppable(text string, i int, r rune) bool {
	if r == utf8.RuneError {
		_, size := utf8.DecodeRuneInString(text[i:])
		return size == 1
	}
	return unicode.IsControl(r) || isDisplayControlRune(r)
}

// sanitizeDistilledFactText removes every rune that can steer a terminal or the
// reading order from one distilled fact's text, and reports whether anything
// was removed. It strips rather than rejects: the legible content of a fact is
// worth keeping, the escape sequence never is, and a caller that drops the
// whole fact over one stray byte loses real extraction to a cosmetic defect.
// The empty result is the caller's signal that nothing legible remained.
//
// Text that needs no change is returned verbatim, with stripped false, so the
// content-derived fact id is stable and no warning claims a strip that did not
// happen.
func sanitizeDistilledFactText(text string) (string, bool) {
	dirty := false
	for i, r := range text {
		if distillFactTextRuneIsDroppable(text, i, r) {
			dirty = true
			break
		}
	}
	if !dirty {
		return text, false
	}
	var b strings.Builder
	b.Grow(len(text))
	for i, r := range text {
		if distillFactTextRuneIsDroppable(text, i, r) {
			continue
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String()), true
}

// distillUntrustedExcerpt renders a fragment of AGENT OUTPUT for a warning
// message. Warnings are printed to the operator's terminal and embedded in the
// --json summary, so quoting agent text into one is the same untrusted-input
// boundary the fact store has: an escape sequence in a line the parser REJECTED
// would otherwise reach the terminal precisely because it was rejected. Strip
// first, then bound the length.
func distillUntrustedExcerpt(text string) string {
	cleaned, _ := sanitizeDistilledFactText(text)
	return truncateString(strings.TrimSpace(cleaned), 120)
}
