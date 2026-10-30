package retrieval

import (
	"testing"
)

// TestLocusTokenIsFilePathSeesHyphenatedPaths closes the loop on drift: the
// fragments were invisible to FactLocusDrift ("website/scripts/pre" has no
// extension, "build.ts" has no slash), so a hyphenated file could never be
// reported as drifted no matter how long ago it was deleted.
func TestLocusTokenIsFilePathSeesHyphenatedPaths(t *testing.T) {
	if !locusTokenIsFilePath("website/scripts/pre-build.ts") {
		t.Error("a hyphenated path with a real extension must count as a file path")
	}
	if locusTokenIsFilePath("website/scripts/pre") {
		t.Error("an extensionless fragment must not count as a file path")
	}
}
