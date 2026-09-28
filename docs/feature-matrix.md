# How Brain compares

`feature-matrix.csv` opens in Excel, Numbers or Sheets: 51 capabilities across
Entire Graph, Entire Brain, Graphify, mem0, cognee, supermemory and
codebase-memory-mcp.

## Two caveats

**Our columns are checked against source and capabilities.** Brain is checked
against this branch; Graph uses the installed provider and its capability
report. `entire graph capabilities --json` and `entire brain capabilities --json`
cover only part of the surface; command help and the linked source documents
cover the rest. A Yes describes an available capability, not a claim that every
released binary already includes it. The five
competitor columns come from public documentation. Nothing was installed or
benchmarked. Every claim about another product cites a page in `Source`, and
`Unknown` means unknown, never "no".

**We chose the rows.** The first version had 31 and Graph or Brain answered Yes
to all of them, which is what happens when the people who build the product pick
the questions. It now has 51, and **15 are rows where neither of ours answers
Yes** — down from 20 after checking the original twelve REAL GAP rows against
current source. Graph answers Yes to 19 of 51; Brain to 31, delegating
5 more to Graph — counted separately, because delegating is not having.

## Labels in the Notes column

| label | meaning | rows |
|---|---|---|
| **REAL GAP** | We lacked it, and local-only + zero-LLM did not explain why | 2 (was 12) |
| **TRADE-OFF** | We lack it as a direct cost of that design | 5 |
| **PARTLY REAL** | Part of the absence is excused, part is not. Describes the verdict, not a partial capability — such a row can read `No` in both our columns | 2 |
| **THEY DO IT BETTER** | We demonstrably have it and someone goes further. Not used where our cells are `Unknown` | 2 |

Rows that were re-scored open with "Was REAL GAP" and state both the available
capability and its limits. Two REAL GAP rows remain open: edge provenance labels
in [Graph PR #264](https://github.com/entireio/entire-graph/pull/264), and mem0
import in [Brain PR #306](https://github.com/entireio/entire-brain/pull/306).
Neither unmerged proposal is counted as shipped.

## What the gaps said, and what happened

Five of those cells moved
to Yes: global facts, recency, MCP fact writes, HTTP MCP, and REST and SDKs.
Document ingest remains Partial because images and OCR are unsupported.
Plain-file memory is Partial because export can be searched but edits do not
round-trip into the store. Webhooks and the two framework adapters are Partial
because integration coverage remains limited.

**Scale remains Unknown at 100M lines.** The historical scale summaries reach
4.8M lines, with a 1,171 MB index and 12.3-second median code search. They do not
establish a scaling law, a capacity limit, or competitor superiority. Raw
per-query samples and exact corpus revisions were not retained; these are not
current-build measurements. `entire brain bench scale` measures a new corpus;
[scale](scale.md) states the method and limits.

**Published benchmarks are Partial, not Yes.** The [eval ledger](eval_ledger.md)
and [scale measurements](scale.md) publish internal results and limitations.
The dedicated overview is in [PR #313](https://github.com/entireio/entire-brain/pull/313).
No head-to-head against another product has been run. These existing sources
support Partial independently of whether that overview merges first.

**One cell was simply wrong.** "Capture beyond a single repository" read No/No
while `entire brain workspace` had shipped multi-repo brains. The real gap was
knowledge belonging to no repository at all, which is what `remember --global`
adds. Pieces still captures across every application; we capture across
repositories.

The three local-first tools that broke the "we are local" defence still do:
**codebase-memory-mcp** ships 162 languages against our 36 semantic of 185,
**Pieces** captures across applications, and **Graphify** parses live Postgres
schemas we do not touch.

## Tools cited without a column

`Notes` and `Source` sometimes cite Pieces, Letta, Zep, Serena, Augment, Zoekt,
Cursor, Sourcegraph, Copilot or Windsurf, where the best evidence came from
outside the five. They are named where relevant but not assessed across all 51
rows.

If you change one of our cells, cite the command. If you change a competitor
cell, cite the page. If a row only exists because it flatters us, delete it.
