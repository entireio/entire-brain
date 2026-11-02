package brainstore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
)

// ErrTrailingJSONData marks a document followed by a second JSON value. Callers
// classify it themselves so each keeps its own state-specific wording.
var ErrTrailingJSONData = errors.New("trailing JSON data")

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
func IsUnknownFieldError(err error) bool {
	return err != nil && strings.HasPrefix(err.Error(), unknownFieldErrPrefix)
}

// CheckedVersionedJSONHeader deliberately decodes only the version before the
// supported-schema decode in DecodeVersionedJSONBody. A newer producer is
// allowed to add fields we do not know about; those bytes are read-only, not
// "corrupt" disposable state.
func CheckedVersionedJSONHeader(data []byte, current int, label string) (int, error) {
	var header struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return 0, fmt.Errorf("%s: %s cannot be parsed", StateCorruptCode, label)
	}
	if header.SchemaVersion > current {
		return header.SchemaVersion, fmt.Errorf("%s: %s schema version %d is newer than supported version %d — a newer entire-brain wrote it and this one leaves it untouched; upgrade with `entire plugin upgrade brain`", UnsupportedVersionCode, label, header.SchemaVersion, current)
	}
	return header.SchemaVersion, nil
}

// DecodeVersionedJSONBody decodes exactly one JSON document into out. Malformed
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
//     its retry loop indefinitely. The wire contracts require the same of
//     readers ("Readers MUST be tolerant").
//
// Neither mode accepts a newer *schema*; the caller's version check rejects that
// first, before any of this runs.
//
// The returned flag says whether unknown fields were dropped, so a reader can
// tell the operator its state was written by a different build instead of
// narrowing it silently.
func DecodeVersionedJSONBody(data []byte, out any, tolerateUnknownFields bool) (droppedUnknownFields bool, err error) {
	err = decodeOneJSONValue(data, out, true)
	if err == nil || !tolerateUnknownFields || !IsUnknownFieldError(err) {
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
		return ErrTrailingJSONData
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

func DecodeStrictVersionedJSON(data []byte, out any, current int, label string) (int, error) {
	version, err := CheckedVersionedJSONHeader(data, current, label)
	if err != nil {
		return version, err
	}
	if _, err := DecodeVersionedJSONBody(data, out, false); err != nil {
		if errors.Is(err, ErrTrailingJSONData) {
			return version, fmt.Errorf("%s: %s contains trailing JSON data", StateCorruptCode, label)
		}
		return version, fmt.Errorf("%s: %s cannot be parsed: %w", StateCorruptCode, label, err)
	}
	return version, nil
}
