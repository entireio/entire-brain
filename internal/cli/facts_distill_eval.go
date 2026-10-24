package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// facts_distill_eval.go is the command tree and shared types for the
// memory-GENERATION eval (does a distilled fact deserve to exist?), scored on
// raw pre-reconcile extraction output. The facts_eval* family next door scores
// RETRIEVAL (which stored facts surface for a query); the two share agent
// plumbing but not metrics. Design: .context/2026-10-23-memory-generation-eval-loop-design.md.

type distillEvalCorpus struct {
	RepoKey      string                   `json:"repo_key"`
	CreatedAt    time.Time                `json:"created_at"`
	CorpusSHA256 string                   `json:"corpus_sha256"`
	Sessions     []distillEvalCorpusEntry `json:"sessions"`
}

type distillEvalCorpusEntry struct {
	SessionID      string `json:"session_id"`
	Branch         string `json:"branch"`
	TranscriptPath string `json:"transcript_path"`
	RawBytes       int    `json:"raw_bytes"`
	Chunks         int    `json:"chunks"`
	Golden         bool   `json:"golden"`
}

type distillEvalRunMeta struct {
	Variant      distillEvalVariant `json:"variant"`
	CorpusSHA256 string             `json:"corpus_sha256"`
	// GoldenOnly records that the run covered only the corpus's golden
	// sessions; score must not count the rest as legitimately silent.
	GoldenOnly  bool      `json:"golden_only,omitempty"`
	StartedAt   time.Time `json:"started_at"`
	WallSeconds float64   `json:"wall_seconds"`
}

type distillEvalVariant struct {
	Name           string `json:"name"`
	TemplateSHA256 string `json:"template_sha256"`
	Agent          string `json:"agent"`
	Model          string `json:"model"`
	MaxChunkBytes  int    `json:"max_chunk_bytes"`
	CapPerChunk    int    `json:"cap_per_chunk"`
}

// distillEvalCorpusSHA fingerprints the session id SET: sha256 of the sorted
// ids joined by newlines. Compare refuses runs whose fingerprints differ.
func distillEvalCorpusSHA(ids []string) string {
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	sum := sha256.Sum256([]byte(strings.Join(sorted, "\n")))
	return hex.EncodeToString(sum[:])
}

func newFactsDistillEvalCommand(opts Options) *cobra.Command {
	cmd := &cobra.Command{
		Hidden: true,
		Use:    "distill-eval",
		Short:  "Offline eval of memory-generation quality (raw extraction output)",
	}
	cmd.AddCommand(newDistillEvalCorpusCommand(opts))
	cmd.AddCommand(newDistillEvalRunCommand(opts))
	cmd.AddCommand(newDistillEvalLabelsCommand(opts))
	cmd.AddCommand(newDistillEvalJudgeCommand(opts))
	cmd.AddCommand(newDistillEvalCalibrateCommand(opts))
	cmd.AddCommand(newDistillEvalScoreCommand(opts))
	cmd.AddCommand(newDistillEvalCompareCommand(opts))
	return cmd
}
