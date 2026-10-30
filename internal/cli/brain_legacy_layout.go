package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
)

var errLegacyBrainLayout = errors.New("this brain uses an old layout")

// Old code stores can contain authored runtime traces. Detect them without
// opening, migrating, or deleting them; 0.2.0 remains the migration bridge.
func checkLegacyBrainLayout(brainDir string, manifest *exportManifest) error {
	legacy := manifest != nil && manifest.Sources != nil && manifest.Sources.Semantic != nil && manifest.Sources.Semantic.StoreOwner != "graph"
	for _, rel := range []string{"semantic/generations", "semantic/snapshots"} {
		if err := base.RejectExistingSymlinkPathComponents(brainDir, rel); err != nil {
			return err
		}
		if _, err := os.Lstat(filepath.Join(brainDir, rel)); err == nil {
			legacy = true
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("check old brain layout: %w", err)
		}
	}
	if legacy {
		return fmt.Errorf("%w; run `%s` with entire-brain 0.2.0 once to preserve runtime traces, then upgrade. Or back up and remove the brain directory, then run `%s` to rebuild. Brain directory: %s", errLegacyBrainLayout, base.BrainInvocation(os.LookupEnv, "refresh index"), base.BrainInvocation(os.LookupEnv, "refresh"), brainDir)
	}
	return nil
}

func requireCurrentBrainLayout(brainDir string) error {
	manifest, err := bmanifest.Load(brainDir)
	if err != nil {
		return err
	}
	return checkLegacyBrainLayout(brainDir, manifest)
}
