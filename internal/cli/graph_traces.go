package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/entireio/entire-brain/internal/cli/internal/cmdutil"
	"github.com/entireio/entire-brain/internal/cli/internal/semstore"
	"path/filepath"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/spf13/cobra"
)

func writeGraphTraces(brainDir string, rows []semstore.TraceObservation) error {
	if err := base.RejectExistingSymlinkPathComponents(brainDir, semstore.TraceFile); err != nil {
		return err
	}
	data, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	if len(data)+1 > semstore.TraceMaxBytes {
		return errors.New("runtime trace observations exceed 64 MiB; existing observations retained")
	}
	return brainstore.WriteFileAtomic(filepath.Join(brainDir, semstore.TraceFile), append(data, '\n'), 0600)
}

func ingestGraphTraces(cmd *cobra.Command, opts Options, ingestOpts semanticTraceIngestOptions, path string) error {
	traces, err := readRuntimeTraces(path)
	if err != nil {
		return err
	}
	env, err := loadSemanticEnv(cmd, opts)
	if err != nil {
		return err
	}
	unlock, err := acquireSemanticIndexLock(env.BrainDir)
	if err != nil {
		return err
	}
	defer unlock()
	if err = requireCurrentBrainLayout(env.BrainDir); err != nil {
		return err
	}
	existing, err := semstore.ReadTraces(env.BrainDir)
	if err != nil {
		return err
	}
	handle, err := semanticStorePath(env)
	if err != nil {
		return err
	}
	db, err := semstore.OpenReadDB(handle)
	if err != nil {
		return err
	}
	defer db.Close()
	report := semanticTraceIngestReport{ImportedAt: opts.Now().UTC(), Path: path, Total: len(traces)}
	for _, t := range traces {
		matched, err := runtimeTraceMatchesStaticEdge(db, t)
		if err != nil {
			return err
		}
		n := 0
		if matched {
			n = 1
			report.Matched++
		}
		existing = append(existing, semstore.TraceObservation{ImportedAt: report.ImportedAt.Format(time.RFC3339Nano), SourcePath: semstore.TraceFile, FromID: t.From, ToID: t.To, ObservedType: t.Type, Matched: n})
	}
	report.Unmatched = report.Total - report.Matched
	if err = writeGraphTraces(env.BrainDir, existing); err != nil {
		return err
	}
	report.Path = semstore.TraceFile
	if ingestOpts.json {
		return cmdutil.WriteJSON(cmd, report)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "ingested runtime traces: %d\nmatched_static_edges: %d\nartifact: %s\n", report.Total, report.Matched, report.Path)
	return nil
}
