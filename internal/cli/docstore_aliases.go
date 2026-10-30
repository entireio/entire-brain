package cli

import "github.com/entireio/entire-brain/internal/cli/internal/docstore"

// Type aliases for the doc-store read layer extracted to docstore: struct
// identity and JSON wire shapes are unchanged, the cli call sites keep their
// old lowercase names.
type (
	docIndex        = docstore.Index
	docRecord       = docstore.Record
	scoredDocRecord = docstore.ScoredRecord
)
