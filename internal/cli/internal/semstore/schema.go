package semstore

import (
	"fmt"
	"strconv"
	"strings"

	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
)

const (
	SupportedMajor = "1"
	// SchemaVersion is the newest provider schema this build knows how
	// to ingest in full. A newer minor is still accepted (ADR 0001: minors are
	// additive) but warns, because facts added after this version are dropped.
	//
	// 1.2 added identity_revision (consumed: internal/entityindex keys every
	// stored delta by it and re-migrates when it changes). 1.3 added
	// completeness.health (consumed: carried verbatim as the Completeness raw
	// message and persisted on the manifest) and the partial-failure language
	// field above. 1.4 added the derived relation provenance label (modeled on
	// Record so the streaming filter round-trips it; the provider
	// computes it from type and resolution, both already modeled, so it carries
	// no information this build was missing). Leaving this at 1.1 fired
	// provider_schema_newer_minor on EVERY index against a current
	// entire-graph, telling users facts were dropped when they were not;
	// leaving it at 1.3 against the released graph v0.5.0 pair did the same.
	SchemaVersion        = "1.4"
	SupportedSchemaRange = "semantic schema >=1.0 <2.0"
)

// ParseSchemaVersion splits a provider schema_version into its major
// and minor numbers. ADR 0001 fixes the shape as major.minor, so anything that
// is not two non-negative integers is not a version this reader can reason
// about: the old implementation only looked at the text before the first dot,
// which silently accepted "1.abc" and "1." as if they were ordinary 1.x minors
// and let a malformed header ride straight into the store.
func ParseSchemaVersion(version string) (int, int, error) {
	majorText, rest, ok := strings.Cut(version, ".")
	if !ok {
		return 0, 0, fmt.Errorf("semantic schema version %q is not major.minor", version)
	}
	// A trailing patch component is tolerated and ignored: it is additive, and
	// refusing it would violate the tolerant-reader rule.
	minorText, _, _ := strings.Cut(rest, ".")
	major, err := strconv.Atoi(majorText)
	if err != nil || major < 0 {
		return 0, 0, fmt.Errorf("semantic schema version %q has a non-numeric major", version)
	}
	minor, err := strconv.Atoi(minorText)
	if err != nil || minor < 0 {
		return 0, 0, fmt.Errorf("semantic schema version %q has a non-numeric minor", version)
	}
	return major, minor, nil
}

// ValidateSchema applies ADR 0001's compatibility boundary: refuse an
// unknown major, accept every minor within the supported major.
func ValidateSchema(version string) error {
	major, _, err := ParseSchemaVersion(version)
	if err != nil {
		return fmt.Errorf("%w (this build reads %s); reinstall the provider with `entire plugin install graph`", err, SupportedSchemaRange)
	}
	if strconv.Itoa(major) != SupportedMajor {
		// Name which side of the pin is behind, so the reader upgrades the
		// right binary instead of reinstalling the one that already works.
		action := "the provider is behind this build; upgrade it with `entire plugin install graph`"
		if supportedMajor, convErr := strconv.Atoi(SupportedMajor); convErr == nil && major > supportedMajor {
			action = "the provider is ahead of this build; upgrade entire-brain"
		}
		return fmt.Errorf("unsupported semantic schema major version %q: this build reads %s — %s", strconv.Itoa(major), SupportedSchemaRange, action)
	}
	return nil
}

// SchemaMinorSkewWarning implements ADR 0001 rule 3: a newer minor
// within the supported major is accepted, but the reader must WARN, because the
// producer may have emitted additive facts this build does not know how to
// ingest. Accepting silently is not compliance — the whole point of the warning
// is that the resulting index is quietly less complete than the snapshot.
//
// This is the single implementation of that rule, applied at every place a
// schema_version crosses into this build alongside ValidateSchema:
//
//  1. GraphStoreSource — the live Graph-owned store header, recorded on the
//     semantic source manifest it assembles for index, refresh, status and
//     brief;
//  2. semanticFreshnessReport — the "graph" freshness axis reported by brain
//     doctor/status and every semantic read command, degraded rather than ok;
//  3. semanticRefreshNeeded — refresh's non-fatal identity warning;
//  4. validateImportedBundle — an imported bundle manifest, warning appended to
//     the semantic source that is then persisted.
//
// It returns nil for an unparseable or non-supported-major version:
// ValidateSchema is the sole authority on rejecting those outright.
// validateImportedBundle runs it directly, first; the three live-path call
// sites above never see a header ValidateSchema would reject, because
// fetchGraphStore already ran it as a hard gate before producing the
// descriptor they read schema_version from.
func SchemaMinorSkewWarning(version string) *bmanifest.SemanticWarning {
	major, minor, err := ParseSchemaVersion(version)
	if err != nil {
		return nil
	}
	knownMajor, knownMinor, err := ParseSchemaVersion(SchemaVersion)
	if err != nil || strconv.Itoa(major) != SupportedMajor || minor <= knownMinor || major != knownMajor {
		return nil
	}
	return &bmanifest.SemanticWarning{
		Code:     "provider_schema_newer_minor",
		Severity: "warning",
		Effect:   "additive semantic facts introduced after " + SchemaVersion + " may not have been ingested",
		// Kept under statusCauseWidth: this Detail is rendered inside the
		// one-line status cause, and the longer wording was cut mid-word
		// ("...so the snapshot is usabl") in the short report.
		Detail: fmt.Sprintf(
			"provider schema_version %q is newer than this build's %s; minors are additive, so the snapshot is usable -- upgrade to ingest everything",
			version, SchemaVersion),
	}
}
