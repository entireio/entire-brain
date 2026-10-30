package cmdutil

import (
	"fmt"
	"io"

	"github.com/entireio/entire-brain/internal/tui"
)

// RenderHostedBanner is the one shape every hosted connect/disconnect message
// uses: a blank line, the done mark with a bold headline, then each paragraph
// dimmed and blank-line separated. Everything is left-aligned — no indentation.
func RenderHostedBanner(out io.Writer, headline string, paragraphs ...string) {
	render := tui.NewRenderer(out)
	fmt.Fprintf(out, "\n%s %s\n", render.Mark(tui.MarkDone), render.Bold(headline))
	for _, paragraph := range paragraphs {
		fmt.Fprintf(out, "\n%s\n", render.Dim(paragraph))
	}
}
