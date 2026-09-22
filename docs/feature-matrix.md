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
the questions. It now has 51, and **11 are rows where neither of ours answers
Yes** — down from 20, because the twelve rows labelled REAL GAP were built
rather than argued with. Graph answers Yes to 20 of 51; Brain to 34, delegating
5 more to Graph — counted separately, because delegating is not having.

## Labels in the Notes column

| label | meaning | rows |
|---|---|---|
| **REAL GAP** | We lacked it, and local-only + zero-LLM did not explain why | 0 (was 12) |
| **TRADE-OFF** | We lack it as a direct cost of that design | 5 |
| **PARTLY REAL** | Part of the absence is excused, part is not. Describes the verdict, not a partial capability — such a row can read `No` in both our columns | 2 |
| **THEY DO IT BETTER** | We demonstrably have it and someone goes further. Not used where our cells are `Unknown` | 2 |

Each former REAL GAP row now opens with "Was REAL GAP" and says what shipped,
what did not, and who still does more. Two of them are not wins: scale is a
measured loss, and head-to-head benchmarks are still not head-to-head.

## What the gaps said, and what happened

"We are local" explained fewer absences than it appeared to, and the twelve rows
where it explained nothing were closed one at a time. Ten of those cells moved
to Yes: document ingest, global facts, recency, edge provenance, MCP fact
writes, HTTP MCP, REST and SDKs, plain-file export, mem0 import, and webhooks
(Partial — the events ship, the integration count does not).

**Two did not become wins, and they are the interesting ones.**

**Scale is now a measured loss rather than an Unknown.** Index size and query
latency both grow linearly: 1.17 GB and 12.3 s p50 at 4.8M lines, roughly 100x
larger per line than Augment's published index. Brain suits repositories of a
few hundred thousand lines and does not reach the largest monorepos.
`entire brain bench scale` reproduces it; [scale](scale.md) has the method.

**Published benchmarks are Partial, not Yes.** [benchmarks](benchmarks.md) now
publishes the measured numbers, the statistics behind the retrieval decisions,
the one agent-outcome result that passed our proof gate, and the closed
negatives — with the commands to re-run them. It is still not a head-to-head
against another product, because we have not run one under conditions we would
defend, and the page says so in its second paragraph.

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
