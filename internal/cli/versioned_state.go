package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

// errTrailingJSONData marks a document followed by a second JSON value. Callers
// classify it themselves so each keeps its own state-specific wording.
var errTrailingJSONData = errors.New("trailing JSON data")

// unknownFieldErrPrefix is how encoding/json reports a field the target does not
// declare. The package exposes no typed error for it, so this text is the only
// available discriminator; TestUnknownFieldErrorTextIsStillStdlibContract pins it
// against the real decoder so a stdlib rewording fails the build here rather than
// silently restoring the brick this guards against.
const unknownFieldErrPrefix = "json: unknown field "

// HasPrefix, not Contains: the stdlib returns this error unwrapped at every
// nesting depth, and matching only the prefix fails closed. If a future stdlib
// ever wraps it, this stops recognising it and the state is called corrupt again
// -- loud and caught by the pin test -- rather than silently tolerating an error
// that merely happens to quote this text.
func isUnknownFieldError(err error) bool {
	return err != nil && strings.HasPrefix(err.Error(), unknownFieldErrPrefix)
}

// checkedVersionedJSONHeader deliberately decodes only the version before the
// supported-schema decode in decodeVersionedJSONBody. A newer producer is
// allowed to add fields we do not know about; those bytes are read-only, not
// "corrupt" disposable state.
func checkedVersionedJSONHeader(data []byte, current int, label string) (int, error) {
	var header struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return 0, fmt.Errorf("%s: %s cannot be parsed", memoryErrStateCorrupt, label)
	}
	if header.SchemaVersion > current {
		return header.SchemaVersion, fmt.Errorf("%s: %s schema version %d is newer than supported version %d", memoryErrUnsupportedVersion, label, header.SchemaVersion, current)
	}
	return header.SchemaVersion, nil
}

// decodeVersionedJSONBody decodes exactly one JSON document into out. Malformed
// bytes and trailing data are always rejected.
//
// tolerateUnknownFields decides what a field out does not declare means, and the
// answer differs by caller, so every caller states it:
//
//   - false, for a decode that guards a rewrite of the same state. A field this
//     build cannot round-trip must stop the write, or the writer silently erases
//     what another build recorded. This is the invariant the forward-state safety
//     tests pin, and the default for anything that writes.
//   - true, for a decode that only reads. Fields are added and retired without a
//     schema bump, so an unknown field there is routine skew between builds, not
//     corruption; every field this build does know is still good. Classifying it
//     corrupt disabled every brain source and left the memory worker failing on
//     its retry loop indefinitely. Package brainwire already requires this of
//     readers ("Readers MUST be tolerant").
//
// Neither mode accepts a newer *schema*; the caller's version check rejects that
// first, before any of this runs.
//
// The returned flag says whether unknown fields were dropped, so a reader can
// tell the operator its state was written by a different build instead of
// narrowing it silently.
func decodeVersionedJSONBody(data []byte, out any, tolerateUnknownFields bool) (droppedUnknownFields bool, err error) {
	err = decodeOneJSONValue(data, out, true)
	if err == nil || !tolerateUnknownFields || !isUnknownFieldError(err) {
		return false, err
	}
	// The strict pass abandons the document where it failed, so out holds a
	// partial value. Clear it rather than merging two decodes into one target.
	if err := resetDecodeTarget(out); err != nil {
		return false, err
	}
	if err := decodeOneJSONValue(data, out, false); err != nil {
		return false, err
	}
	return true, nil
}

func decodeOneJSONValue(data []byte, out any, strict bool) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if strict {
		decoder.DisallowUnknownFields()
	}
	if err := decoder.Decode(out); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errTrailingJSONData
	}
	return nil
}

func resetDecodeTarget(out any) error {
	value := reflect.ValueOf(out)
	if value.Kind() != reflect.Pointer || value.IsNil() {
		return fmt.Errorf("decode target must be a non-nil pointer, got %T", out)
	}
	value.Elem().Set(reflect.Zero(value.Elem().Type()))
	return nil
}

func decodeStrictVersionedJSON(data []byte, out any, current int, label string) (int, error) {
	version, err := checkedVersionedJSONHeader(data, current, label)
	if err != nil {
		return version, err
	}
	if _, err := decodeVersionedJSONBody(data, out, false); err != nil {
		if errors.Is(err, errTrailingJSONData) {
			return version, fmt.Errorf("%s: %s contains trailing JSON data", memoryErrStateCorrupt, label)
		}
		return version, fmt.Errorf("%s: %s cannot be parsed: %w", memoryErrStateCorrupt, label, err)
	}
	return version, nil
}

// removeCheckedMemoryStateFile removes only the same regular file identity
// that was inspected and accepted by validate. Brain writers serialize on the
// Brain lock; the identity checks also keep unsafe aliases from turning an
// authorized cleanup into an unlink of unclassified state.
func removeCheckedMemoryStateFile(brainDir, rel, label string, maxBytes int64, validate func([]byte) error) error {
	return removeCheckedMemoryStateFileValidated(brainDir, rel, label, func(before os.FileInfo) (bool, error) {
		data, present, err := readMemoryStateFileExpected(brainDir, rel, label, maxBytes, before)
		if err != nil || !present {
			return present, err
		}
		return true, validate(data)
	})
}

func removeCheckedMemoryStateFileValidated(brainDir, rel, label string, validate func(os.FileInfo) (bool, error)) error {
	clean, err := cleanBrainRelativePath(rel)
	if err != nil {
		return fmt.Errorf("%s: %s has an unsafe path: %w", memoryErrStateUnsafe, label, err)
	}
	path := filepath.Join(brainDir, clean)
	before, err := memoryStateLstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("%s: inspect %s: %w", memoryErrStateCorrupt, label, err)
	}
	present, err := validate(before)
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	after, err := memoryStateLstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("%s: re-inspect %s: %w", memoryErrStateCorrupt, label, err)
	}
	if after.Mode()&os.ModeSymlink != 0 || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		return fmt.Errorf("%s: %s changed before removal", memoryErrStateUnsafe, label)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
