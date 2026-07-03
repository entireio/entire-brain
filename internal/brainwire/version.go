package brainwire

import (
	"fmt"
	"strconv"
	"strings"
)

// BrainSchemaVersion is the wire schema version this build produces and can
// read. It is a "major.minor" string governed by the schema-compatibility
// policy of ADR-0001 (mirrored in docs/semantic_brain_plan.md, "Schema
// compatibility policy"):
//
//   - The major component is the compatibility boundary. A reader supports
//     exactly one major (this one) and refuses any other.
//   - Minor bumps are additive-only: a higher minor within the same major adds
//     fields but removes/renames nothing, so an older reader stays compatible
//     by ignoring the unknown fields (a tolerant reader).
//   - Any breaking change requires a major bump.
const BrainSchemaVersion = "1.0"

// CheckCompatibility compares a remote artifact's brain_schema_version against
// the local BrainSchemaVersion and reports whether the local reader may consume
// it, per the ADR-0001 rules:
//
//   - Same major: ok (warn is empty when the remote minor is <= local).
//   - Unknown or greater major (and any unparseable version): hard error; ok is
//     false and err is non-nil.
//   - Greater minor within the same major: ok, with a non-empty warn advising
//     that newer additive fields will be silently ignored. This tolerant-reader
//     behavior only holds because decoders MUST NOT use
//     json.Decoder.DisallowUnknownFields; see the package doc and version doc.
//
// A caller should hard-fail when err != nil, surface warn to the operator when
// it is non-empty, and otherwise proceed.
func CheckCompatibility(remote string) (ok bool, warn string, err error) {
	localMajor, localMinor, err := parseSchemaVersion(BrainSchemaVersion)
	if err != nil {
		// The local const is validated by tests; treat a bad local value as a
		// programming error rather than a remote-compatibility outcome.
		return false, "", fmt.Errorf("brainwire: invalid local BrainSchemaVersion %q: %w", BrainSchemaVersion, err)
	}

	remoteMajor, remoteMinor, err := parseSchemaVersion(remote)
	if err != nil {
		return false, "", fmt.Errorf("brainwire: unparseable remote brain_schema_version %q: %w", remote, err)
	}

	if remoteMajor != localMajor {
		return false, "", fmt.Errorf(
			"brainwire: incompatible brain_schema_version %q: major %d is not supported (local supports major %d)",
			remote, remoteMajor, localMajor,
		)
	}

	if remoteMinor > localMinor {
		return true, fmt.Sprintf(
			"brainwire: remote brain_schema_version %q is newer than local %s; unknown additive fields will be ignored (tolerant reader)",
			remote, BrainSchemaVersion,
		), nil
	}

	return true, "", nil
}

// parseSchemaVersion parses a strict "major.minor" version into its two
// non-negative integer components. It rejects anything that is not exactly two
// dot-separated non-negative integers (e.g. "1", "1.2.3", "1.x", "-1.0", "").
func parseSchemaVersion(v string) (major, minor int, err error) {
	parts := strings.Split(strings.TrimSpace(v), ".")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("want \"major.minor\", got %q", v)
	}
	major, err = parseComponent(parts[0])
	if err != nil {
		return 0, 0, fmt.Errorf("major component of %q: %w", v, err)
	}
	minor, err = parseComponent(parts[1])
	if err != nil {
		return 0, 0, fmt.Errorf("minor component of %q: %w", v, err)
	}
	return major, minor, nil
}

// parseComponent parses one non-negative version component, rejecting empty
// strings, signs, and non-digits.
func parseComponent(s string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("empty component")
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("non-digit in %q", s)
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, err
	}
	return n, nil
}
