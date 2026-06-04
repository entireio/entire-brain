// Package entirebrain exposes repository assets that ship with the binary.
//
// The prompt templates under templates/ are the single source of truth for the
// agent-facing gates (intake and distillation). They live at the module root so
// they can be embedded here and reused both by the Go binary and by external
// skill installers that read the files directly. internal/cli cannot embed them
// itself because go:embed cannot reference parent directories.
package entirebrain

import "embed"

// Templates holds the embedded prompt templates (templates/*.md).
//
//go:embed templates/*.md
var Templates embed.FS
