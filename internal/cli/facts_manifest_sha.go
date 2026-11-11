package cli

// evalBrainManifestSHA256 stamps eval summaries and `memories status` output
// with the manifest content hash, so runs can be matched to the exact store
// state. Untagged: `memories status` ships in release builds (COR-2322).

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"

	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
)

func fileSHA256Hex(path string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func evalBrainManifestSHA256(brainDir string) string {
	return fileSHA256Hex(filepath.Join(brainDir, bmanifest.FileName))
}
