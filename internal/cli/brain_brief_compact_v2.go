package cli

import (
	"crypto/sha256"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

const (
	brainBriefCompactV2Marker = "entire.brain_brief compact_v2"
	// The legend is part of the hashed packet body. "Previous" is scoped to
	// the same opcode, so unrelated physical rows between two records do not
	// reset a metadata repeat reference.
	brainBriefCompactV2Legend = "legend ~=absent ^=previous_same_opcode_record_same_column"
)

type compactV2FieldKind uint8

const (
	compactV2String compactV2FieldKind = iota
	compactV2Strings
	compactV2Bool
	compactV2Int
	compactV2Float
)

type compactV2Field struct {
	name      string
	kind      compactV2FieldKind
	reference bool
}

type compactV2Schema struct {
	tag            string
	opcode         byte
	fields         []compactV2Field
	fieldPositions map[string]int
	declaration    string
}

type compactV1RawField struct {
	key   string
	value string
}

type compactV1RawRecord struct {
	tag    string
	fields []compactV1RawField
	raw    string
}

type compactV2FamilyPlan struct {
	positional  bool
	declaration string
	rows        []string
}

func compactV2Natural(name string, kind compactV2FieldKind) compactV2Field {
	return compactV2Field{name: name, kind: kind}
}

func compactV2Metadata(name string, kind compactV2FieldKind) compactV2Field {
	return compactV2Field{name: name, kind: kind, reference: true}
}

func compactV2SchemaFor(tag string, opcode byte, fields ...compactV2Field) compactV2Schema {
	positions := make(map[string]int, len(fields))
	for i, field := range fields {
		positions[field.name] = i
	}
	schema := compactV2Schema{tag: tag, opcode: opcode, fields: fields, fieldPositions: positions}
	var declaration strings.Builder
	writeCompactV2Declaration(&declaration, schema)
	schema.declaration = declaration.String()
	return schema
}

// compactV2Schemas is the complete, versioned positional schema. Natural
// language columns deliberately do not permit repeat references: an agent must
// see task text, excerpts, guidance, actions, evidence, reasons, details,
// titles, descriptions, and workflow prose directly in every record.
var compactV2Schemas = []compactV2Schema{
	compactV2SchemaFor("task", 'q',
		compactV2Natural("value", compactV2String)),
	compactV2SchemaFor("status", 's',
		compactV2Metadata("freshness", compactV2String), compactV2Metadata("repo_key", compactV2String),
		compactV2Metadata("brain_schema", compactV2Int), compactV2Metadata("branch", compactV2String),
		compactV2Metadata("head", compactV2String), compactV2Metadata("dirty", compactV2Bool),
		compactV2Metadata("changed_files", compactV2Int)),
	compactV2SchemaFor("sources", 'u',
		compactV2Metadata("seed", compactV2Bool), compactV2Metadata("sessions", compactV2Bool),
		compactV2Metadata("semantic", compactV2Bool), compactV2Metadata("history", compactV2Bool),
		compactV2Metadata("facts", compactV2Bool)),
	compactV2SchemaFor("live", 'l',
		compactV2Metadata("staged", compactV2Strings), compactV2Metadata("unstaged", compactV2Strings),
		compactV2Metadata("untracked", compactV2Strings), compactV2Metadata("changed", compactV2Strings),
		compactV2Natural("diff_stat", compactV2String)),
	compactV2SemanticSymbolSchema("live_symbol", 'L', false),
	compactV2SchemaFor("facts_status", 'F',
		compactV2Metadata("facts", compactV2Int), compactV2Metadata("distilled", compactV2Int),
		compactV2Metadata("authored", compactV2Int), compactV2Metadata("superseded", compactV2Int),
		compactV2Metadata("branches", compactV2Int), compactV2Metadata("proposals", compactV2Int),
		compactV2Metadata("verified_facts", compactV2Int), compactV2Metadata("verified", compactV2Int),
		compactV2Metadata("stale", compactV2Int), compactV2Metadata("orphaned", compactV2Int),
		compactV2Metadata("unverifiable_here", compactV2Int), compactV2Metadata("sampled_of", compactV2Int)),
	compactV2SchemaFor("semantic_status", 'S',
		compactV2Metadata("provider", compactV2String), compactV2Metadata("provider_version", compactV2String),
		compactV2Metadata("schema", compactV2String), compactV2Metadata("capabilities", compactV2Strings),
		compactV2Metadata("files", compactV2Int), compactV2Metadata("symbols", compactV2Int),
		compactV2Metadata("relations", compactV2Int), compactV2Metadata("warnings", compactV2Int),
		compactV2Metadata("partial_failures", compactV2Int)),
	compactV2SchemaFor("freshness_axis", 'a',
		compactV2Metadata("name", compactV2String), compactV2Metadata("state", compactV2String),
		compactV2Natural("detail", compactV2String)),
	compactV2WarningSchema("freshness_warning", 'w'),
	compactV2WarningSchema("semantic_warning", 'W'),
	compactV2WarningSchema("semantic_partial_failure", 'E'),
	compactV2SchemaFor("blind_spot", 'b',
		compactV2Metadata("path", compactV2String), compactV2Metadata("code", compactV2String),
		compactV2Natural("detail", compactV2String)),
	compactV2SchemaFor("edit_file", 'e', compactV2Metadata("path", compactV2String)),
	compactV2SchemaFor("test_file", 't', compactV2Metadata("path", compactV2String)),
	compactV2SchemaFor("likely_file", 'p', compactV2Metadata("path", compactV2String)),
	compactV2SemanticSymbolSchema("symbol", 'y', false),
	compactV2SemanticRelationSchema("relation", 'r'),
	compactV2EvidenceSchema("relation_evidence", 'R'),
	compactV2SemanticSymbolSchema("neighbor", 'n', false),
	compactV2SemanticRelationSchema("runtime_trace", 'x'),
	compactV2EvidenceSchema("runtime_trace_evidence", 'X'),
	compactV2SemanticSymbolSchema("test_root", 'o', false),
	compactV2SemanticSymbolSchema("test_suggestion", 'O', true),
	compactV2SchemaFor("history", 'h',
		compactV2Metadata("path", compactV2String), compactV2Metadata("line", compactV2Int),
		compactV2Metadata("timestamp", compactV2String), compactV2Metadata("score", compactV2Int),
		compactV2Metadata("matched_terms", compactV2Strings), compactV2Natural("excerpt", compactV2String)),
	compactV2SchemaFor("fact", 'f',
		compactV2Metadata("id", compactV2String), compactV2Metadata("paths", compactV2Strings),
		compactV2Metadata("kind", compactV2String), compactV2Metadata("locus", compactV2Strings),
		compactV2Natural("text", compactV2String), compactV2Metadata("branch", compactV2String),
		compactV2Metadata("origin", compactV2String), compactV2Metadata("status", compactV2String),
		compactV2Metadata("confidence", compactV2String), compactV2Metadata("related_ids", compactV2Strings),
		compactV2Metadata("superseded_by", compactV2String), compactV2Metadata("anchors", compactV2Int),
		compactV2Metadata("verified_anchors", compactV2Int), compactV2Metadata("stale_locus", compactV2Strings)),
	compactV2SchemaFor("fact_drift", 'd',
		compactV2Metadata("id", compactV2String), compactV2Metadata("stale_locus", compactV2Strings)),
	compactV2SchemaFor("action", 'c',
		compactV2Metadata("file", compactV2String), compactV2Metadata("symbol", compactV2String),
		compactV2Natural("action", compactV2String), compactV2Natural("evidence", compactV2String)),
	compactV2SchemaFor("pattern", 'P',
		compactV2Metadata("id", compactV2String), compactV2Metadata("type", compactV2String),
		compactV2Metadata("scope", compactV2String), compactV2Metadata("kind", compactV2String),
		compactV2Natural("title", compactV2String), compactV2Metadata("strength", compactV2Float),
		compactV2Metadata("strength_label", compactV2String), compactV2Metadata("support", compactV2Int),
		compactV2Metadata("repos", compactV2Int), compactV2Metadata("skill_status", compactV2String),
		compactV2Natural("note", compactV2String), compactV2Natural("intent_sig", compactV2String),
		compactV2Natural("gram", compactV2String), compactV2Metadata("dossier_status", compactV2String),
		compactV2Metadata("verdict", compactV2String), compactV2Metadata("workspace", compactV2String),
		compactV2Metadata("success", compactV2Int), compactV2Metadata("corrected", compactV2Int),
		compactV2Metadata("neutral", compactV2Int), compactV2Metadata("example_path", compactV2String),
		compactV2Metadata("example_line", compactV2Int)),
	compactV2SchemaFor("consolidation", 'C',
		compactV2Metadata("pattern_id", compactV2String), compactV2Metadata("type", compactV2String),
		compactV2Natural("title", compactV2String), compactV2Natural("trigger", compactV2String),
		compactV2Metadata("confidence", compactV2Float), compactV2Metadata("status", compactV2String),
		compactV2Metadata("verdict", compactV2String), compactV2Natural("workflow", compactV2Strings),
		compactV2Natural("verification", compactV2Strings), compactV2Natural("failure_modes", compactV2Strings),
		compactV2Metadata("anchor_transcript", compactV2String), compactV2Metadata("anchor_start", compactV2Int),
		compactV2Metadata("anchor_end", compactV2Int), compactV2Metadata("anchor_outcome", compactV2String)),
	compactV2SchemaFor("theme", 'T',
		compactV2Metadata("id", compactV2String), compactV2Natural("title", compactV2String),
		compactV2Natural("description", compactV2String), compactV2Metadata("shape", compactV2String),
		compactV2Metadata("support", compactV2Int), compactV2Metadata("strength", compactV2Float),
		compactV2Metadata("status", compactV2String), compactV2Metadata("verdict", compactV2String)),
	compactV2SchemaFor("guidance", 'g', compactV2Natural("text", compactV2String)),
	compactV2SchemaFor("warning", '!',
		compactV2Metadata("source", compactV2String), compactV2Natural("text", compactV2String)),
}

func compactV2SemanticSymbolSchema(tag string, opcode byte, suggestion bool) compactV2Schema {
	fields := []compactV2Field{
		compactV2Metadata("id", compactV2String), compactV2Metadata("kind", compactV2String),
		compactV2Metadata("name", compactV2String), compactV2Metadata("short_name", compactV2String),
		compactV2Metadata("file", compactV2String), compactV2Metadata("start", compactV2Int),
		compactV2Metadata("end", compactV2Int), compactV2Metadata("path", compactV2String),
		compactV2Natural("signature", compactV2String), compactV2Metadata("language", compactV2String),
		compactV2Metadata("score", compactV2Int), compactV2Metadata("confidence", compactV2Float),
		compactV2Natural("reason", compactV2String), compactV2Metadata("warning_codes", compactV2Strings),
	}
	if suggestion {
		fields = append(fields, compactV2Natural("suggestion_reason", compactV2String))
	}
	return compactV2SchemaFor(tag, opcode, fields...)
}

func compactV2SemanticRelationSchema(tag string, opcode byte) compactV2Schema {
	return compactV2SchemaFor(tag, opcode,
		compactV2Metadata("index", compactV2Int), compactV2Metadata("id", compactV2String),
		compactV2Metadata("type", compactV2String), compactV2Metadata("from", compactV2String),
		compactV2Metadata("to", compactV2String), compactV2Metadata("file", compactV2String),
		compactV2Metadata("start", compactV2Int), compactV2Metadata("end", compactV2Int),
		compactV2Metadata("path", compactV2String), compactV2Metadata("scope", compactV2String),
		compactV2Metadata("resolution", compactV2String), compactV2Metadata("target_kind", compactV2String),
		compactV2Metadata("confidence", compactV2Float), compactV2Natural("reason", compactV2String),
		compactV2Metadata("warning_codes", compactV2Strings))
}

func compactV2EvidenceSchema(tag string, opcode byte) compactV2Schema {
	return compactV2SchemaFor(tag, opcode,
		compactV2Metadata("owner_index", compactV2Int), compactV2Metadata("index", compactV2Int),
		compactV2Metadata("kind", compactV2String), compactV2Metadata("file", compactV2String),
		compactV2Metadata("start", compactV2Int), compactV2Metadata("end", compactV2Int),
		compactV2Natural("detail", compactV2String))
}

func compactV2WarningSchema(tag string, opcode byte) compactV2Schema {
	return compactV2SchemaFor(tag, opcode,
		compactV2Metadata("code", compactV2String), compactV2Metadata("severity", compactV2String),
		compactV2Metadata("path", compactV2String), compactV2Natural("effect", compactV2String),
		compactV2Natural("detail", compactV2String))
}

// emitBrainBriefCompactV2 transcodes the exact compact_v1 semantic record
// stream. Keeping v1 as the projection source makes field selection, record
// ordering, duplicates, and privacy omissions identical by construction while
// v2 changes only their wire representation.
func emitBrainBriefCompactV2(cmd *cobra.Command, report brainBriefReport) error {
	if err := validateBrainBriefCompactNumbers(report, "compact_v2"); err != nil {
		return err
	}
	var v1 strings.Builder
	v1cmd := &cobra.Command{}
	v1cmd.SetOut(&v1)
	if err := emitBrainBriefCompactV1(v1cmd, report); err != nil {
		return err
	}
	packet, err := transcodeBrainBriefCompactV2(v1.String())
	if err != nil {
		return err
	}
	_, err = io.WriteString(cmd.OutOrStdout(), packet)
	return err
}

func transcodeBrainBriefCompactV2(v1 string) (string, error) {
	records, err := parseBrainBriefCompactV1Body(v1)
	if err != nil {
		return "", err
	}
	schemasByTag := make(map[string]compactV2Schema, len(compactV2Schemas))
	for _, schema := range compactV2Schemas {
		if _, exists := schemasByTag[schema.tag]; exists {
			return "", fmt.Errorf("compact_v2 duplicate schema tag %q", schema.tag)
		}
		schemasByTag[schema.tag] = schema
	}
	for _, record := range records {
		if _, ok := schemasByTag[record.tag]; !ok {
			return "", fmt.Errorf("compact_v2 has no schema for %q", record.tag)
		}
	}
	plans, err := planCompactV2Families(records, schemasByTag)
	if err != nil {
		return "", err
	}

	var out strings.Builder
	out.Grow(len(v1))
	out.WriteString(brainBriefCompactV2Marker)
	out.WriteByte('\n')
	out.WriteString(brainBriefCompactV2Legend)
	out.WriteByte('\n')
	positions := make(map[string]int, len(plans))
	for _, record := range records {
		plan := plans[record.tag]
		if !plan.positional {
			out.WriteString(record.raw)
			out.WriteByte('\n')
			continue
		}
		position := positions[record.tag]
		if position == 0 {
			out.WriteString(plan.declaration)
		}
		out.WriteString(plan.rows[position])
		positions[record.tag] = position + 1
	}
	body := out.String()
	digest := sha256.Sum256([]byte(body))
	fmt.Fprintf(&out, "end\t%d\t%x\n", len(records), digest)
	return out.String(), nil
}

// planCompactV2Families compares the exact canonical bytes for each complete
// record family. A repeated family uses positional rows only when declaration
// plus row bytes are strictly smaller than its keyed v1 rows. Equal sizes are
// keyed, which freezes a deterministic tie rule and prevents declarations from
// ever making a family grow.
func planCompactV2Families(records []compactV1RawRecord, schemasByTag map[string]compactV2Schema) (map[string]compactV2FamilyPlan, error) {
	type familyState struct {
		plan            compactV2FamilyPlan
		previous        []string
		keyedBytes      int
		positionalBytes int
		records         int
	}
	counts := make(map[string]int, len(schemasByTag))
	for _, record := range records {
		counts[record.tag]++
	}
	states := make(map[string]familyState, len(schemasByTag))
	for _, record := range records {
		schema := schemasByTag[record.tag]
		state := states[record.tag]
		if state.records == 0 {
			state.plan.declaration = schema.declaration
			state.plan.rows = make([]string, 0, counts[record.tag])
			state.positionalBytes = len(schema.declaration)
		}
		row, cells, err := encodeCompactV2Row(schema, record, state.previous)
		if err != nil {
			return nil, err
		}
		state.plan.rows = append(state.plan.rows, row)
		state.previous = cells
		state.keyedBytes += len(record.raw) + 1
		state.positionalBytes += len(row)
		state.records++
		states[record.tag] = state
	}

	plans := make(map[string]compactV2FamilyPlan, len(states))
	for tag, state := range states {
		state.plan.positional = compactV2UsePositional(state.records, state.keyedBytes, state.positionalBytes)
		plans[tag] = state.plan
	}
	return plans, nil
}

func compactV2UsePositional(recordCount, keyedBytes, positionalBytes int) bool {
	return recordCount > 1 && positionalBytes < keyedBytes
}

func encodeCompactV2Row(schema compactV2Schema, record compactV1RawRecord, previous []string) (string, []string, error) {
	cells, last, err := compactV2Cells(schema, record)
	if err != nil {
		return "", nil, err
	}
	var out strings.Builder
	rowBytes := 3 + last // opcode, tabs, and trailing newline
	for i := 0; i <= last; i++ {
		rowBytes += len(cells[i])
	}
	out.Grow(rowBytes)
	out.WriteByte(schema.opcode)
	for i := 0; i <= last; i++ {
		out.WriteByte('\t')
		cell := cells[i]
		if schema.fields[i].reference && cell != "~" && len(previous) == len(cells) && previous[i] == cell && len(cell) > 1 {
			out.WriteByte('^')
		} else {
			out.WriteString(cell)
		}
	}
	out.WriteByte('\n')
	return out.String(), cells, nil
}

func writeCompactV2Declaration(out *strings.Builder, schema compactV2Schema) {
	out.WriteByte('@')
	out.WriteByte(schema.opcode)
	out.WriteByte('=')
	out.WriteString(schema.tag)
	out.WriteByte('(')
	for i, field := range schema.fields {
		if i > 0 {
			out.WriteByte(',')
		}
		out.WriteString(field.name)
	}
	out.WriteString(")\n")
}

func compactV2Cells(schema compactV2Schema, record compactV1RawRecord) ([]string, int, error) {
	cells := make([]string, len(schema.fields))
	for i := range cells {
		cells[i] = "~"
	}
	last := -1
	for _, field := range record.fields {
		position, ok := schema.fieldPositions[field.key]
		if !ok {
			return nil, -1, fmt.Errorf("compact_v2 %s has unknown field %q", record.tag, field.key)
		}
		if position <= last {
			return nil, -1, fmt.Errorf("compact_v2 %s fields are not in schema order", record.tag)
		}
		encoded, err := encodeCompactV2Cell(schema.fields[position].kind, field.value)
		if err != nil {
			return nil, -1, fmt.Errorf("compact_v2 %s.%s: %w", record.tag, field.key, err)
		}
		cells[position] = encoded
		last = position
	}
	return cells, last, nil
}

func encodeCompactV2Cell(kind compactV2FieldKind, raw string) (string, error) {
	switch kind {
	case compactV2String:
		value, err := strconv.Unquote(raw)
		if err != nil {
			return "", fmt.Errorf("invalid quoted string")
		}
		if compactV2SafeAtom(value) {
			return value, nil
		}
		return raw, nil
	case compactV2Strings:
		return encodeCompactV2StringArray(raw)
	case compactV2Bool:
		switch raw {
		case "true":
			return "1", nil
		case "false":
			return "0", nil
		default:
			return "", fmt.Errorf("invalid boolean")
		}
	case compactV2Int:
		if _, err := strconv.Atoi(raw); err != nil {
			return "", fmt.Errorf("invalid integer")
		}
		return raw, nil
	case compactV2Float:
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
			return "", fmt.Errorf("invalid finite float")
		}
		return raw, nil
	default:
		return "", fmt.Errorf("unknown field kind")
	}
}

