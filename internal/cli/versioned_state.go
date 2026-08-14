package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// checkedVersionedJSONHeader deliberately decodes only the version before a
// strict supported-schema decode. A newer producer is allowed to add fields we
// do not know about; those bytes are read-only, not "corrupt" disposable state.
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

func decodeStrictVersionedJSON(data []byte, out any, current int, label string) (int, error) {
	version, err := checkedVersionedJSONHeader(data, current, label)
	if err != nil {
		return version, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return version, fmt.Errorf("%s: %s cannot be parsed: %w", memoryErrStateCorrupt, label, err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return version, fmt.Errorf("%s: %s contains trailing JSON data", memoryErrStateCorrupt, label)
	}
	return version, nil
}

// removeCheckedMemoryStateFile removes only the same regular file identity
// that was inspected and accepted by validate. Brain writers serialize on the
// Brain lock; the identity checks also keep unsafe aliases from turning an
// authorized cleanup into an unlink of unclassified state.
func removeCheckedMemoryStateFile(brainDir, rel, label string, maxBytes int64, validate func([]byte) error) error {
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
	data, present, err := readMemoryStateFileExpected(brainDir, rel, label, maxBytes, before)
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	if err := validate(data); err != nil {
		return err
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
