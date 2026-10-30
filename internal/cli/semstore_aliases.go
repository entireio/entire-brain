package cli

import "github.com/entireio/entire-brain/internal/cli/internal/semstore"

// The semantic snapshot/store read layer lives in internal/cli/internal/semstore
// (COR-2268). These aliases keep the package-local spellings compiling while
// command clusters move out; they retire with the cli package.
type (
	semanticRecord   = semstore.Record
	semanticEvidence = semstore.Evidence
)
