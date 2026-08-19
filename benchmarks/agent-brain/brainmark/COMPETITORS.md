# COMPETITORS.md — baseline dignity

> Plan item 0.9. A benchmark that beats a badly-configured competitor has measured
> its own configuration, not its product. This file is the contract that stops
> BrainMark from doing that: for each competitor, the **published default** it runs
> under, the **one tuning pass** it is allowed, and the **version pin** its numbers
> are attached to.
>
> Reviewers read this file to answer "was the baseline given a fair chance?".
> `report.py` may not print a competitor column whose pin block here is unresolved.

---

## The three rules

### 1. Published defaults, not our defaults

Every competitor runs the configuration **its own documentation recommends**, at the
version pinned below. Where a setting is not published, the code default wins, and
the fact that we fell back to a code default is recorded in `config.json` with an
`_..._note` key. We never quietly hand a competitor a weaker model, a smaller budget,
or a mode its authors do not recommend.

Two settings are **not** the competitor's to choose, because they are the fairness
frame rather than the product:

- **Ingest bytes.** Every memory source receives the byte-identical, sha256-pinned
  session-A transcript. A competitor that would normally ingest something else still
  gets these bytes; that is the comparison.
- **Packet budget and envelope.** Retrieval output is truncated by the same
  rank-preserving bounder to `packet.max_bytes` and rendered into the same envelope
  for every arm. `top_k` is the competitor's own default; the byte cap is ours and is
  identical across arms.

Deviating from a published default in either direction is a tuning act and is
governed by rule 2.

### 2. Exactly ONE tuning pass, dev split only

Each competitor may be tuned **once**, and only against the 10-pair dev split. The
sealed split is never touched, never inspected, and never used to choose a setting.

The pass must be logged in the table at the bottom of this file **before** the
confirmatory run, with:

| field | meaning |
|---|---|
| date | when the pass was run |
| what changed | the exact setting, old value → new value |
| why | the published guidance or the observed dev-split failure that motivated it |
| dev-split effect | the metric before and after, on dev only |
| who | who ran it |

Rules that make the pass honest:

- **One pass, not one pass per idea.** Trying six settings and keeping the best is
  six passes worth of selection; if that is what happened, say so and report it as
  a dev-split sweep, not a single pass.
- **Symmetric or not at all.** If `full_brain` gets a tuning pass, every competitor
  gets one. If a competitor is left at defaults, `full_brain` runs at defaults too.
- **No confirmatory-set feedback.** A setting changed after seeing sealed-split
  results invalidates the run. The correct response to "the competitor underperforms
  on the sealed split" is to report it, not to fix it.

### 3. Every number carries a pin

A competitor result is quotable only if the run recorded, in the packet provenance:
version/commit, any patch applied (path + sha256), the embedder and LLM used at
ingest and at retrieval, and the top_k / budget in force. An unresolved pin is a
blocker, not a footnote — see **Pin resolution procedure** below.

---

## mem0

- **What**: the named commercial memory competitor (`mem0ai`, OSS library + server).
- **Homepage / repo**: <https://github.com/mem0ai/mem0>
- **Client surface to mirror**:
  `/Users/suhaan/devenv/eg-memharness/bench/memory/benchmarks/common/` (the
  duck-typed `add`/`search` shape every arm in that harness implements).

### Published default config (what BrainMark runs)

| setting | value | source |
|---|---|---|
| package | `mem0ai` | published |
| version pin | `0.1.118` | `config.json:competitors.mem0.version_pin` |
| commit pin | `4debc58a83377b18be81ae1e5969a300736b2fac` | `eg-memharness/bench/memory/UPSTREAM.md:102` |
| extraction LLM | `openai/gpt-4o-mini` | mem0's own default |
| embedder | `openai/text-embedding-3-small` | mem0's code default — **unconfirmed pin, see below** |
| retrieval | `search(query, top_k=<mem0 default>)`, then our shared byte cap | published |

### Deliberate deviation, disclosed

The eg-memharness prose campaign drove mem0's extractor with `azure_ai/gpt-5.6-terra`
(`eg-memharness/bench/memory/README.md:33`), a much stronger model than mem0's own
default. BrainMark **keeps mem0's published default** (`gpt-4o-mini`). That is the
config a mem0 user gets out of the box, so it is the honest baseline. Upgrading the
extractor would be a tuning act and would consume mem0's single permitted pass — and
it would have to be matched by an equivalent upgrade elsewhere under rule 2.

If a reviewer asks for the stronger-extractor variant, run it as a **dev-split
tuning pass** and log it; do not silently swap it into the confirmatory cell.

### Version-pin procedure

```bash
python3 -m pip download "mem0ai==0.1.118" --no-deps -d /tmp/mem0pin   # freeze the wheel
python3 -c "import mem0, hashlib, pathlib; print(mem0.__version__)"
# and record, in the packet provenance for every mem0 packet:
#   mem0.__version__, the embedder model id, the extraction LLM id, top_k
```

