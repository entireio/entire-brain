# How Brain compares

`feature-matrix.csv` sits beside this file and opens in Excel, Numbers or
Sheets. It puts Entire Graph and Entire Brain against five alternatives across
51 capabilities: Graphify, mem0, cognee, supermemory, and codebase-memory-mcp.

Read the two caveats first. They are the difference between a useful document
and a sales sheet.

## Our columns are measured; the others are not

The **Entire Graph** and **Entire Brain** columns are read from the shipped
binaries, mostly from `entire graph capabilities --json` and
`entire brain capabilities --json`. You can reproduce any of them.

The five competitor columns come from those projects' public documentation.
**Nothing was installed and nothing was benchmarked.** Every claim about
another product cites a page in the `Source` column, and where we could not
find an answer the cell reads `Unknown` — which means unknown, and never "no".

If you are making a decision that turns on one of those cells, verify it
yourself. We would rather you did.

## The rows are a bias surface, and we got this wrong first

The first version of this matrix had 31 rows, and Graph or Brain answered "Yes"
to every single one. That is not a finding. It is what happens when the people
who build the product also choose the questions.

Someone asked the obvious question — *what do the others have that you don't?* —
and the matrix now carries 51 rows, 20 of which exist because a competitor has
something we lack. Graph answers Yes to 19 of 51 and Brain to 31 of 51. Those
numbers are more useful than the old ones precisely because they are worse.

The `Notes` column labels each of those rows:

| label | meaning |
|---|---|
| **REAL GAP** | We lack it, and our local-only, zero-LLM design does not explain why. 11 rows. |
| **TRADE-OFF** | We lack it as a direct cost of that design. 5 rows. |
| **PARTLY REAL** | Some of each. 2 rows. |
| **THEY DO IT BETTER** | We have it; someone does it further. 3 rows. |

## What the gaps say

The pattern worth internalising is that **"we are local" explains fewer of our
absences than it first appears.** Three local-first tools each break that
defence on their own:

- **codebase-memory-mcp** ships 162 languages with an embedded language-server
  pass and no API keys. We cover 36 semantic languages of 185 detected.
- **Pieces** stores everything on-device and still captures across every
  application rather than one repository — so Brain's per-repository scope is a
  product decision, not a privacy consequence.
- **Graphify** parses PDFs, Office files and live Postgres schemas with
  tree-sitter, locally. Deterministic parsers cost no tokens.

The sharpest single gap is that **Brain's MCP tools are read-only.** An agent
mid-session cannot persist what it just learned; `entire brain remember` is a
CLI command. Every competitor surveyed has a write path.

## Tools cited but not given a column

The `Notes` and `Source` columns sometimes cite tools that are not columns,
because the best evidence for a gap came from outside the five: Pieces, Letta,
Zep, Serena, Augment, Zoekt, Cursor, Sourcegraph, Copilot and Windsurf. They are
named where they are relevant rather than hidden, but they have not been
assessed across all 51 rows.

## Keeping it honest

If you change a cell in our two columns, cite the command that shows it. If you
change a competitor cell, cite the page. If a row only exists because it
flatters us, delete it.
