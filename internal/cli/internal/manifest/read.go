package manifest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
)

// ReadFile decodes the canonical manifest without a byte ceiling.
// It uses the same checked descriptor as bounded state readers, without needing
// the source repository or Git. encoding/json buffers a whole JSON value and
// the manifest is materialized in memory: this is NOT a bounded-memory reader.
// Rewinding this descriptor (never reopening the path) preserves schema-first
// checks and tolerant reads versus strict rewrites, including unknown fields.
func ReadFile(brainDir string, tolerateUnknown bool) (*Manifest, int, bool, bool, error) {
	f, present, err := brainstore.OpenMemoryStateFileExpected(brainDir, FileName, "brain manifest", nil)
	if err != nil {
		return nil, 0, false, present, fmt.Errorf("read brain manifest: %w", err)
	}
	if !present {
		return &Manifest{SchemaVersion: SchemaVersion}, 0, false, false, nil
	}
	defer f.Close()
	manifest, version, dropped, err := DecodeFile(f, tolerateUnknown)
	return manifest, version, dropped, true, err
}

func decodePass(f *os.File, out any, strict bool) error {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	decoder := json.NewDecoder(f)
	if strict {
		decoder.DisallowUnknownFields()
	}
	if err := decoder.Decode(out); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return brainstore.ErrTrailingJSONData
	}
	return nil
}

func DecodeFile(f *os.File, tolerateUnknown bool) (*Manifest, int, bool, error) {
	var header struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := decodePass(f, &header, false); err != nil {
		return nil, 0, false, fmt.Errorf("parse brain manifest: %s: brain manifest cannot be parsed: %w", brainstore.StateCorruptCode, err)
	}
	version := header.SchemaVersion
	if version > SchemaVersion {
		return nil, version, false, fmt.Errorf("%s: brain manifest schema version %d is newer than supported version %d; upgrade entire-brain to read it", brainstore.UnsupportedVersionCode, version, SchemaVersion)
	}
	var manifest Manifest
	err := decodePass(f, &manifest, true)
	dropped := false
	if tolerateUnknown && brainstore.IsUnknownFieldError(err) {
		manifest = Manifest{}
		err = decodePass(f, &manifest, false)
		dropped = err == nil
	}
	if err != nil {
		// An unknown field is not damage, and saying "cannot be parsed" sends
		// the reader looking for corruption that is not there. The manifest is
		// entirely readable; a WRITER is refusing it, because re-encoding from
		// a struct that never saw the field would erase what another build
		// recorded. Readers pass tolerateUnknown and are unaffected.
		//
		// Observed cost of the old wording: a component sat failed for four
		// days reporting memory_state_corrupt, while the manifest was fine and
		// the condition had already gone away.
		//
		// The error CODE stays memory_state_corrupt deliberately. It is part of
		// the maintenance taxonomy with many consumers, so retyping this case
		// is a contract change for a human to make, not a side effect of
		// improving a sentence.
		if !tolerateUnknown && brainstore.IsUnknownFieldError(err) {
			return nil, version, false, fmt.Errorf(
				"parse brain manifest: %s: brain manifest carries a field this build does not know (%w); "+
					"the file is readable and is NOT damaged, but a writer will not rewrite it and silently drop that field. "+
					"Use a build that knows the field, or let a read-only command run instead",
				brainstore.StateCorruptCode, err)
		}
		return nil, version, false, fmt.Errorf("parse brain manifest: %s: brain manifest cannot be parsed: %w", brainstore.StateCorruptCode, err)
	}
	if version < MinSchemaVersion {
		return nil, version, false, fmt.Errorf("%s: brain manifest declares schema version %d, older than the first supported version %d", brainstore.UnsupportedVersionCode, version, MinSchemaVersion)
	}
	Normalize(&manifest)
	return &manifest, version, dropped, nil
}

// Load reads the manifest for the callers that only consume it —
// status, doctor, the memory worker, distillation, search.
//
// It tolerates fields it does not declare. Manifest fields are added and retired
// without a schema bump (history_coverage.checkpointed_unexported_commits was
// retired at version 3), so every manifest written before such a change carries
// a field a later build has never heard of. Rejecting those bytes reported the
// whole brain as memory_state_corrupt: all sources off, no facts, and the memory
// worker failing on its retry loop indefinitely, for state that was merely
// written by a different build and is entirely readable.
//
// Writers must not be this forgiving — see LoadForReplace.
func Load(outputDir string) (*Manifest, error) {
	manifest, _, err := Read(outputDir, true)
	return manifest, err
}

// Exists reports whether a brain export (its manifest file) is present.
func Exists(brainDir string) bool {
	info, err := os.Stat(filepath.Join(brainDir, FileName))
	return err == nil && !info.IsDir()
}