Prep cost (ingest LLM calls) is metered and reported in its own column — mem0's
ingest is LLM-driven and that is a real cost difference, not a defect.

---

## Graphify

- **What**: YC-backed code+docs knowledge-graph memory. The code-domain competitor.
- **Repo**: <https://github.com/Graphify-Labs/graphify> · PyPI `graphifyy`
- **Client surface to mirror**:
  `/Users/suhaan/devenv/eg-memharness/bench/memory/benchmarks/common/graphify_client.py`
  (+ its out-of-process bridge `graphify_mem_bridge.py`).

### Published default config (what BrainMark runs)

| setting | value | source |
|---|---|---|
| commit pin | `9f25a3aaa1050913c2d8a1b9f0b0f0ed18296abd` (public **v8**) | `graphify-parity/results/memory-native-v52/results.json:revisions.graphify` |
| extraction mode | `structural` — tree-sitter, **0 LLM**, 0 network | Graphify's published default mode |
| retrieval | Graphify's own `_score_query` → `_pick_seeds` → `_bfs` (depth 3), ranked **nodes** | `graphify_client.py` header |
| top_k | Graphify's default | published |

`structural` is the mode Graphify advertises and the mode the parity harness sealed.
`graphify extract --backend <model>` is the LLM variant; using it would be a tuning
act, not a default.

**Version drift is real, and it is why the pin is load-bearing**: the same client
file recorded commit `07b9143d4b90b1e1cb88dc71423f742a501efd29` for the v45 run and
`9f25a3aa…` for v52. Graphify moves. A BrainMark number must name its commit.

### Environment that must be set before a Graphify arm runs

`graphify_client.py`'s baked-in defaults are **remote-VM paths** and are invalid on
this machine:

```
_DEFAULT_PYTHON = /home/suhaan_entire_io/memarms/venvs/graphify/bin/python
_DEFAULT_SOURCE = /home/suhaan_entire_io/memarms/inputs/repos/graphify
```

So, before the first Graphify packet:

```bash
git clone https://github.com/Graphify-Labs/graphify /path/to/graphify
git -C /path/to/graphify checkout 9f25a3aaa1050913c2d8a1b9f0b0f0ed18296abd
python3 -m venv /path/to/venvs/graphify && /path/to/venvs/graphify/bin/pip install -e /path/to/graphify networkx
export GRAPHIFY_SOURCE=/path/to/graphify GRAPHIFY_PYTHON=/path/to/venvs/graphify/bin/python
git -C /path/to/graphify rev-parse HEAD   # MUST equal the pin; record it in the packet provenance
```

`config.json:competitors.graphify.{bridge,source,python}` stay `null`, meaning
"inherit `GRAPHIFY_*`". Set them explicitly instead if you want the pin enforced by
config rather than by environment.

---

## cmm (codebase-memory-mcp)

- **What**: DeusData's single-binary tree-sitter code knowledge graph + MCP tools.
  The OSS code-memory competitor.
- **Repo**: <https://github.com/DeusData/codebase-memory-mcp>
- **Client surface to mirror**:
  `/Users/suhaan/devenv/eg-memharness/bench/memory/benchmarks/common/cmm_client.py`

### Published default config (what BrainMark runs)

| setting | value | source |
|---|---|---|
| version | `v0.9.0` | upstream release |
| build revision | `b637e3330c96cfe452da623db068c241aaa3ec01` | `graphify-parity/results/memory-native-v52/results.json:revisions.codebase_memory_mcp` (stable across the v45 and v52 runs) |
| patch | `cmm-v0.9.0-markdown-sections`, sha256 `df139f2695152de9ee9c562b93d99c7ad57e357beb8cdd72577a2651aefa205b` | `/Users/suhaan/devenv/eg-memharness/bench/memory/patches/0005-cmm-v0.9.0-markdown-sections.patch` (re-hashed 2026-08-18); identical copy at `graphify-parity/patches/cmm-v0.9.0-markdown-sections.patch` |
| index mode | `full` | published |
| retrieval | `search_graph` — BM25 over SQLite FTS5 | published |

### The patch is a baseline-dignity measure, and it is disclosed

The **shipped** v0.9.0 binary excludes `Section` nodes from its BM25 result set
(`src/mcp/mcp.c::bm25_search`). On a prose / Markdown corpus it therefore indexes
everything and retrieves **nothing** — in the v52 matrix, cmm returned 1050
byte-identical empty contexts and its score was the reader's prior on an empty
context, not a product measurement.

Shipping that as a "competitor result" would be a fake win. The patch drops
`Section` from the exclusion list **and changes nothing else**, so BrainMark runs the
**patched** build: the most charitable version of the product.

