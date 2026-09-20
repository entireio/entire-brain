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
