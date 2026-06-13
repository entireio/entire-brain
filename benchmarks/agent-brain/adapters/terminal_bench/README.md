# Terminal-Bench adapter (Entire brain + Codex/Claude)

Runs Codex (GPT-5.5) or Claude Code (Opus 4.8) **with the Entire brain injected** against
Terminal-Bench tasks, for a clean brain-vs-no-brain comparison. The no-brain control is the
stock `tb` agent, so the A/B isolates the brain's contribution and reproduces the published
baseline.

## What it does

`EntireBrainCodexAgent` / `EntireBrainClaudeAgent` subclass the stock Terminal-Bench installed
agents and override `perform_task` to, per task: copy a linux/amd64 `entire-brain` binary into
the task container, build **seed+semantic** on the task's working dir, capture the brief packet,
and prepend the disciplined two-step delivery policy (name the change, treat `likely_edit_files`
as a candidate to **verify** not blindly trust — the same shape shipped in `run.py`'s
`full_cli_compact` path).

Terminal-Bench tasks are fresh repos with **no Entire session history**, so this exercises the
**seed+semantic + delivery-discipline** half only. The history half is proven separately on
cli-bench (the `hist-fix-proof` suites). Keep that distinction honest in any writeup.

## Requirements (currently the blockers)

1. **API keys in env** — Terminal-Bench's installed agents read these directly:
   - `OPENAI_API_KEY` for the Codex/GPT-5.5 arm
   - `ANTHROPIC_API_KEY` for the Claude/Opus arm
   The local subscription-authed `claude`/`codex` CLIs cannot substitute.
2. **linux/amd64 Docker** — TB task images are amd64. On Apple Silicon, use a cloud amd64
   runner or accept slow emulation. Build the brain binary to match:
   `GOOS=linux GOARCH=amd64 go build -o /tmp/entire-brain-linux-amd64 ./cmd/entire-brain`
   and `export ENTIRE_BRAIN_LINUX_BIN=/tmp/entire-brain-linux-amd64`.

## Run (once the above are satisfied)

```bash
# brain arm (Codex / GPT-5.5)
tb run --agent-import-path benchmarks.agent_brain.adapters.terminal_bench.entire_brain_agent:EntireBrainCodexAgent \
       --model gpt-5.5 --dataset-name terminal-bench-core
# no-brain control (stock)
tb run --agent codex --model gpt-5.5 --dataset-name terminal-bench-core
```

Phasing (per the campaign plan): smoke 3-5 tasks → ~20 → full `terminal-bench-core`. Stop
condition is a **contamination-clean** result ≥ 88.31% (Sentra's published GPT-5.5+memory number).
Fresh container per task; assert no Entire history leaks into a TB repo (there is none); always
run the same-base no-brain control. A number that is not clean is not a number.

## Status

Code-complete and import-validated against terminal-bench 0.2.18. **Not yet executed
end-to-end** — blocked on the API keys and amd64 Docker above.
