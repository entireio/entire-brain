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

// distillTemplate returns the raw distillation prompt template shipped with the
// binary. A missing template is a programming/build error, not a runtime one.
func distillTemplate() (string, error) {
	data, err := entirebrain.Templates.ReadFile(distillTemplateName)
	if err != nil {
		return "", fmt.Errorf("read distill template: %w", err)
	}
	return string(data), nil
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
		return []string{"claude", "--print", "--no-session-persistence", "--setting-sources", "user", "--strict-mcp-config", "--mcp-config", "{}", "--disable-slash-commands", "--permission-mode", "dontAsk", "--tools", "", "--system-prompt", prompt}, nil
	case "none", "":
		return nil, errors.New("distillation requires an agent; --agent none has nothing to run")
	default:
		return nil, fmt.Errorf("unsupported --agent %q", agent)
	}
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
		tab := strings.IndexByte(line, '\t')
		if tab < 0 {
			// Prose / preamble the template forbids; ignore quietly unless it
			// looks like an attempted fact (contains a path-like token).
			if strings.Contains(line, ".") && strings.Contains(line, " ") {
				warnings = append(warnings, "dropped line without tab separator: "+truncateString(strings.TrimSpace(line), 120))
			}
			continue
		}
		rawPaths := strings.Split(strings.TrimSpace(line[:tab]), ",")
		text := strings.TrimSpace(line[tab+1:])
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
