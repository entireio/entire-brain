package cli

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	entirebrain "github.com/ashtom/entire-brain"
)

const (
	distillTemplateName    = "templates/entire-brain-distill.md"
	distillTaxonomyMarker  = "${TAXONOMY_BLOCK}"
	distillMaxOutputBytes  = 256 * 1024
	distillFactMaxTextSize = 2000
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
	switch agent {
	case "command":
		if len(agentCommand) == 0 {
			return nil, errors.New("--agent-command is required when --agent command")
		}
		return append([]string(nil), agentCommand...), nil
	case "codex":
		return []string{"codex", "exec", "--skip-git-repo-check", "--ephemeral", "--ignore-user-config", "--ignore-rules", "--sandbox", "read-only", prompt}, nil
	case "claude-code":
		return []string{"claude", "--print", "--no-session-persistence", "--setting-sources", "user", "--strict-mcp-config", "--mcp-config", "{\"mcpServers\":{}}", "--disable-slash-commands", "--permission-mode", "dontAsk", "--tools", "", "--system-prompt", prompt}, nil
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
// `path[,path]<TAB>fact`. Lines are dropped (with a warning) when they have no
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
	for _, raw := range strings.Split(output, "\n") {
		line := strings.TrimRight(raw, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		// Some models (seen with gpt-5.3-codex-spark) emit the separator as a
		// literal backslash-t escape instead of a real tab character. Recover it:
		// convert the *first* literal `\t` to a real tab when no real tab is
		// present. Only the first is the path/fact separator — a later `\t` belongs
		// to the fact text and must be left intact. The leading field is still
		// validated as a taxonomy path below, so a wrongful conversion cannot
		// manufacture a bogus fact.
		if !strings.ContainsRune(line, '\t') && strings.Contains(line, `\t`) {
			line = strings.Replace(line, `\t`, "\t", 1)
		}
		if !strings.ContainsRune(line, '\t') {
			// Prose / preamble the template forbids; ignore quietly unless it
			// looks like an attempted fact (contains a path-like token).
			if strings.Contains(line, ".") && strings.Contains(line, " ") {
				warnings = append(warnings, "dropped line without tab separator: "+truncateString(strings.TrimSpace(line), 120))
			}
			continue
		}
		rawPaths, text := splitFactLine(line)
		if text == "" {
			continue
		}
		paths := normalizeFactPaths(rawPaths)
		paths = filterFactPathsByTaxonomy(paths, taxonomy, &warnings)
		if len(paths) == 0 {
			warnings = append(warnings, "dropped fact with no valid taxonomy path: "+truncateString(text, 120))
			continue
		}
		if len(records) >= factsMaxPerChunk {
			capped = true
			continue
		}
		text = truncateString(text, distillFactMaxTextSize)
		records = append(records, factRecord{
			ID:         factRecordID(text, paths),
			Paths:      paths,
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
	return records, warnings
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
