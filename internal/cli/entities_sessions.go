package cli

import (
	"context"
	"strings"

	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
)

// sessionBranchByID maps a session id to the branch the brain exported it on.
func sessionBranchByID(ctx context.Context, opts Options, repoDir string) map[string]string {
	out := map[string]string{}
	storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return out
	}
	manifest, err := bmanifest.Load(storage.BrainDir)
	if err != nil || manifest == nil {
		return out
	}
	sessions := manifest.Sessions
	if manifest.Sources != nil && manifest.Sources.Sessions != nil && len(manifest.Sources.Sessions.Sessions) > 0 {
		sessions = manifest.Sources.Sessions.Sessions
	}
	for _, session := range sessions {
		id := strings.TrimSpace(session.SessionID)
		if id == "" {
			continue
		}
		if branch := strings.TrimSpace(resolveDistillBranch(manifest, session)); branch != "" {
			out[id] = branch
		}
	}
	return out
}
