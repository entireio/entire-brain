package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/entire-brain/internal/agentsetup"
)

// MergePreEditHookForTest keeps the cross-package call in one place.
func MergePreEditHookForTest(root, brainCmd string) (bool, error) {
	return agentsetup.MergePreEditHook(root, brainCmd)
}

func readSettingsForTest(t *testing.T, root string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	return string(data)
}
