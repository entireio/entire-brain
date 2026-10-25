package cli

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

// distillEvalJudgeRunnerFor is the agent seam for the generation judge; tests
// replace it with a canned runner.
var distillEvalJudgeRunnerFor = defaultDistillAgentRunner

type distillEvalJudgeLine struct {
	FactID     string `json:"fact_id"`
	Verdict    string `json:"verdict"`
	JudgeAgent string `json:"judge_agent"`
	JudgeModel string `json:"judge_model"`
}

type distillEvalCalibration struct {
	N                int     `json:"n"`
	Agreement        float64 `json:"agreement"`
	AgreementOnKills float64 `json:"agreement_on_kills"`
	AgreementOnKeeps float64 `json:"agreement_on_keeps"`
}

// distillEvalJudgePrompt applies the distill template's four checks to
// ALREADY-GENERATED facts. This judges generation quality (should the fact
// exist?), not retrieval relevance — judgePrompt() next door is the latter.
func distillEvalJudgePrompt() string {
	return `You review candidate repository memories extracted from one AI coding session.
For each FACT below, decide whether it deserves to exist as a durable memory
for this repository, applying all four checks:

  1. DURABLE — still relevant a week or a month from now?
  2. USEFUL — would a future session genuinely benefit, or is it obvious?
  3. NOT ALREADY KNOWN — not discoverable from the code, git log, or README?
  4. WORTH WRITING DOWN — would a senior engineer record this in onboarding
     notes, or roll their eyes at it?

A fact failing ANY check is kill. Ephemeral task state, restated code, tool
mechanics, and session-only feedback are kill.

Output exactly one line per fact, in fact-number order:

    <fact#> <keep|kill>

Output only these lines. No prose.`
}

func distillEvalJudgeCacheKey(fact distillRawFactLine, agent, model, promptSHA string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		"gen-v1", fact.Text, fact.SessionID, agent, model, promptSHA,
	}, "\x00")))
	return hex.EncodeToString(sum[:])
}

func distillEvalJudgeInput(sessionID string, facts []distillRawFactLine) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "SESSION: %s\n\nFACTS\n", sessionID)
	for i, f := range facts {
		fmt.Fprintf(&b, "%d [%s] [%s] %s\n", i+1, f.Kind, strings.Join(f.Paths, ","), f.Text)
	}
	return []byte(b.String())
}

// parseDistillEvalJudgeOutput maps 1-based fact numbers to keep verdicts. Any
// fact the output misses or garbles stays kill — a judge hiccup must never
// inflate precision.
func parseDistillEvalJudgeOutput(output string, n int) []string {
	verdicts := make([]string, n)
	for i := range verdicts {
		verdicts[i] = "kill"
	}
	for _, raw := range strings.Split(output, "\n") {
		fields := strings.Fields(strings.TrimSpace(raw))
		if len(fields) < 2 {
			continue
		}
		idx, err := strconv.Atoi(fields[0])
		if err != nil || idx < 1 || idx > n {
			continue
		}
		if fields[1] == "keep" {
			verdicts[idx-1] = "keep"
		}
	}
	return verdicts
}