func compactV2SafeAtom(value string) bool {
	if value == "" || value == "~" || value == "^" {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' && i > 0 {
			continue
		}
		switch c {
		case '_', '.', '/', ':', '@', '+', '-':
			continue
		default:
			return false
		}
	}
	return true
}

func encodeCompactV2StringArray(raw string) (string, error) {
	values, err := parseCompactV1StringArray(raw)
	if err != nil {
		return "", err
	}
	var out strings.Builder
	out.Grow(len(raw))
	out.WriteByte('[')
	for i, value := range values {
		if i > 0 {
			out.WriteByte(',')
		}
		if compactV2SafeAtom(value) {
			out.WriteString(value)
		} else {
			out.WriteString(strconv.Quote(value))
		}
	}
	out.WriteByte(']')
	return out.String(), nil
}

func parseCompactV1StringArray(raw string) ([]string, error) {
	if len(raw) < 2 || raw[0] != '[' || raw[len(raw)-1] != ']' {
		return nil, fmt.Errorf("invalid string array")
	}
	if raw == "[]" {
		return nil, nil
	}
	var values []string
	for cursor := 1; cursor < len(raw)-1; {
		if raw[cursor] != '"' {
			return nil, fmt.Errorf("invalid quoted array element")
		}
		end, err := scanCompactQuoted(raw, cursor)
		if err != nil || end > len(raw)-1 {
			return nil, fmt.Errorf("invalid quoted array element")
		}
		value, err := strconv.Unquote(raw[cursor:end])
		if err != nil {
			return nil, fmt.Errorf("invalid quoted array element")
		}
		values = append(values, value)
		cursor = end
		if cursor == len(raw)-1 {
			break
		}
		if raw[cursor] != ',' {
			return nil, fmt.Errorf("invalid array separator")
		}
		cursor++
		if cursor == len(raw)-1 {
			return nil, fmt.Errorf("trailing array separator")
		}
	}
	return values, nil
}

