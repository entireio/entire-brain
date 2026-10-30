// Package cmdutil holds command-level helpers shared by the per-command
// packages of the internal/cli tree: output rendering today, with command
// plumbing joining it as the command-assembly split proceeds.
package cmdutil

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

func WriteJSON(cmd *cobra.Command, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	n, err := fmt.Fprintln(cmd.OutOrStdout(), string(data))
	if err == nil && n != len(data)+1 {
		return io.ErrShortWrite
	}
	return err
}

func WriteText(cmd *cobra.Command, render func(io.Writer)) error {
	// Stream directly to stdout instead of buffering the whole rendered body:
	// large multi-get/fact output must not be held in memory in full. The
	// sticky writer short-circuits after the first failure and preserves the
	// error, so callers still skip receipt recording when output fails.
	return WriteTextToWriter(cmd.OutOrStdout(), render)
}

func WriteTextToWriter(writer io.Writer, render func(io.Writer)) error {
	out := &StickyErrorWriter{Writer: writer}
	render(out)
	return out.Err
}

func JSONOutputBytes(value any) ([]byte, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// ValueOrUnset renders an optional string field for human output, making an
// absent value explicit instead of printing an empty gap.
func ValueOrUnset(value string) string {
	if value == "" {
		return "<unset>"
	}
	return value
}

// StickyErrorWriter short-circuits after the first write failure and retains
// the error, so streamed renderers can report output failure once at the end.
type StickyErrorWriter struct {
	Writer io.Writer
	Err    error
}

func (w *StickyErrorWriter) Write(p []byte) (int, error) {
	if w.Err != nil {
		return 0, w.Err
	}
	n, err := w.Writer.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	if err != nil {
		w.Err = err
	}
	return n, err
}
