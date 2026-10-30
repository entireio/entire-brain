package cli

import (
	"github.com/entireio/entire-brain/internal/cli/command/stats"
	"github.com/entireio/entire-brain/internal/cli/internal/historystore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
)

// statsConfig binds target resolution, repo storage paths, the conversation
// vector status, the short-term overlay reader, and the history index
// versions at the assembly boundary. Command packages never import cli, and
// no mutable hook is copied.
func statsConfig(opts Options) stats.Config {
	return stats.Config{Options: opts, Deps: statsDeps()}
}

// statsDeps is separate from statsConfig because stats.BuildReport takes only
// the seams, not the full command config (conversation_phase2_test.go builds
// reports directly).
func statsDeps() stats.Deps {
	return stats.Deps{
		AgentSurfaceTarget: agentSurfaceTarget,
		RepoStoragePaths:   repoStoragePaths,
		ConversationVectorStatus: func(brainDir string, manifest *bmanifest.Manifest) (string, string, int, bool) {
			status := buildConversationStatus(brainDir, manifest)
			if status == nil {
				return "", "", 0, false
			}
			return status.VectorState, status.VectorModelID, status.Vectors, true
		},
		LoadShortTermOverlay: func(brainDir string, source *bmanifest.HistorySource) stats.ShortTermOverlay {
			overlay, state := loadHistoryShortTermState(brainDir, source)
			files := make(map[string][]historystore.Record, len(overlay.Files))
			for path, entry := range overlay.Files {
				files[path] = entry.Records
			}
			return stats.ShortTermOverlay{
				State:       state,
				Absent:      state == shortTermStateAbsent,
				Current:     state == shortTermStateCurrent,
				Complete:    overlay.complete(),
				Files:       files,
				GeneratedAt: overlay.GeneratedAt,
				Truncated:   overlay.Truncated,
				FailedFiles: overlay.FailedFiles,
			}
		},
		ScanCacheVersion: historyScanCacheVersion,
		FTSSchema:        historyFTSSchema,
		ConversationKind: conversationKind,
	}
}
