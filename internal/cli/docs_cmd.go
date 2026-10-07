package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// `entire brain docs` — answering "will this document be read, and as what?"
//
// Document ingest fails quietly by nature. A PDF that is a scan, a spreadsheet
// whose text lives in a part we skipped, a file with an extension this build
// does not handle: in every case the refresh succeeds, the document is absent,
// and nothing points at why. These two commands make that visible without
// running a refresh and reading the brain directory afterwards.

func newDocsCommand(opts Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "docs",
		Short: "Inspect how non-code documents are read into the brain",
		Long: strings.TrimSpace(`
PDF, Word, Excel and PowerPoint files under docs/ are read as text during
refresh and indexed alongside markdown, so an agent can retrieve a design doc
that was written in Word or a spec that arrived as a PDF.

Extraction is deterministic and local: no model, no network, no external tools.
Scanned PDFs have no text layer and are reported rather than indexed as empty.`),
	}
	cmd.AddCommand(newDocsExtractCommand(), newDocsFormatsCommand())
	return cmd
}

func newDocsExtractCommand() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "extract <file>",
		Short: "Print the text this build reads out of one document",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := args[0]
			// Through the same bounded, regular-file-checked read the seed
			// path uses for caller-supplied documents. os.Stat follows
			// symlinks and says nothing about FIFOs, and a plain ReadFile on
			// one blocks forever with no writer; an oversized file is refused
			// on its stated size rather than after paying for it.
			data, err := safeReadFile(path, maxExtractDocumentBytes)
			if err != nil {
				return err
			}
			text, extractErr := extractDocumentText(path, data)
			if jsonOut {
				payload := map[string]any{
					"path":        path,
					"format":      string(documentFormatFor(path)),
					"source_size": int64(len(data)),
					"extracted":   extractErr == nil,
				}
				if extractErr != nil {
					payload["error"] = extractErr.Error()
					var docErr *documentExtractionError
					if errors.As(extractErr, &docErr) {
						payload["reason"] = docErr.Reason
						if docErr.Remedy != "" {
							payload["remedy"] = docErr.Remedy
						}
					}
				} else {
					payload["characters"] = len(text)
					payload["text"] = text
				}
				return writeJSON(cmd, payload)
			}
			if extractErr != nil {
				return extractErr
			}
			// The header goes to stderr so the text can be piped somewhere
			// without it: `docs extract spec.pdf > spec.txt` should produce the
			// document, not the document plus a banner.
			fmt.Fprintf(cmd.ErrOrStderr(), "%s: %d characters from %d bytes\n", path, len(text), len(data))
			fmt.Fprintln(cmd.OutOrStdout(), text)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit the result as JSON")
	return cmd
}

func newDocsFormatsCommand() *cobra.Command {
	return &cobra.Command{
		Hidden: true,
		Use:    "formats",
		Short:  "List the document formats this build reads",
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			for _, format := range []struct{ ext, note string }{
				{".pdf", "text layer only; a scan needs OCR, which this build does not do"},
				{".docx", "body, footnotes and endnotes; headers and footers are skipped as repetition"},
				{".xlsx", "every sheet, labelled with its name; shared strings resolved"},
				{".pptx", "slides in deck order, then speaker notes"},
			} {
				fmt.Fprintf(out, "%-7s %s\n", format.ext, format.note)
			}
			fmt.Fprintf(out, "\nRead from docs/ during refresh, the same places markdown is taken from.\n")
			fmt.Fprintf(out, "Legacy .doc/.xls/.ppt are a different format and are not read.\n")
			return nil
		},
	}
}
