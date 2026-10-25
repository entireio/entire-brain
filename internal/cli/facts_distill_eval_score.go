package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

type distillEvalSummary struct {
	Variant      distillEvalVariant         `json:"variant"`
	CorpusSHA256 string                     `json:"corpus_sha256"`
	GoldenOnly   bool                       `json:"golden_only,omitempty"`
	JudgeConfig  string                     `json:"judge_config,omitempty"`
	Sessions     []distillEvalSessionResult `json:"sessions"`
	Facts        int                        `json:"facts"`
	Precision    distillEvalPrecision       `json:"precision"`
	CapShare     float64                    `json:"cap_share"`
	Redundancy   distillEvalRedundancy      `json:"redundancy"`
	Cost         distillEvalCost            `json:"cost"`
	// IncompleteSessions lists sessions whose raw output is missing chunks or
	// fact lines (agent failures mid-run); their volume/cost read artificially
	// low. Populated only under --allow-incomplete — without it, score refuses.
	IncompleteSessions []string `json:"incomplete_sessions,omitempty"`
}

type distillEvalSessionResult struct {
	SessionID    string `json:"session_id"`
	Facts        int    `json:"facts"`
	Chunks       int    `json:"chunks"`
	Tokens       int    `json:"tokens"`
	AgentCalls   int    `json:"agent_calls"`
	NearDupPairs int    `json:"near_dup_pairs"`
}

// distillEvalPrecision is pooled across the run's facts, never per-session:
// a silent session's precision is 0/0, and dropping such sessions would bias
// against the variant that succeeds at silence.
type distillEvalPrecision struct {
	Golden   *float64 `json:"golden,omitempty"`
	Judge    *float64 `json:"judge,omitempty"`
	Combined *float64 `json:"combined,omitempty"`
	GoldenN  int      `json:"golden_n"`
	JudgeN   int      `json:"judge_n"`
}

type distillEvalRedundancy struct {
	ExactDupRate float64 `json:"exact_dup_rate"`
	NearDupRate  float64 `json:"near_dup_rate"`
}

type distillEvalCost struct {
	ExtractionCalls int     `json:"extraction_calls"`
	Tokens          int     `json:"tokens"`
	WallSeconds     float64 `json:"wall_seconds"`
}

// distillEvalWordJaccard is the near-duplicate test: lowercase word-set
// Jaccard. ponytail: naive text similarity with a fixed 0.5 threshold; the
// upgrade path is the BM25 ranker if this proves too coarse.
func distillEvalWordJaccard(a, b string) float64 {
	setOf := func(s string) map[string]bool {
		set := map[string]bool{}
		for _, w := range strings.Fields(strings.ToLower(s)) {
			set[w] = true
		}
		return set
	}
	sa, sb := setOf(a), setOf(b)
	if len(sa) == 0 || len(sb) == 0 {
		return 0
	}
	inter := 0
	for w := range sa {
		if sb[w] {
			inter++
		}
	}
	union := len(sa) + len(sb) - inter
	return float64(inter) / float64(union)
}

const distillEvalNearDupThreshold = 0.5

