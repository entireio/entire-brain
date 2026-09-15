package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// readBrainManifestFile decodes the canonical manifest without a byte ceiling.
// It uses the same checked descriptor as bounded state readers, without needing
// the source repository or Git. encoding/json buffers a whole JSON value and
// the manifest is materialized in memory: this is NOT a bounded-memory reader.
// Rewinding this descriptor (never reopening the path) preserves schema-first
// checks and tolerant reads versus strict rewrites, including unknown fields.
func readBrainManifestFile(brainDir string, tolerateUnknown bool) (*exportManifest, int, bool, bool, error) {
	f, present, err := openMemoryStateFileExpected(brainDir, exportManifestFileName, "brain manifest", nil)
	if err != nil {
		return nil, 0, false, present, fmt.Errorf("read brain manifest: %w", err)
	}
	if !present {
		return &exportManifest{SchemaVersion: brainManifestSchemaVersion}, 0, false, false, nil
	}
	defer f.Close()
	manifest, version, dropped, err := decodeBrainManifestFile(f, tolerateUnknown)
	return manifest, version, dropped, true, err
}

func decodeManifestPass(f *os.File, out any, strict bool) error {
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
		return errTrailingJSONData
	}
	return nil
}

func decodeBrainManifestFile(f *os.File, tolerateUnknown bool) (*exportManifest, int, bool, error) {
	var header struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := decodeManifestPass(f, &header, false); err != nil {
		return nil, 0, false, fmt.Errorf("parse brain manifest: %s: brain manifest cannot be parsed: %w", memoryErrStateCorrupt, err)
	}
	version := header.SchemaVersion
	if version > brainManifestSchemaVersion {
		return nil, version, false, fmt.Errorf("%s: brain manifest schema version %d is newer than supported version %d", memoryErrUnsupportedVersion, version, brainManifestSchemaVersion)
	}
	var manifest exportManifest
	err := decodeManifestPass(f, &manifest, true)
	dropped := false
	if tolerateUnknown && isUnknownFieldError(err) {
		manifest = exportManifest{}
		err = decodeManifestPass(f, &manifest, false)
		dropped = err == nil
	}
	if err != nil {
		return nil, version, false, fmt.Errorf("parse brain manifest: %s: brain manifest cannot be parsed: %w", memoryErrStateCorrupt, err)
	}
	if version < brainManifestMinSchemaVersion {
		return nil, version, false, fmt.Errorf("%s: brain manifest declares schema version %d, older than the first supported version %d", memoryErrUnsupportedVersion, version, brainManifestMinSchemaVersion)
	}
	normalizeBrainManifest(&manifest)
	return &manifest, version, dropped, nil
}
