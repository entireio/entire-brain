# How Brain compares

`feature-matrix.csv` opens in Excel, Numbers or Sheets: 51 capabilities across
Entire Graph, Entire Brain, Graphify, mem0, cognee, supermemory and
codebase-memory-mcp.

## Two caveats

**Our columns are measured; the others are not.** Graph and Brain are read from
the shipped binaries — mostly `entire graph capabilities --json` and
`entire brain capabilities --json` — so you can reproduce them. The five
competitor columns come from public documentation. Nothing was installed or
benchmarked. Every claim about another product cites a page in `Source`, and
`Unknown` means unknown, never "no".

**We chose the rows.** The first version had 31 and Graph or Brain answered Yes
to all of them, which is what happens when the people who build the product pick
the questions. It now has 51, and **20 are rows where neither of ours answers
Yes**. Graph answers Yes to 19 of 51; Brain to 26, delegating 5 more to Graph —
counted separately, because delegating is not having.

## Labels in the Notes column

| label | meaning | rows |
|---|---|---|
| **REAL GAP** | We lack it, and local-only + zero-LLM does not explain why | 12 |
| **TRADE-OFF** | We lack it as a direct cost of that design | 5 |
| **PARTLY REAL** | Part of the absence is excused, part is not. Describes the verdict, not a partial capability — such a row can read `No` in both our columns | 2 |
| **THEY DO IT BETTER** | We demonstrably have it and someone goes further. Not used where our cells are `Unknown` | 2 |

## What the gaps say

"We are local" explains fewer absences than it appears. Three local-first tools
break that defence on their own: **codebase-memory-mcp** ships 162 languages
with an embedded language-server pass and no API keys against our 36 semantic of
185; **Pieces** stores everything on-device and still captures across every
application rather than one repository; **Graphify** parses PDFs, Office files
and live Postgres schemas with tree-sitter.

The sharpest gap is what an agent can write. Brain's MCP surface has 36 tools
and four write — `brain_index_repository`, `brain_refresh`,
`brain_ingest_traces`, `brain_delete_project` — all of them derived state. None
authors a durable fact, so an agent cannot record what it just learned;
`entire brain remember` is a CLI command.

## Tools cited without a column

`Notes` and `Source` sometimes cite Pieces, Letta, Zep, Serena, Augment, Zoekt,
Cursor, Sourcegraph, Copilot or Windsurf, where the best evidence came from
outside the five. They are named where relevant but not assessed across all 51
rows.

If you change one of our cells, cite the command. If you change a competitor
cell, cite the page. If a row only exists because it flatters us, delete it.