func newDistillEvalScoreCommand(opts Options) *cobra.Command {
	var (
		runDir          string
		corpusPath      string
		labelsPath      string
		judgePath       string
		out             string
		allowIncomplete bool
	)
	cmd := &cobra.Command{
		Use:   "score",
		Short: "Score a run: pooled precision, volume, redundancy, cap share, cost",
		RunE: func(cmd *cobra.Command, args []string) error {
			corpus, err := loadDistillEvalCorpusFile(corpusPath)
			if err != nil {
				return err
			}
			var meta distillEvalRunMeta
			metaData, err := os.ReadFile(filepath.Join(runDir, "run.json"))
			if err != nil {
				return fmt.Errorf("read run metadata: %w", err)
			}
			if err := json.Unmarshal(metaData, &meta); err != nil {
				return fmt.Errorf("decode run metadata: %w", err)
			}
			chunks, facts, err := readDistillEvalRawFacts(filepath.Join(runDir, "raw-facts.jsonl"))
			if err != nil {
				return err
			}

			corpusSessions := corpus.Sessions
			if meta.GoldenOnly {
				var golden []distillEvalCorpusEntry
				for _, s := range corpusSessions {
					if s.Golden {
						golden = append(golden, s)
					}
				}
				corpusSessions = golden
			}

			incomplete := distillEvalIncompleteSessions(chunks, facts)
			if len(incomplete) > 0 && !allowIncomplete {
				return fmt.Errorf("session(s) with missing chunks or fact lines (agent failures make a variant read cheaper and quieter than it is): %s; fix the run or pass --allow-incomplete to score anyway", strings.Join(incomplete, ", "))
			}

			factsBySession := map[string][]distillRawFactLine{}
			for _, f := range facts {
				factsBySession[f.SessionID] = append(factsBySession[f.SessionID], f)
			}
			chunksBySession := map[string]int{}
			atCap := 0
			for _, c := range chunks {
				chunksBySession[c.SessionID]++
				if c.Cap > 0 && c.FactsEmitted == c.Cap {
					atCap++
				}
			}

			tokensBySession, callsBySession, err := distillEvalReadCallTrace(filepath.Join(runDir, "call-trace.jsonl"))
			if err != nil {
				return err
			}

			exactPairs, nearPairs := 0, 0
			nearBySession := map[string]int{}
			for sessionID, sf := range factsBySession {
				for i := 0; i < len(sf); i++ {
					for j := i + 1; j < len(sf); j++ {
						if sf[i].FactID == sf[j].FactID {
							exactPairs++
							continue
						}
						if distillEvalWordJaccard(sf[i].Text, sf[j].Text) >= distillEvalNearDupThreshold {
							nearPairs++
							nearBySession[sessionID]++
						}
					}
				}
			}

			var sessions []distillEvalSessionResult
			totalCalls, totalTokens := 0, 0
			for _, entry := range corpusSessions {
				calls := callsBySession[entry.SessionID]
				if calls == 0 {
					calls = chunksBySession[entry.SessionID]
				}
				totalCalls += calls
				totalTokens += tokensBySession[entry.SessionID]
				sessions = append(sessions, distillEvalSessionResult{
					SessionID:    entry.SessionID,
					Facts:        len(factsBySession[entry.SessionID]),
					Chunks:       chunksBySession[entry.SessionID],
					Tokens:       tokensBySession[entry.SessionID],
					AgentCalls:   calls,
					NearDupPairs: nearBySession[entry.SessionID],
				})
			}

			precision, judgeConfig, err := distillEvalPooledPrecision(facts, labelsPath, judgePath)
			if err != nil {
				return err
			}

			ratio := func(num, den int) float64 {
				if den == 0 {
					return 0
				}
				return float64(num) / float64(den)
			}
			summary := distillEvalSummary{
				Variant:            meta.Variant,
				CorpusSHA256:       meta.CorpusSHA256,
				GoldenOnly:         meta.GoldenOnly,
				JudgeConfig:        judgeConfig,
				IncompleteSessions: incomplete,
				Sessions:           sessions,
				Facts:              len(facts),
				Precision:          precision,
				CapShare:           ratio(atCap, len(chunks)),
				Redundancy: distillEvalRedundancy{
					ExactDupRate: ratio(exactPairs, len(facts)),
					NearDupRate:  ratio(nearPairs, len(facts)),
				},
				Cost: distillEvalCost{
					ExtractionCalls: totalCalls,
					Tokens:          totalTokens,
					WallSeconds:     meta.WallSeconds,
				},
			}
			data, err := json.MarshalIndent(summary, "", "  ")
			if err != nil {
				return err
			}
			if out == "" {
				out = filepath.Join(runDir, "summary.json")
			}
			if err := os.WriteFile(out, append(data, '\n'), 0o600); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\n", data)
			return nil
		},
	}
	cmd.Flags().StringVar(&runDir, "run", "", "Run directory (required)")
	cmd.Flags().StringVar(&corpusPath, "corpus", "", "Path to corpus.json (required; lists every session, silent ones included)")
	cmd.Flags().StringVar(&labelsPath, "labels", "", "Filled golden label sheet")
	cmd.Flags().StringVar(&judgePath, "judge", "", "Judge verdicts file")
	cmd.Flags().StringVar(&out, "out", "", "Summary path (default <run>/summary.json)")
	cmd.Flags().BoolVar(&allowIncomplete, "allow-incomplete", false, "Score despite sessions with missing chunks or fact lines; they are listed in the summary")
	_ = cmd.MarkFlagRequired("run")
	_ = cmd.MarkFlagRequired("corpus")
	return cmd
}