Two obligations follow, both non-negotiable:

1. **Disclose the patch in the report itself**, in the competitor table, not only in
   a methods appendix.
2. **Guard against the null arm.** A cmm packet that comes back empty is an
   infrastructure failure, not "memory did not help". `memsources` must raise on an
   empty packet rather than emit one — the same rule every arm is held to.

Note the domain caveat carried over from the prose campaign: cmm is a *code*
memory. BrainMark's corpus is a coding-session transcript, which is closer to its
home domain than LoCoMo prose was, but the transcript is still not a repository.
State this in limitations; do not quote the prose-campaign numbers here.

### UNRESOLVED PIN — the compiled binary

No cmm binary exists on this machine. `cmm_client.py:65` points at
`/home/suhaan_entire_io/memarms/inputs/bin/cmm-patched/codebase-memory-mcp` (a remote
VM) and `devenv/cmm` is a dangling symlink, not a checkout. `config.json` therefore
carries `binary: null` / `binary_sha256: null` — **deliberately null, not invented**.

---

## Pin resolution procedure (for every `null` / "unconfirmed" pin above)

A pin is resolved when the value is (a) obtained from the artifact itself, not from a
memory or a doc, and (b) written into `config.json` **and** echoed into the packet
provenance of every packet that competitor produces. Until then the competitor's
column is marked `PIN UNRESOLVED` in the report.

### cmm binary sha256

```bash
# 1. build or fetch v0.9.0 at the recorded build revision
git clone https://github.com/DeusData/codebase-memory-mcp /path/to/cmm
git -C /path/to/cmm checkout b637e3330c96cfe452da623db068c241aaa3ec01
# 2. apply the sealed patch and verify it first
shasum -a 256 /Users/suhaan/devenv/eg-memharness/bench/memory/patches/0005-cmm-v0.9.0-markdown-sections.patch
#    must print df139f2695152de9ee9c562b93d99c7ad57e357beb8cdd72577a2651aefa205b
git -C /path/to/cmm apply /Users/suhaan/devenv/eg-memharness/bench/memory/patches/0005-cmm-v0.9.0-markdown-sections.patch
# 3. build, then pin the artifact
shasum -a 256 /path/to/cmm/codebase-memory-mcp
# 4. write binary + binary_sha256 into config.json:competitors.cmm
```

If the remote VM is still reachable, the faster path is to hash the artifact that
actually produced the historical numbers:

```bash
ssh <vm> shasum -a 256 /home/suhaan_entire_io/memarms/inputs/bin/cmm-patched/codebase-memory-mcp
```

Record which path was used. A locally rebuilt binary is **not** guaranteed to be the
same artifact as the one on the VM; if they differ, the BrainMark pin is the local
build and the historical numbers are not comparable byte-for-byte.

### mem0 embedder

`text-embedding-3-small` is mem0's **code default**, inferred from upstream at the
pinned commit; no eg-memharness launcher ever set an override
(`grep -n EMBEDDER /Users/suhaan/devenv/eg-memharness/bench/memory/run_locomo.sh`
finds nothing). That makes it *evidence*, not *proof*. Resolve it by observation:

```bash
python3 - <<'PY'
from mem0 import Memory
m = Memory()                     # default config, pinned version
print(m.config.embedder.provider, m.config.embedder.config.model)
print(m.config.llm.provider,      m.config.llm.config.model)
PY
```

Write the observed values into `config.json:competitors.mem0.embedder` and drop the
`_embedder_note`. The same two lines must be emitted into every mem0 packet's
provenance at run time, so the report proves what ran rather than what was configured.

### Graphify commit

Resolved (`9f25a3aa…`), but it is only real if enforced. At run time:

```bash
git -C "$GRAPHIFY_SOURCE" rev-parse HEAD
```

must equal `config.json:competitors.graphify.commit_pin`, and the observed value goes
into the packet provenance. A mismatch is a hard failure, not a warning — Graphify
moved commits between our own v45 and v52 runs.

---

## Tuning-pass log

Empty. No competitor has been tuned. Any row added here must predate the
confirmatory run.

| date | competitor | what changed | why | dev-split effect | who |
|---|---|---|---|---|---|
| — | — | — | — | — | — |

---

## Pin status summary

| competitor | version/commit | patch | embedder / LLM | binary sha256 | status |
|---|---|---|---|---|---|
| mem0 | `4debc58a` (v0.1.118) | n/a | default (**unconfirmed**) | n/a (pip) | **PARTIAL** — resolve embedder by observation |
| Graphify | `9f25a3aa` (v8) | n/a | none (0-LLM structural) | n/a (source) | **RESOLVED** — enforce at run time |
| cmm | v0.9.0 @ `b637e333` | `df139f26…` ✅ verified | none (0-LLM) | **null** | **PARTIAL** — build or hash the binary |
