package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

// distillEvalLabelLine is one row of the golden label sheet: generated with an
// empty verdict, hand-filled by the owner with "keep" or "kill".
type distillEvalLabelLine struct {
	FactID         string   `json:"fact_id"`
	SessionID      string   `json:"session_id"`
	TranscriptPath string   `json:"transcript_path"`
	Kind           string   `json:"kind"`
	Paths          []string `json:"paths"`
	Text           string   `json:"text"`
	Verdict        string   `json:"verdict"`
	Reason         string   `json:"reason"`
}

type distillEvalVerdict struct {
	Verdict string // "keep" | "kill"
	Reason  string
}

// loadDistillEvalLabels reads a filled label sheet keyed by fact_id. Any
// verdict other than exactly "keep" or "kill" fails with its 1-based line
// number, so an unfilled or typoed sheet never silently scores as empty.
func loadDistillEvalLabels(path string) (map[string]distillEvalVerdict, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	labels := map[string]distillEvalVerdict{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		if len(sc.Bytes()) == 0 {
			continue
		}
		var line distillEvalLabelLine
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, lineNo, err)
		}
		if line.Verdict != "keep" && line.Verdict != "kill" {
			return nil, fmt.Errorf("%s line %d: verdict must be exactly \"keep\" or \"kill\", got %q", path, lineNo, line.Verdict)
		}
		if line.FactID == "" {
			return nil, fmt.Errorf("%s line %d: missing fact_id", path, lineNo)
		}
		labels[line.FactID] = distillEvalVerdict{Verdict: line.Verdict, Reason: line.Reason}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return labels, nil
}

// readDistillEvalRawFacts loads the fact lines (type "fact") of a run's
// raw-facts.jsonl, plus the chunk lines (type "chunk") for callers that need
// them.
func readDistillEvalRawFacts(path string) (chunks []distillRawChunkLine, facts []distillRawFactLine, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var probe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(sc.Bytes(), &probe); err != nil {
			return nil, nil, fmt.Errorf("%s: %w", path, err)
		}
		switch probe.Type {
		case "chunk":
			var c distillRawChunkLine
			if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
				return nil, nil, err
			}
			chunks = append(chunks, c)
		case "fact":
			var fl distillRawFactLine
			if err := json.Unmarshal(sc.Bytes(), &fl); err != nil {
				return nil, nil, err
			}
			facts = append(facts, fl)
		}
	}
	return chunks, facts, sc.Err()
}

func loadDistillEvalCorpusFile(path string) (distillEvalCorpus, error) {
	var corpus distillEvalCorpus
	data, err := os.ReadFile(path)
	if err != nil {
		return corpus, fmt.Errorf("read corpus %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &corpus); err != nil {
		return corpus, fmt.Errorf("decode corpus %s: %w", path, err)
	}
	return corpus, nil
}

func newDistillEvalLabelsCommand(opts Options) *cobra.Command {
	var (
		runDir     string
		corpusPath string
		out        string
	)
	cmd := &cobra.Command{
		Use:   "labels",
		Short: "Generate the golden label sheet (keep/kill, hand-filled) from a run",
		RunE: func(cmd *cobra.Command, args []string) error {
			corpus, err := loadDistillEvalCorpusFile(corpusPath)
			if err != nil {
				return err
			}
			golden := map[string]distillEvalCorpusEntry{}
			for _, s := range corpus.Sessions {
				if s.Golden {
					golden[s.SessionID] = s
				}
			}
			if len(golden) == 0 {
				return errors.New("corpus has no golden sessions")
			}
			_, facts, err := readDistillEvalRawFacts(filepath.Join(runDir, "raw-facts.jsonl"))
			if err != nil {
				return err
			}
			if out == "" {
				out = filepath.Join(runDir, "labels.jsonl")
			}
			f, err := os.Create(out)
			if err != nil {
				return err
			}
			defer f.Close()
			enc := json.NewEncoder(f)
			rows := 0
			for _, fact := range facts {
				entry, ok := golden[fact.SessionID]
				if !ok {
					continue
				}
				rows++
				if err := enc.Encode(distillEvalLabelLine{
					FactID: fact.FactID, SessionID: fact.SessionID,
					TranscriptPath: entry.TranscriptPath,
					Kind:           fact.Kind, Paths: fact.Paths, Text: fact.Text,
				}); err != nil {
					return err
				}
			}
			fmt.Fprintf(cmd.OutOrStdout(), "label sheet: %d facts from %d golden sessions → %s\nfill each verdict with keep or kill, then score with --labels\n", rows, len(golden), out)
			return nil
		},
	}
	cmd.Flags().StringVar(&runDir, "run", "", "Run directory containing raw-facts.jsonl (required)")
	cmd.Flags().StringVar(&corpusPath, "corpus", "", "Path to corpus.json (required)")
	cmd.Flags().StringVar(&out, "out", "", "Sheet path (default <run>/labels.jsonl)")
	_ = cmd.MarkFlagRequired("run")
	_ = cmd.MarkFlagRequired("corpus")
	return cmd
}
