package patterns

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
)

func IsCorpusRollbackJournalRel(rel string) bool {
	rel = filepath.ToSlash(rel)
	for _, name := range CorpusSidecarNames {
		if strings.HasSuffix(name, "-journal") && rel == filepath.ToSlash(filepath.Join("patterns", name)) {
			return true
		}
	}
	return false
}

func IsCorpusStagingArtifactRel(rel string) bool {
	rel = filepath.ToSlash(rel)
	dir, name := filepath.ToSlash(filepath.Dir(rel)), filepath.Base(filepath.FromSlash(rel))
	return dir == "patterns" && strings.HasPrefix(name, CorpusStagingPrefix)
}

type PublicationReadState string

const (
	PublicationCorrupt     PublicationReadState = "corrupt"
	PublicationUnsafe      PublicationReadState = "unsafe"
	PublicationUnsupported PublicationReadState = "unsupported"
)

// PublicationLoadError keeps publication-marker failures
// machine-classifiable at privacy inventory and deletion boundaries. In
// particular, a newer producer's marker is unsupported read-only state, not a
// disposable corrupt leaf.
type PublicationLoadError struct {
	Code    string
	State   PublicationReadState
	Version int
	Err     error
}

func (e *PublicationLoadError) Error() string {
	detail := "pattern corpus publication marker is invalid"
	if e.Err != nil {
		detail = e.Err.Error()
	}
	if strings.HasPrefix(detail, e.Code+":") {
		return detail
	}
	return e.Code + ": " + detail
}

func (e *PublicationLoadError) Unwrap() error { return e.Err }

func newPublicationLoadError(err error, version int) error {
	code := brainstore.MemoryErrorCode(err)
	state := PublicationCorrupt
	switch code {
	case brainstore.UnsupportedVersionCode:
		state = PublicationUnsupported
	case brainstore.StateUnsafeCode:
		state = PublicationUnsafe
	default:
		code = brainstore.StateCorruptCode
	}
	return &PublicationLoadError{Code: code, State: state, Version: version, Err: err}
}

// DecodeCorpusPublication is the single classifier for publication
// markers at both recovery and privacy-cleanup boundaries. It classifies the
// version header before a strict current-schema decode so a future producer's
// additional fields remain opaque, read-only state.
func DecodeCorpusPublication(data []byte) (CorpusPublication, error) {
	var publication CorpusPublication
	version, err := brainstore.DecodeStrictVersionedJSON(data, &publication, CorpusPublicationVersion, "pattern corpus publication marker")
	if err != nil {
		return CorpusPublication{}, newPublicationLoadError(err, version)
	}
	decodedSHA, decodeErr := hex.DecodeString(publication.SHA256)
	if publication.SchemaVersion != CorpusPublicationVersion ||
		!strings.HasPrefix(publication.Candidate, CorpusStagingPrefix) ||
		filepath.Base(publication.Candidate) != publication.Candidate ||
		strings.ContainsAny(publication.Candidate, `/\`) ||
		decodeErr != nil || len(decodedSHA) != sha256.Size || publication.SHA256 != strings.ToLower(publication.SHA256) ||
		publication.Size < 1 || publication.Size > brainstore.PrivacySQLiteSnapshotMaxBytes {
		return CorpusPublication{}, newPublicationLoadError(
			fmt.Errorf("%s: invalid pattern corpus publication marker", brainstore.StateCorruptCode), publication.SchemaVersion)
	}
	return publication, nil
}
