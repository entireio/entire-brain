package cli

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
)

const (
	brainBriefCompactV3Marker = "entire.brain_brief c3"
	// V3 retains v2's record schemas, body grammar, and strictly-shorter family
	// selection. It has a compact marker, permits the existing exact-value
	// repeat marker in every column, and uses an unpadded base64url checksum.
	brainBriefCompactV3Legend = brainBriefCompactV2Legend
)

// compactV3Schemas copies the frozen v2 positional schema and widens the
// repeat-reference allowlist. Field order, types, opcodes, and declarations
// remain byte-for-byte identical. A repeat marker is emitted only for an exact
// prior value in the same opcode and column; unique values remain inline.
var compactV3Schemas = compactV3AllReferenceSchemas()

// compactV3SchemasByTag is immutable after initialization and safe for
// concurrent emitters. Keeping the versioned lookup here also validates the
// frozen schema once instead of rebuilding the same map for every packet.
var compactV3SchemasByTag = compactV3SchemaLookup()

func compactV3AllReferenceSchemas() []compactV2Schema {
	schemas := make([]compactV2Schema, len(compactV2Schemas))
	for i, schema := range compactV2Schemas {
		schemas[i] = schema
		schemas[i].fields = append([]compactV2Field(nil), schema.fields...)
		for j := range schemas[i].fields {
			schemas[i].fields[j].reference = true
		}
	}
	return schemas
}

func compactV3SchemaLookup() map[string]compactV2Schema {
	byTag := make(map[string]compactV2Schema, len(compactV3Schemas))
	for _, schema := range compactV3Schemas {
		if _, exists := byTag[schema.tag]; exists {
			panic(fmt.Sprintf("compact_v3 duplicate schema tag %q", schema.tag))
		}
		byTag[schema.tag] = schema
	}
	return byTag
}

// emitBrainBriefCompactV3 transcodes the same compact_v1 canonical solving
// projection as compact_v2. V3 remains opt-in; no default selects it.
func emitBrainBriefCompactV3(cmd *cobra.Command, report brainBriefReport) error {
	if err := validateBrainBriefCompactNumbers(report, "compact_v3"); err != nil {
		return err
	}
	var v1 strings.Builder
	v1cmd := &cobra.Command{}
	v1cmd.SetOut(&v1)
	if err := emitBrainBriefCompactV1(v1cmd, report); err != nil {
		return err
	}
	packet, err := transcodeBrainBriefCompactV3(v1.String())
	if err != nil {
		return err
	}
	_, err = io.WriteString(cmd.OutOrStdout(), packet)
	return err
}

func transcodeBrainBriefCompactV3(v1 string) (string, error) {
	records, err := parseBrainBriefCompactV1Body(v1)
	if err != nil {
		return "", fmt.Errorf("compact_v3: %w", err)
	}
	for _, record := range records {
		if _, ok := compactV3SchemasByTag[record.tag]; !ok {
			return "", fmt.Errorf("compact_v3 has no schema for %q", record.tag)
		}
	}
	plans, err := planCompactV2Families(records, compactV3SchemasByTag)
	if err != nil {
		return "", fmt.Errorf("compact_v3: %w", err)
	}

	var out strings.Builder
	out.Grow(len(v1))
	out.WriteString(brainBriefCompactV3Marker)
	out.WriteByte('\n')
	out.WriteString(brainBriefCompactV3Legend)
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
	fmt.Fprintf(&out, "end\t%d\t%s\n", len(records), base64.RawURLEncoding.EncodeToString(digest[:]))
	return out.String(), nil
}