// distillEvalIncompleteSessions flags sessions whose raw output cannot be
// trusted: fewer chunk lines than any chunk line's chunk_count claims (failed
// chunks write no line), or fewer fact lines than the chunk lines' summed
// facts_emitted (a raw-write failure drops the tail of a chunk).
func distillEvalIncompleteSessions(chunks []distillRawChunkLine, facts []distillRawFactLine) []string {
	seen := map[string]int{}
	expected := map[string]int{}
	emitted := map[string]int{}
	for _, c := range chunks {
		seen[c.SessionID]++
		if c.ChunkCount > expected[c.SessionID] {
			expected[c.SessionID] = c.ChunkCount
		}
		emitted[c.SessionID] += c.FactsEmitted
	}
	factLines := map[string]int{}
	for _, f := range facts {
		factLines[f.SessionID]++
	}
	var incomplete []string
	for sessionID, n := range seen {
		if n < expected[sessionID] || factLines[sessionID] < emitted[sessionID] {
			incomplete = append(incomplete, sessionID)
		}
	}
	sort.Strings(incomplete)
	return incomplete
}

func distillEvalReadCallTrace(path string) (tokens, calls map[string]int, err error) {
	tokens, calls = map[string]int{}, map[string]int{}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return tokens, calls, nil
		}
		return nil, nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var rec struct {
			Session      string `json:"session"`
			InputTokens  int    `json:"input_tokens"`
			OutputTokens int    `json:"output_tokens"`
		}
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			return nil, nil, fmt.Errorf("%s: %w", path, err)
		}
		if rec.Session == "" {
			continue
		}
		calls[rec.Session]++
		tokens[rec.Session] += rec.InputTokens + rec.OutputTokens
	}
	return tokens, calls, sc.Err()
}

// distillEvalPooledPrecision pools keep/(keep+kill) over the run's UNIQUE fact
// ids. Golden (owner) verdicts override judge verdicts in Combined.
func distillEvalPooledPrecision(facts []distillRawFactLine, labelsPath, judgePath string) (distillEvalPrecision, string, error) {
	var precision distillEvalPrecision
	inRun := map[string]bool{}
	var runOrder []string
	for _, f := range facts {
		if !inRun[f.FactID] {
			inRun[f.FactID] = true
			runOrder = append(runOrder, f.FactID)
		}
	}

	var labels map[string]distillEvalVerdict
	if labelsPath != "" {
		var err error
		labels, err = loadDistillEvalLabels(labelsPath)
		if err != nil {
			return precision, "", err
		}
	}
	var judged map[string]distillEvalJudgeLine
	judgeConfig := ""
	if judgePath != "" {
		var err error
		judged, err = loadDistillEvalJudgeLines(judgePath)
		if err != nil {
			return precision, "", err
		}
		for _, j := range judged {
			judgeConfig = j.JudgeAgent + "/" + j.JudgeModel
			break
		}
	}

	rate := func(keeps, n int) *float64 {
		if n == 0 {
			return nil
		}
		v := float64(keeps) / float64(n)
		return &v
	}
	goldenKeeps, judgeKeeps, combinedKeeps, combinedN := 0, 0, 0, 0
	for _, id := range runOrder {
		label, hasLabel := labels[id]
		j, hasJudge := judged[id]
		if hasLabel {
			precision.GoldenN++
			if label.Verdict == "keep" {
				goldenKeeps++
			}
		}
		if hasJudge {
			precision.JudgeN++
			if j.Verdict == "keep" {
				judgeKeeps++
			}
		}
		switch {
		case hasLabel:
			combinedN++
			if label.Verdict == "keep" {
				combinedKeeps++
			}
		case hasJudge:
			combinedN++
			if j.Verdict == "keep" {
				combinedKeeps++
			}
		}
	}
	precision.Golden = rate(goldenKeeps, precision.GoldenN)
	precision.Judge = rate(judgeKeeps, precision.JudgeN)
	precision.Combined = rate(combinedKeeps, combinedN)
	return precision, judgeConfig, nil
}
