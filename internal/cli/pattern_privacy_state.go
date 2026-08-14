package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// patternCorpusPrivacyArtifactRels derives the privacy inventory from the
// publication layer's canonical sidecar set. Keep the ownership of those
// names in pattern_corpus.go: publication, privacy deletion, and verification
// must agree about every SQLite leaf, including rollback journals.
func patternCorpusPrivacyArtifactRels() []string {
	rels := make([]string, 0, 1+len(patternCorpusSidecarNames))
	rels = append(rels, patternCorpusPath)
	for _, name := range patternCorpusSidecarNames {
		rels = append(rels, filepath.ToSlash(filepath.Join("patterns", name)))
	}
	return rels
}

func isPatternCorpusPrivacyArtifactRel(rel string) bool {
	rel = filepath.ToSlash(rel)
	for _, candidate := range patternCorpusPrivacyArtifactRels() {
		if rel == candidate {
			return true
		}
	}
	return false
}

func isPatternCorpusRollbackJournalRel(rel string) bool {
	rel = filepath.ToSlash(rel)
	for _, name := range patternCorpusSidecarNames {
		if strings.HasSuffix(name, "-journal") && rel == filepath.ToSlash(filepath.Join("patterns", name)) {
			return true
		}
	}
	return false
}

func isPatternCorpusStagingArtifactRel(rel string) bool {
	rel = filepath.ToSlash(rel)
	dir, name := filepath.ToSlash(filepath.Dir(rel)), filepath.Base(filepath.FromSlash(rel))
	return dir == "patterns" && strings.HasPrefix(name, patternCorpusStagingPrefix)
}

type patternCorpusPublicationReadState string

const (
	patternCorpusPublicationCorrupt     patternCorpusPublicationReadState = "corrupt"
	patternCorpusPublicationUnsafe      patternCorpusPublicationReadState = "unsafe"
	patternCorpusPublicationUnsupported patternCorpusPublicationReadState = "unsupported"
)

// patternCorpusPublicationLoadError keeps publication-marker failures
// machine-classifiable at privacy inventory and deletion boundaries. In
// particular, a newer producer's marker is unsupported read-only state, not a
// disposable corrupt leaf.
type patternCorpusPublicationLoadError struct {
	Code    string
	State   patternCorpusPublicationReadState
	Version int
	Err     error
}

func (e *patternCorpusPublicationLoadError) Error() string {
	detail := "pattern corpus publication marker is invalid"
	if e.Err != nil {
		detail = e.Err.Error()
	}
	if strings.HasPrefix(detail, e.Code+":") {
		return detail
	}
	return e.Code + ": " + detail
}

func (e *patternCorpusPublicationLoadError) Unwrap() error { return e.Err }

func newPatternCorpusPublicationLoadError(err error, version int) error {
	code := memoryErrorCode(err)
	state := patternCorpusPublicationCorrupt
	switch code {
	case memoryErrUnsupportedVersion:
		state = patternCorpusPublicationUnsupported
	case memoryErrStateUnsafe:
		state = patternCorpusPublicationUnsafe
	default:
		code = memoryErrStateCorrupt
	}
	return &patternCorpusPublicationLoadError{Code: code, State: state, Version: version, Err: err}
}

// decodePatternCorpusPublication is the single classifier for publication
// markers at both recovery and privacy-cleanup boundaries. It classifies the
// version header before a strict current-schema decode so a future producer's
// additional fields remain opaque, read-only state.
func decodePatternCorpusPublication(data []byte) (patternCorpusPublication, error) {
	var publication patternCorpusPublication
	version, err := decodeStrictVersionedJSON(data, &publication, patternCorpusPublicationVersion, "pattern corpus publication marker")
	if err != nil {
		return patternCorpusPublication{}, newPatternCorpusPublicationLoadError(err, version)
	}
	decodedSHA, decodeErr := hex.DecodeString(publication.SHA256)
	if publication.SchemaVersion != patternCorpusPublicationVersion ||
		!strings.HasPrefix(publication.Candidate, patternCorpusStagingPrefix) ||
		filepath.Base(publication.Candidate) != publication.Candidate ||
		strings.ContainsAny(publication.Candidate, `/\`) ||
		decodeErr != nil || len(decodedSHA) != sha256.Size || publication.SHA256 != strings.ToLower(publication.SHA256) ||
		publication.Size < 1 || publication.Size > privacySQLiteSnapshotMaxBytes {
		return patternCorpusPublication{}, newPatternCorpusPublicationLoadError(
			fmt.Errorf("%s: invalid pattern corpus publication marker", memoryErrStateCorrupt), publication.SchemaVersion)
	}
	return publication, nil
}

// loadPatternCorpusPublicationForPrivacy binds the parsed marker bytes to the
// exact regular-file identity observed by inventory. Callers may retain info
// only while holding the Brain privacy/write locks; removal rechecks it.
func loadPatternCorpusPublicationForPrivacy(brainDir string) (patternCorpusPublication, os.FileInfo, bool, error) {
	info, present, err := privacyArtifactInfo(brainDir, patternCorpusPublicationMarkerPath, "pattern corpus publication marker")
	if err != nil {
		return patternCorpusPublication{}, nil, present, newPatternCorpusPublicationLoadError(err, 0)
	}
	if !present {
		return patternCorpusPublication{}, nil, false, nil
	}
	if info.Size() < 1 || info.Size() > patternCorpusPublicationMarkerMax {
		return patternCorpusPublication{}, nil, true, newPatternCorpusPublicationLoadError(
			fmt.Errorf("%s: pattern corpus publication marker is outside its safe size bounds", memoryErrStateUnsafe), 0)
	}
	data, stillPresent, err := readMemoryStateFileExpected(
		brainDir, patternCorpusPublicationMarkerPath, "pattern corpus publication marker", patternCorpusPublicationMarkerMax, info)
	if err != nil {
		return patternCorpusPublication{}, nil, true, newPatternCorpusPublicationLoadError(err, 0)
	}
	if !stillPresent {
		return patternCorpusPublication{}, nil, true, newPatternCorpusPublicationLoadError(
			fmt.Errorf("%s: pattern corpus publication marker disappeared during inventory", memoryErrStateUnsafe), 0)
	}
	publication, err := decodePatternCorpusPublication(data)
	if err != nil {
		return patternCorpusPublication{}, nil, true, err
	}
	return publication, info, true, nil
}

// removePatternCorpusPublicationForPrivacy removes only a supported current
// marker and only the exact identity whose bytes were classified. Unknown,
// corrupt, raced, or unsafe state remains byte-for-byte untouched.
func removePatternCorpusPublicationForPrivacy(brainDir string) error {
	_, info, present, err := loadPatternCorpusPublicationForPrivacy(brainDir)
	if err != nil || !present {
		return err
	}
	if err := removeBrainRelativeFileExpected(brainDir, patternCorpusPublicationMarkerPath, info); err != nil {
		return newPatternCorpusPublicationLoadError(err, patternCorpusPublicationVersion)
	}
	return nil
}
