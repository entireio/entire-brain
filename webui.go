package entirebrain

import "embed"

// WebUI holds the embedded static assets for the `entire brain viz` local web
// interface (webui/dist). It lives at the module root because go:embed cannot
// reference parent directories, mirroring Templates and EmbedModel — so
// internal/cli serves it via entirebrain.WebUI. The assets are hand-authored and
// fully self-contained (no CDN); they are served from a loopback-only server, so
// the plugin stays a single static binary with no network dependency.
//
//go:embed webui/dist
var WebUI embed.FS
