package cli

import (
	"fmt"
	"strconv"
	"strings"
)

// brainBriefAgentPacketProfile keeps versioned wire identities explicit while
// sharing the byte-sensitive prefix packer. Footer encoding and integrity
// validation remain version-owned callbacks, so adding a new schema cannot
// silently change an older packet contract.
type brainBriefAgentPacketProfile[C any] struct {
	format   string
	marker   string
	budget   int
	add      func(*C, brainBriefAgentV1Record)
	sized    func(bodyBytes, bodyRecords int, emitted, available C) int
	finalize func(body string, bodyRecords int, emitted, available C) string
	validate func(packet string) error
}

func packBrainBriefAgentPacket[C any](
	mandatory []brainBriefAgentV1Record,
	optional [][]brainBriefAgentV1Record,
	profile brainBriefAgentPacketProfile[C],
) (string, C, error) {
	var available C
	for _, record := range mandatory {
		profile.add(&available, record)
	}
	for _, section := range optional {
		for _, record := range section {
			profile.add(&available, record)
		}
	}

	var emitted C
	bodyCapacity := len(profile.marker) + 1
	for _, record := range mandatory {
		profile.add(&emitted, record)
		bodyCapacity += len(record.line)
	}
	for _, section := range optional {
		for _, record := range section {
			bodyCapacity += len(record.line)
		}
	}
	if bodyCapacity > profile.budget {
		bodyCapacity = profile.budget
	}
	var body strings.Builder
	body.Grow(bodyCapacity)
	body.WriteString(profile.marker)
	body.WriteByte('\n')
	for _, record := range mandatory {
		body.WriteString(record.line)
	}
	bodyRecords := len(mandatory)
	mandatoryPacketBytes := profile.sized(body.Len(), bodyRecords, emitted, available)
	if mandatoryPacketBytes > profile.budget {
		var zero C
		return "", zero, fmt.Errorf(
			"%s mandatory packet is %d bytes, exceeds %d-byte budget",
			profile.format, mandatoryPacketBytes, profile.budget,
		)
	}

	// Each optional section is a prefix. A rejected record stops that section,
	// while lower-priority sections may still use residual capacity.
	for _, section := range optional {
		for _, record := range section {
			candidateCounts := emitted
			profile.add(&candidateCounts, record)
			candidateBodyBytes := body.Len() + len(record.line)
			candidateBodyRecords := bodyRecords + 1
			if profile.sized(candidateBodyBytes, candidateBodyRecords, candidateCounts, available) > profile.budget {
				break
			}
			body.WriteString(record.line)
			bodyRecords = candidateBodyRecords
			emitted = candidateCounts
		}
	}

	packet := profile.finalize(body.String(), bodyRecords, emitted, available)
	if len(packet) > profile.budget {
		var zero C
		return "", zero, fmt.Errorf(
			"%s packet is %d bytes, exceeds %d-byte budget",
			profile.format, len(packet), profile.budget,
		)
	}
	if err := profile.validate(packet); err != nil {
		var zero C
		return "", zero, err
	}
	return packet, emitted, nil
}

func validateBrainBriefAgentPacketSchema(
	format, marker string,
	schemas []brainBriefAgentV1RecordSchema,
	packet string,
) error {
	if !strings.HasSuffix(packet, "\n") {
		return fmt.Errorf("%s packet has no final newline", format)
	}
	lines := strings.Split(strings.TrimSuffix(packet, "\n"), "\n")
	if len(lines) < 2 || lines[0] != marker {
		return fmt.Errorf("%s marker mismatch", format)
	}
	byTag := make(map[string]brainBriefAgentV1RecordSchema, len(schemas))
	for _, schema := range schemas {
		if _, duplicate := byTag[schema.tag]; duplicate {
			return fmt.Errorf("%s duplicate record schema %q", format, schema.tag)
		}
		byTag[schema.tag] = schema
	}
	for _, line := range lines[1:] {
		record, err := parseCompactV1RawRecord(line)
		if err != nil {
			return fmt.Errorf("%s record: %w", format, err)
		}
		schema, ok := byTag[record.tag]
		if !ok {
			return fmt.Errorf("%s has no schema for record %q", format, record.tag)
		}
		positions := make(map[string]int, len(schema.fields))
		for position, field := range schema.fields {
			positions[field] = position
		}
		last := -1
		for _, field := range record.fields {
			position, ok := positions[field.key]
			if !ok {
				return fmt.Errorf("%s %s has unknown field %q", format, record.tag, field.key)
			}
			if position <= last {
				return fmt.Errorf("%s %s fields are not in schema order", format, record.tag)
			}
			last = position
		}
	}
	return nil
}

func brainBriefAgentRecordField(format string, record compactV1RawRecord, key string) (string, error) {
	for _, field := range record.fields {
		if field.key == key {
			return field.value, nil
		}
	}
	return "", fmt.Errorf("%s %s.%s missing", format, record.tag, key)
}

func brainBriefAgentRecordString(format string, record compactV1RawRecord, key string) (string, error) {
	raw, err := brainBriefAgentRecordField(format, record, key)
	if err != nil {
		return "", err
	}
	value, err := strconv.Unquote(raw)
	if err != nil {
		return "", fmt.Errorf("%s %s.%s: %w", format, record.tag, key, err)
	}
	return value, nil
}

func brainBriefAgentRecordInt(format string, record compactV1RawRecord, key string) (int, error) {
	raw, err := brainBriefAgentRecordField(format, record, key)
	if err != nil {
		return 0, err
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s %s.%s: %w", format, record.tag, key, err)
	}
	return value, nil
}

func brainBriefAgentRecordBool(format string, record compactV1RawRecord, key string) (bool, error) {
	raw, err := brainBriefAgentRecordField(format, record, key)
	if err != nil {
		return false, err
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s %s.%s: %w", format, record.tag, key, err)
	}
	return value, nil
}