func newDistillEvalJudgeCommand(opts Options) *cobra.Command {
	var (
		runDir    string
		agent     string
		model     string
		cachePath string
		out       string
	)
	cmd := &cobra.Command{
		Use:   "judge",
		Short: "Judge every generated fact with a fixed agent (keep/kill, four checks)",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, facts, err := readDistillEvalRawFacts(filepath.Join(runDir, "raw-facts.jsonl"))
			if err != nil {
				return err
			}
			// One verdict per unique fact id, first occurrence wins the slot;
			// scoring looks verdicts up by id.
			seen := map[string]bool{}
			bySession := map[string][]distillRawFactLine{}
			var sessionOrder []string
			for _, f := range facts {
				if seen[f.FactID] {
					continue
				}
				seen[f.FactID] = true
				if _, ok := bySession[f.SessionID]; !ok {
					sessionOrder = append(sessionOrder, f.SessionID)
				}
				bySession[f.SessionID] = append(bySession[f.SessionID], f)
			}

			prompt := distillEvalJudgePrompt()
			promptSum := sha256.Sum256([]byte(prompt))
			promptSHA := hex.EncodeToString(promptSum[:])
			judgeArgs, err := distillAgentCommandArgs(agent, nil, prompt)
			if err != nil {
				return fmt.Errorf("judge agent: %w", err)
			}
			judgeArgs = injectAgentModel(judgeArgs, agent, model)
			run := distillEvalJudgeRunnerFor(agent)

			cache := map[string]string{}
			if cachePath != "" {
				if data, err := os.ReadFile(cachePath); err == nil {
					if err := json.Unmarshal(data, &cache); err != nil {
						return fmt.Errorf("parse judge cache %s: %w", cachePath, err)
					}
				} else if !os.IsNotExist(err) {
					return err
				}
			}
			cacheDirty := false

			if out == "" {
				out = filepath.Join(runDir, "judge.jsonl")
			}
			f, err := os.Create(out)
			if err != nil {
				return err
			}
			defer f.Close()
			enc := json.NewEncoder(f)

			judged := 0
			for _, sessionID := range sessionOrder {
				sessionFacts := bySession[sessionID]
				var pending []distillRawFactLine
				verdictByID := map[string]string{}
				for _, fact := range sessionFacts {
					if v, ok := cache[distillEvalJudgeCacheKey(fact, agent, model, promptSHA)]; ok {
						verdictByID[fact.FactID] = v
						continue
					}
					pending = append(pending, fact)
				}
				if len(pending) > 0 {
					output, err := run(cmd.Context(), opts.Env.RepoRoot, judgeArgs, distillEvalJudgeInput(sessionID, pending), defaultDistillTimeout)
					if err != nil {
						return fmt.Errorf("judge session %s: %w", sessionID, err)
					}
					verdicts := parseDistillEvalJudgeOutput(output, len(pending))
					for i, fact := range pending {
						verdictByID[fact.FactID] = verdicts[i]
						cache[distillEvalJudgeCacheKey(fact, agent, model, promptSHA)] = verdicts[i]
						cacheDirty = true
					}
				}
				for _, fact := range sessionFacts {
					judged++
					if err := enc.Encode(distillEvalJudgeLine{
						FactID: fact.FactID, Verdict: verdictByID[fact.FactID],
						JudgeAgent: agent, JudgeModel: model,
					}); err != nil {
						return err
					}
				}
			}
			if cacheDirty && cachePath != "" {
				data, err := json.MarshalIndent(cache, "", "  ")
				if err != nil {
					return err
				}
				if err := writeFileAtomic(cachePath, append(data, '\n'), 0o600); err != nil {
					return err
				}
			}
			fmt.Fprintf(cmd.OutOrStdout(), "judged %d facts (%s/%s) → %s\n", judged, agent, model, out)
			return nil
		},
	}
	cmd.Flags().StringVar(&runDir, "run", "", "Run directory containing raw-facts.jsonl (required)")
	cmd.Flags().StringVar(&agent, "agent", "claude-code", "Judge agent")
	cmd.Flags().StringVar(&model, "model", "claude-fable-5", "Judge model (must differ from the distiller under test)")
	cmd.Flags().StringVar(&cachePath, "judge-cache", "", "Persist/reuse verdicts at this path")
	cmd.Flags().StringVar(&out, "out", "", "Verdicts path (default <run>/judge.jsonl)")
	_ = cmd.MarkFlagRequired("run")
	return cmd
}

func loadDistillEvalJudgeLines(path string) (map[string]distillEvalJudgeLine, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]distillEvalJudgeLine{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		if len(sc.Bytes()) == 0 {
			continue
		}
		var line distillEvalJudgeLine
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, lineNo, err)
		}
		out[line.FactID] = line
	}
	return out, sc.Err()
}

func newDistillEvalCalibrateCommand(opts Options) *cobra.Command {
	var labelsPath, judgePath, out string
	cmd := &cobra.Command{
		Use:   "calibrate",
		Short: "Measure judge agreement against the golden labels",
		RunE: func(cmd *cobra.Command, args []string) error {
			labels, err := loadDistillEvalLabels(labelsPath)
			if err != nil {
				return err
			}
			judged, err := loadDistillEvalJudgeLines(judgePath)
			if err != nil {
				return err
			}
			var n, agree, kills, killAgree, keeps, keepAgree int
			for id, label := range labels {
				j, ok := judged[id]
				if !ok {
					continue
				}
				n++
				match := j.Verdict == label.Verdict
				if match {
					agree++
				}
				if label.Verdict == "kill" {
					kills++
					if match {
						killAgree++
					}
				} else {
					keeps++
					if match {
						keepAgree++
					}
				}
			}
			ratio := func(num, den int) float64 {
				if den == 0 {
					return 0
				}
				return float64(num) / float64(den)
			}
			cal := distillEvalCalibration{
				N:                n,
				Agreement:        ratio(agree, n),
				AgreementOnKills: ratio(killAgree, kills),
				AgreementOnKeeps: ratio(keepAgree, keeps),
			}
			data, err := json.MarshalIndent(cal, "", "  ")
			if err != nil {
				return err
			}
			if out == "" {
				out = filepath.Join(filepath.Dir(judgePath), "calibration.json")
			}
			if err := os.WriteFile(out, append(data, '\n'), 0o600); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\n", data)
			return nil
		},
	}
	cmd.Flags().StringVar(&labelsPath, "labels", "", "Filled golden label sheet (required)")
	cmd.Flags().StringVar(&judgePath, "judge", "", "Judge verdicts file (required)")
	cmd.Flags().StringVar(&out, "out", "", "Calibration report path (default <judge-dir>/calibration.json)")
	_ = cmd.MarkFlagRequired("labels")
	_ = cmd.MarkFlagRequired("judge")
	return cmd
}
