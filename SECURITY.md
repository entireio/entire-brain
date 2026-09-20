# Security Policy

## Reporting a vulnerability

Report security issues privately through GitHub's
[security advisories](https://github.com/entireio/entire-brain/security/advisories/new)
for this repository, or by email to **security@entire.io**.

Please do not open a public issue for a suspected vulnerability.

Include what you can: affected version (`entire brain version`), the platform,
steps to reproduce, and what an attacker gains. A short reproduction is worth
more than a long description.

We aim to acknowledge a report within three working days.

## What Brain reads, and where it goes

Brain's threat model is shaped by what it handles, so it is worth stating
plainly:

- **It reads sensitive material by design.** Repository source, agent session
  transcripts, prompts, commit history, and documentation.
- **It keeps that material on your machine.** The brain lives under the plugin
  data directory; `entire brain path` prints the location for a repository.
- **The default build makes no network calls.** `entire brain doctor` reports
  the effective egress policy under `memory_provider_egress`. Agent-dependent
  commands — `distill`, and anything invoking a hosted model — are the
  exceptions, and they are opt-in.

Because transcripts and source can carry credentials that were never meant to
be retained, Brain applies a redaction pass to synthesised evidence, rendered
cards, transcript excerpts, drafts, and JSON egress. It targets tokens, JWTs,
secret-shaped environment assignments, private-key blocks, and home-directory
paths.

**Treat that redaction as a mitigation, not a guarantee.** It is pattern-based
and cannot recognise every secret shape. A brain built from a repository whose
history contains real credentials should be handled with the same care as the
repository itself.

## Scope

In scope:

- Anything that causes Brain to transmit repository content off the machine
  without the operator opting in
- Escaping the plugin data directory on read or write
- Redaction failures on the documented secret classes above
- Privilege escalation through the watcher, the MCP server, or the installer

Out of scope:

- Secrets committed to the repository under analysis; Brain reflects the
  history it is given
- Findings that require an attacker who already has local code execution as the
  same user
- The `brain_cgo` build's third-party native dependencies, which should be
  reported upstream

## Supported versions

Security fixes land on the latest release. Brain is pre-1.0 and moves quickly;
older versions are not patched.
