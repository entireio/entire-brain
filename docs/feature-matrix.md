# Feature Matrix

Open `feature-matrix.csv` in Excel or Sheets.

**How to read it.** The **Entire Graph** and **Entire Brain** columns are verified
against the shipped binaries — `entire brain capabilities --json`,
`entire graph capabilities --json`, and `entire brain doctor` on a real
repository. They are facts about this release.

**The competitor columns are not verified.** They are read from each project's
public positioning and README, not from running them. Cells we could not
establish say `Unknown` rather than guessing, and `Unknown` means exactly that:
not "no". Before this matrix is used externally, the competitor columns should
be checked by someone who has run those tools, or dropped.

The two index-token figures (mem0 50.85M, cognee 12.35M) come from the
`entire-graph` LoCoMo comparison table and carry that table's methodology.

No cell in this matrix is a performance claim. Entire Brain's product
documentation deliberately makes none.

## The row list is a bias surface, not just the cells

An earlier version of this matrix had 31 rows and Graph or Brain answered "Yes"
to every one of them. That is not a result; it is what happens when the people
who built the product also choose the questions.

The matrix now carries 49 rows, 18 of which exist because a competitor has
something we do not. Graph answers Yes to 19 of 49 and Brain to 31 of 49. Those
numbers are more useful than the old ones precisely because they are worse.

`feature-gaps.csv` is the companion: every capability the field has that we
lack, each citing a documentation page, and each labelled as one of

- **REAL GAP** — survives our local-only, zero-LLM design, so the design is not
  an excuse for it
- **TRADE-OFF** — a genuine cost of that design
- **PARTLY REAL** — some of each

Ten of the twenty are REAL GAP. The most uncomfortable is that three local-first
tools each break the local-only defence on their own: one ships 162 languages
with an embedded language-server pass and no API keys, one is on-device yet
captures across every application rather than one repository, and one parses
PDFs and live SQL schemas with tree-sitter. Whenever "we are local" is about to
be used to explain an absence, check it against those three first.
