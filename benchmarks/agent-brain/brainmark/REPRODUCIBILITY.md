# Reproducibility — exact pins

Everything needed to re-run BrainMark and get the same artifacts. Values below
were captured at the $0 build stage on 2026-08-18. A pin that drifts invalidates
the seal, and `report.py` says so rather than silently comparing across versions.

## Toolchain

| component | pin |
|---|---|
| Python | 3.14.6 (stdlib only; `mem0ai` optional and import-guarded) |
| Go | go1.26.2 darwin/arm64 |
| Claude Code CLI | 2.1.234 |
| Docker | required for grading only (`swebench.harness.run_evaluation`) |
| Platform | darwin/arm64 (`sandbox-exec` read-isolation is macOS-only; elsewhere it degrades to netjail + env sanitization and records that in `meta.json`) |

## Source pins

| artifact | commit / sha256 |
|---|---|
| entire-brain (this repo) | `07c5760533e3d3ada1f6892dc33206e52dffb8fe`, branch `feat/brainmark` |
| `benchmarks/agent-brain/run.py` (imported as a library, unmodified) | sha256 `24440e92cbf3373611457f8748db31439474892b5ba1d3a1d199e2744ed249a3` |
| graphmark checkout | `623b2c62727ec317266c24d9053052ccff6b36aa` |
| vendored `metrics.py` source commit | `641c009cbe10ccac72059de0c92e339943274b67` |
| vendored `metrics.py` source sha256 | `d84be013f6c87d27e9a795402d0842e4b5add2246a90ea40eca9177cf80cb563` |
| `entire-brain` binary (built here) | sha256 `c68d123f607aa68cf527bcf24d142fe5d5831bcb706120a26bbce6b75d7bc3c7` |

Rebuild the binary with, from the repo root:

```bash
go build -o benchmarks/agent-brain/brainmark/bin/entire-brain ./cmd/entire-brain
```

The binary's sha256 is recorded into every `full_brain` packet's provenance, so a
rebuild is visible in the results rather than silent.

## Task pool

| file | instances contributed |
|---|---|
| `graphmark/agentic-swebench/tasks/multilingual_300.json` | 300 |
| `graphmark/agentic-swebench/tasks/pilot_tasks.json` | 3 (rest duplicate) |
| `graphmark/agentic-swebench/tasks/python_tasks.json` | 0 (all duplicates) |

Per-file sha256 is recorded in `candidates/INDEX.json` under `sources`. Repo cache
is `graphmark/agentic-swebench/repo-cache/<owner>_<name>`.

## Models

Set in `config.json`; **placeholders until the tier is chosen** — fix them before
sealing, since the seal hashes the config.

| role | pilot | full |
|---|---|---|
| session A | `claude-sonnet-4-6` | `claude-opus-4-6` |
| session B | `claude-sonnet-4-6` | `claude-opus-4-6` |
| `entire-brain distill` | agent `claude-code`, model `claude-sonnet-4-6`, effort `medium` | same |

Agent invocation is fixed (`run_b.py:build_command`): `--print
--no-session-persistence --strict-mcp-config --mcp-config '{"mcpServers":{}}'
--disable-slash-commands --permission-mode bypassPermissions --output-format
stream-json --verbose --safe-mode`. Session A omits `--no-session-persistence`
because persistence is what writes the native JSONL that gets harvested.

## Competitor pins

| arm | pin |
|---|---|
| mem0 | `mem0ai==0.1.118`, LLM `openai/gpt-4o-mini`, embedder `openai/text-embedding-3-small` |
| graphify | eg-memharness `GraphifyClient`; binary/source/bridge via `GRAPHIFY_{BRIDGE,SOURCE,PYTHON}` |
| cmm | eg-memharness `CmmClient`; `CMM_BIN`, index mode `full`, `CMM_MEM_BUDGET_MB=4096` |

`CMM_MEM_BUDGET_MB` is a **resource** setting, not a capability change: cmm
otherwise reserves half of system RAM per process, which fails searches outright
under concurrency 8.

Competitor clients are **imported by path** from
`/Users/suhaan/devenv/eg-memharness/bench/memory/benchmarks/common/`, not copied,
so a fix there propagates. A missing checkout fails the arm loudly at prep.

## Seeds and estimator

- master seed `20260815` (`config.json`)
- bootstrap: 20,000 percentile draws, seeded — identical inputs give identical CIs
- estimator: vendored graphmark `geo` / `median_ratio` / `bootstrap_ci` /
  `loo_range` / `mcnemar_exact`, equal-weighted per pair
- ratio offset `+1`
- packet budget 24,576 bytes, `top_k` 20

## Determinism guarantees

| stage | guarantee |
|---|---|
| mining | byte-identical across runs; tested |
| packet envelope | canonical JSON (sorted keys, tight separators); re-derivation gives the same sha256; tested |
| prompts | symmetry sha identical across all five arms; tested |
| agent sessions | **not** deterministic — this is why every packet is pinned and every session is graded externally |
| grading | official SWE-bench Docker harness |

## Re-running the $0 stage

```bash
cd benchmarks/agent-brain
python3 -m unittest discover -s brainmark/tests -t . -p "test_*.py"   # 55 tests
python3 brainmark/mine_pairs.py                                       # deterministic
python3 brainmark/seal.py verify                                      # after sealing
```