func parseBrainBriefCompactV1Body(packet string) ([]compactV1RawRecord, error) {
	if !strings.HasSuffix(packet, "\n") {
		return nil, fmt.Errorf("compact_v1 packet has no final newline")
	}
	lines := strings.Split(strings.TrimSuffix(packet, "\n"), "\n")
	if len(lines) < 3 || lines[0] != brainBriefCompactV1Marker {
		return nil, fmt.Errorf("compact_v1 marker mismatch")
	}
	end, err := parseCompactV1RawRecord(lines[len(lines)-1])
	if err != nil || end.tag != "end" {
		return nil, fmt.Errorf("compact_v1 end record missing")
	}
	records := make([]compactV1RawRecord, 0, len(lines)-2)
	for _, line := range lines[1 : len(lines)-1] {
		record, err := parseCompactV1RawRecord(line)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, nil
}

func parseCompactV1RawRecord(line string) (compactV1RawRecord, error) {
	if line == "" {
		return compactV1RawRecord{}, fmt.Errorf("empty compact_v1 record")
	}
	tagEnd := strings.IndexByte(line, ' ')
	if tagEnd < 0 {
		return compactV1RawRecord{tag: line, raw: line}, nil
	}
	record := compactV1RawRecord{tag: line[:tagEnd], raw: line}
	for cursor := tagEnd + 1; cursor < len(line); {
		keyStart := cursor
		for cursor < len(line) && line[cursor] != '=' && line[cursor] != ' ' {
			cursor++
		}
		if cursor == keyStart || cursor >= len(line) || line[cursor] != '=' {
			return compactV1RawRecord{}, fmt.Errorf("invalid compact_v1 field in %q", record.tag)
		}
		key := line[keyStart:cursor]
		cursor++
		valueStart := cursor
		if cursor >= len(line) {
			return compactV1RawRecord{}, fmt.Errorf("missing compact_v1 value in %q", record.tag)
		}
		var err error
		switch line[cursor] {
		case '"':
			cursor, err = scanCompactQuoted(line, cursor)
		case '[':
			cursor, err = scanCompactArray(line, cursor)
		default:
			for cursor < len(line) && line[cursor] != ' ' {
				cursor++
			}
		}
		if err != nil || cursor == valueStart {
			return compactV1RawRecord{}, fmt.Errorf("invalid compact_v1 value in %q", record.tag)
		}
		record.fields = append(record.fields, compactV1RawField{key: key, value: line[valueStart:cursor]})
		if cursor == len(line) {
			break
		}
		if line[cursor] != ' ' || cursor+1 == len(line) || line[cursor+1] == ' ' {
			return compactV1RawRecord{}, fmt.Errorf("invalid compact_v1 field separator in %q", record.tag)
		}
		cursor++
	}
	return record, nil
}

func scanCompactQuoted(value string, start int) (int, error) {
	for cursor := start + 1; cursor < len(value); cursor++ {
		switch value[cursor] {
		case '\\':
			cursor++
			if cursor >= len(value) {
				return 0, fmt.Errorf("unterminated quoted value")
			}
		case '"':
			return cursor + 1, nil
		case '\n', '\r':
			return 0, fmt.Errorf("unescaped line break")
		}
	}
	return 0, fmt.Errorf("unterminated quoted value")
}

func scanCompactArray(value string, start int) (int, error) {
	for cursor := start + 1; cursor < len(value); cursor++ {
		switch value[cursor] {
		case '"':
			end, err := scanCompactQuoted(value, cursor)
			if err != nil {
				return 0, err
			}
			cursor = end - 1
		case ']':
			return cursor + 1, nil
		case '\n', '\r':
			return 0, fmt.Errorf("unescaped line break")
		}
	}
	return 0, fmt.Errorf("unterminated array value")
}
