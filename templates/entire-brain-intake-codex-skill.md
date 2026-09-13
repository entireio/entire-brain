---
name: entire-brain-intake
description: Use when working in a repository that may have an Entire brain. Locates the brain with `entire brain path "$PWD"` and reads seed, export, and history-gap context efficiently before coding or answering repository questions.
---

# Entire Brain Intake

When starting work in a repository with Entire enabled, first locate the project
brain from the current working directory:

```sh
brain_dir="$(entire brain path "$PWD")"
```

`path` reports where the brain lives; it never builds one. If the command fails,
or `$brain_dir` does not exist, report that this repository has no brain yet
(`entire brain setup` builds it) and continue with normal repo inspection.

Use the brain as staged context. Do not read everything blindly.

## Intake Order

1. Read `$brain_dir/README.md`.
2. Read `$brain_dir/manifest.json`.
3. If present and successful, read:
   - `$brain_dir/seed/agent/quick-overview.md`
   - `$brain_dir/seed/agent/overview.md`
   - `$brain_dir/seed/agent/architecture.md`
   - `$brain_dir/seed/agent/risks.md`
   - `$brain_dir/seed/agent/maintenance-guide.md`
   - `$brain_dir/seed/agent/open-questions.md`
4. Read `$brain_dir/seed/history-gaps.md` if it exists.
5. Use `manifest.json` session metadata to choose relevant transcripts.
6. Read only task-relevant session transcripts. Do not read every transcript
   unless the task requires broad archaeology.

## Interpretation Rules

- Treat seed artifacts as current repository-derived context.
- Treat session transcripts as development rationale where available.
- Treat `history-gaps.md` as uncertainty.
- `missing_session` commits are not transcript-backed.
- `covered` commits were made under an Entire session (they carry a checkpoint
  trailer); the session is exported, though not every intermediate checkpoint has
  its own transcript.
- Do not invent rationale, CI status, release process, or historical decisions
  not present in seed docs, session exports, or commit-gap notes.
