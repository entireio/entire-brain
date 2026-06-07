package entirebrain

import _ "embed"

// EmbedModel is the bundled Model2Vec static embedding table, converted by
// scripts/convert_embedmodel.py. It is embedded so the brain's semantic recall
// runs fully offline in a single static binary, with no cgo, ONNX runtime, or
// network fetch. The decode + inference path is pure Go in internal/cli/embed.go.
// It lives at the module root because go:embed cannot reference parent
// directories from internal/cli (same constraint as Templates).
//
//go:embed assets/embedmodel.bin
var EmbedModel []byte