// LoadForReplace reads the manifest on behalf of a writer about to
// overwrite it, and refuses fields it cannot round-trip: re-encoding from a
// struct that never saw them erases what another build recorded. Availability is
// the reader's concern; not destroying state is this one's.
func LoadForReplace(outputDir string) (*Manifest, error) {
	manifest, _, err := Read(outputDir, false)
	// NOT isUnknownFieldError: that matches on a PREFIX, and by the time the
	// error reaches here it has been wrapped ("parse brain manifest: ..."), so
	// the prefix never matches and the remedy was silently never attached.
	if IsRetiredFieldError(err) {
		// Durable, and the bare decoder error says none of this. A field that
		// was RETIRED without a schema bump leaves every manifest written by
		// the older build unwritable by this one -- for ever, because nothing
		// in the normal path rewrites it. On this machine that silently drove
		// a two-hour relaunch chain per repository, with a live sleeping
		// process per retry, and the worker log recorded only "prepass_failed".
		return manifest, errors.New(RetiredFieldMessage(err))
	}
	return manifest, err
}

// IsRetiredFieldError reports a manifest a writer cannot round-trip because
// it carries a field this build no longer declares.
//
// It is a DURABLE condition: no amount of retrying changes the bytes on disk,
// so a relaunch chain spends two hours and ends exactly where it started.
// retiredRemedy is the one wording, so the "already said it" check
// below cannot drift from the thing it is checking for.
// A func, not a const: the spelling depends on how the reader reached this
// binary, which is only knowable at call time.
func retiredRemedy() string {
	return "; this manifest carries a field this build retired, so it cannot be rewritten without erasing what another build recorded -- rebuild it with `" + base.BrainInvocation(os.LookupEnv, "refresh") + " --force`"
}

// RetiredFieldMessage states the failure AND what to do about it.
//
// The remedy has to be attached here rather than at the read, because the
// manifest is read through several paths and only some of them wrap the error.
// Measured: the delta-export path reported the bare decoder error, so the one
// line a reader actually saw named a JSON field and no action at all.
func RetiredFieldMessage(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	// Idempotent. LoadForReplace already attaches this remedy, and
	// the worker then asks for the message again -- appending unconditionally
	// printed the whole explanation TWICE. Caught by weakening: deleting this
	// function's retired-field branch made the output better, which is only
	// possible if the branch was doing harm.
	if strings.Contains(msg, retiredRemedy()) {
		return msg
	}
	return msg + retiredRemedy()
}

func IsRetiredFieldError(err error) bool {
	return brainstore.IsUnknownFieldError(err) || (err != nil &&
		strings.Contains(err.Error(), brainstore.StateCorruptCode) &&
		strings.Contains(err.Error(), "unknown field"))
}

// Read also reports whether it dropped fields written by another
// build, so status and doctor can recommend a refresh instead of staying silent.
func Read(outputDir string, tolerateUnknownFields bool) (*Manifest, bool, error) {
	manifest, _, dropped, _, err := ReadFile(outputDir, tolerateUnknownFields)
	return manifest, dropped, err
}

func Normalize(manifest *Manifest) {
	if manifest.SchemaVersion == 0 {
		manifest.SchemaVersion = 1
	}
	if manifest.Sources == nil {
		manifest.Sources = &Sources{}
	}
	if manifest.Sources.Sessions == nil && (len(manifest.Sessions) > 0 || manifest.TranscriptMode != "" || manifest.CheckpointsScanned > 0) {
		manifest.Sources.Sessions = SessionSourceFromExport(*manifest)
	}
}

func SessionSourceFromExport(manifest Manifest) *SessionSource {
	return &SessionSource{
		GeneratedAt:        manifest.GeneratedAt,
		EntireCLIVersion:   manifest.EntireCLIVersion,
		DefaultBranch:      manifest.DefaultBranch,
		TranscriptMode:     manifest.TranscriptMode,
		TranscriptStorage:  manifest.TranscriptStorage,
		Scope:              manifest.Scope,
		CheckpointLimit:    manifest.CheckpointLimit,
		CheckpointsScanned: manifest.CheckpointsScanned,
		OldestSessionAt:    OldestSessionAt(manifest.Sessions),
		LatestCheckpointID: LatestCheckpointID(manifest.Sessions),
		Branches:           append([]Branch(nil), manifest.Branches...),
		Sessions:           append([]Session(nil), manifest.Sessions...),
		Warnings:           append([]string(nil), manifest.Warnings...),
	}
}

func OldestSessionAt(sessions []Session) *time.Time {
	var oldest time.Time
	for _, session := range sessions {
		if session.CreatedAt.IsZero() {
			continue
		}
		if oldest.IsZero() || session.CreatedAt.Before(oldest) {
			oldest = session.CreatedAt.UTC()
		}
	}
	if oldest.IsZero() {
		return nil
	}
	return &oldest
}

func LatestCheckpointID(sessions []Session) string {
	var latest Session
	for _, session := range sessions {
		if latest.SessionID == "" || shouldReplaceSession(latest, session) {
			latest = session
		}
	}
	return latest.LatestCheckpoint
}

func shouldReplaceSession(current, candidate Session) bool {
	if candidate.CreatedAt.After(current.CreatedAt) {
		return true
	}
	if current.CreatedAt.Equal(candidate.CreatedAt) {
		return candidate.LatestCheckpoint > current.LatestCheckpoint
	}
	return false
}
