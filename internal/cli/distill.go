package cli

import (
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	entirebrain "github.com/entireio/entire-brain"
	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
)

const (
	distillTemplateName   = "templates/entire-brain-distill.md"
	distillTaxonomyMarker = "${TAXONOMY_BLOCK}"
	distillMaxOutputBytes = 256 * 1024

	// Ollama /api/generate echoes the prompt back as a `context` token array the
	// distiller never reads. Measured at ~3x the prompt bytes for dense ASCII;
	// 8x leaves headroom for tokenizers that split finer, while keeping the read
	// bounded by a request size we control.
	distillOllamaEnvelopeFactor = 8
	distillOllamaEnvelopeSlack  = 64 * 1024
	// The envelope allowance scales off a request size the caller chooses, and
	// --max-chunk-bytes has no upper bound, so without a ceiling a hostile or
	// malfunctioning loopback endpoint could amplify a large chunk into a much
	// larger read than the old fixed cap ever permitted. 32 MiB is far above any
	// workable chunk size (a usable chunk is tens of KiB) while keeping the read
	// bounded no matter what is configured.
	distillOllamaMaxEnvelopeBytes = 32 * 1024 * 1024
	// Budget for the /api/ps context-window probe, independent of the generate
	// call's own timeout so a long generation cannot starve it.
	ollamaContextProbeTimeout       = 10 * time.Second
	distillMaxStructuredOutputBytes = 4 * 1024 * 1024
	distillFactMaxTextSize          = 2000
)

// distillTemplate returns the distillation prompt body shipped with the binary,
// with its YAML frontmatter stripped. The frontmatter is metadata for skill
// installers, not part of the prompt — and a leading "---" would be read as a
// flag by any agent that still takes instructions positionally (codex reads
// them from stdin here, but `--agent command` wrappers may not). A missing
// template is a build error, not runtime.
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
func renderDistillPrompt(taxonomy factstore.Taxonomy) (string, error) {
	template, err := distillTemplate()
	if err != nil {
		return "", err
	}
	if !strings.Contains(template, distillTaxonomyMarker) {
		return "", fmt.Errorf("distill template missing %s marker", distillTaxonomyMarker)
	}
	return strings.ReplaceAll(template, distillTaxonomyMarker, factstore.TaxonomyBlock(taxonomy)), nil
}

// distilledFactsFromOutput parses the agent's line-based output for one
// transcript chunk into fact records. Each emitted line is
// `[kind<TAB>]path[,path]<TAB>fact` — the leading KIND column is optional and
// falls back to deterministic inference when absent or invalid. Lines are
// dropped (with a warning) when they have no
// tab, no syntactically valid path, or no path whose top-level category exists
// in the active taxonomy — the taxonomy's top levels are fixed, so a path under
// an unknown category cannot be trusted. Output is capped at factstore.MaxPerChunk;
// extra lines are dropped with a warning so a runaway agent cannot flood the
// store.
//
// Every produced record cites the supplied anchor as its provenance and is
// stamped origin=distilled, status=active. Ids are content-derived, so the same
// statement from the same chunk is idempotent across reruns.
func distilledFactsFromOutput(output string, taxonomy factstore.Taxonomy, anchor factAnchor, branch string, now time.Time) ([]factRecord, []string) {
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
		paths = factstore.FilterPathsByTaxonomy(paths, taxonomy, &warnings)
		if len(paths) == 0 {
			warnings = append(warnings, "dropped fact with no valid taxonomy path: "+distillUntrustedExcerpt(text))
			continue
		}
		if len(records) >= factstore.MaxPerChunk {
			capped = true
			continue
		}
		text = base.TruncateString(text, distillFactMaxTextSize)
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
			kind = factstore.InferKind(paths, text) // agent omitted/violated kind → deterministic fallback
		}
		records = append(records, factRecord{
			ID:         factRecordID(text, paths),
			Paths:      paths,
			Kind:       kind,
			Locus:      base.FactLocus(text),
			Text:       text,
			Branch:     branch,
			Origin:     factstore.OriginDistilled,
			Status:     factStatusActive,
			Provenance: []factAnchor{anchor},
			CreatedAt:  now,
			UpdatedAt:  now,
		})
	}
	if capped {
		warnings = append(warnings, fmt.Sprintf("chunk produced more than %d facts; extra lines dropped", factstore.MaxPerChunk))
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

// stampFactExtractor records which agent/model/effort generated the facts, so
// eval runs can compare fact quality across extraction models.
func stampFactExtractor(records []factRecord, agent, model, effort string) {
	for i := range records {
		records[i].Agent = strings.TrimSpace(agent)
		records[i].Model = strings.TrimSpace(model)
		records[i].Effort = strings.TrimSpace(effort)
	}
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
	return base.TruncateString(strings.TrimSpace(cleaned), 120)
}
