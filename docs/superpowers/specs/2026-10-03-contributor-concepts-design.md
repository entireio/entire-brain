# Contributor Concepts and Architecture Guide

## Goal

Give contributors and agents working on entire-brain a skimmable glossary and mental model of how the repository's captured inputs become local brain stores and retrieval surfaces, while keeping `CONTRIBUTING.md` the entry point for practical contribution guidance.

## Audience and scope

This documentation is for people changing entire-brain, not end users onboarding a repository. The slice is documentation-only. It updates `CONTRIBUTING.md`, adds `docs/concepts.md`, and links that page from `README.md`. It does not change runtime behavior, CLI command organization, or existing detailed user/operations guidance.

## Structure

- `CONTRIBUTING.md` remains the short entry point. It links to the concepts page, summarizes the source tree and normal local checks, and links to the detailed development, installation/build-tag, and release references.
- `docs/concepts.md` contains a concise glossary and an internal data-flow overview. The overview connects Entire capture/checkpoints and repository inputs to refresh/build stages, per-repository persistent state, indexed retrieval sources, CLI/MCP/hooks, and multi-repository workspaces.
- `README.md` links to the concepts page from its documentation list.

The concepts page should link to `docs/reference.md`, `docs/getting-started.md`, and `docs/durable_facts_plan.md` for detailed behavior rather than restating those guides. It may link to `docs/agent-coordination.md` for agent activation and coordination.

## Content rules

- Include only concepts supported by current implementation, not a candidate term merely because it appeared in the issue packet.
- Explain the distinctions that are easy to conflate: repository identity (`repo key`) versus the local brain store (`brain`); captured checkpoint/session evidence versus derived history/facts/patterns; individual repository brains versus a workspace manifest/aggregate; and provider-generated semantic index versus Brain's retrieval corpus.
- Describe facts as either authored (`remember`) or distilled from retained sessions, and describe conversation retrieval as opt-in/experimental where relevant.
- Treat freshness as source-specific status about indexed inputs, not a guarantee that all sources are current.
- Describe `refresh` as an orchestration/build step and link to the current CLI help for command specifics. Do not state that a planned CLI help grouping has shipped.
- Architecture statements are verified against current source and executed commands; stored Brain/Graph index results are discovery aids only.
- Keep glossary definitions short and make the architecture overview explain relationships rather than repeat definitions.

## Verified repository facts at design time

- The code is a Go module targeting Go 1.27 (`go.mod`); this checkout reports Go 1.27.1.
- The host invokes the plugin as `entire brain`; current `entire brain --help` presents conceptual user-facing categories, including retrieval, maintenance, semantic index, and workspace-related commands.
- Repository stores are rooted below plugin data `repos/<repo-key>`; `workspace.go` currently defines workspace manifests with repository keys and optional local path hints, and resolves them under plugin data `workspaces/<name>`.
- A Brain manifest tracks seed, sessions, semantic, history, facts, docs, and patterns source layers (`internal/cli/brain.go`). `refresh` coordinates session export, seed/docs, history index, document index, and semantic index stages (`internal/cli/refresh.go`).
- Mise defines `fmt`, `lint`, `test`, `test:ci`, `test:phase1`, `build`, `build-all`, and aggregate `check` tasks. `lint` includes `go vet`, gofmt verification, module tidiness, and shellcheck. The focused user-facing contributor path is `mise run lint` and `mise run test`; broader CI validation can link to the documented/check task rather than imply every local contributor must run the release gate.

## Validation

- Check all relative Markdown links added or changed in the three user-facing docs.
- Execute any newly documented command that is represented as canonical guidance (Go version and help/build/task commands).
- Run `git diff --check`, repository lint, and repository tests; report the actual commands and results. Since this is documentation-only, do not add production tests solely to exercise prose.
- Review the complete diff for scope, accuracy, duplicate material, and unsettled CLI-layout claims.
