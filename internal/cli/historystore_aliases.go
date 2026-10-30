package cli

import (
	"github.com/entireio/entire-brain/internal/cli/internal/historystore"
)

// Aliases for the history-store types that sank into internal/historystore
// (COR-2268). They keep the hundreds of existing cli references compiling
// without a shim; new code should use the historystore names directly.
type (
	historyIndex          = historystore.Index
	historyRecord         = historystore.Record
	historyLegacyIdentity = historystore.LegacyIdentity
)
