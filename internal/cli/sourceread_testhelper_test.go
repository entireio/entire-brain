package cli

import (
	"os"
	"strings"
	"testing"
)

// readSourceForTest reads a file from this package, failing loudly rather than
// letting a guard pass vacuously on a missing file.
func readSourceForTest(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	if len(data) == 0 {
		t.Fatalf("%s is empty; any assertion over it would be vacuous", name)
	}
	return string(data)
}

// functionBodyForTest returns the text from a function's declaration to the
// next top-level closing brace. Crude, and sufficient for pinning a shape.
func functionBodyForTest(t *testing.T, src, name string) string {
	t.Helper()
	i := strings.Index(src, "func "+name+"(")
	if i < 0 {
		return ""
	}
	rest := src[i:]
	if j := strings.Index(rest, "\n}\n"); j >= 0 {
		return rest[:j]
	}
	return rest
}
