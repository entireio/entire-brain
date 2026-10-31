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
  the effective egress policy under `memory_provider_egress`. The exceptions are
  opt-in: agent-dependent commands — `distill`, and anything invoking a hosted
  model — and webhooks, which stay off until `ENTIRE_BRAIN_WEBHOOK_URL` is set
  and send fact ids, kinds and taxonomy paths but not fact text unless
  `ENTIRE_BRAIN_WEBHOOK_INCLUDE_TEXT` is also set. Both respect
  `ENTIRE_BRAIN_NO_EGRESS`.

## Egress controls

This is the authoritative list of the environment variables that govern where
brain data may go, in precedence order:

1. **`ENTIRE_BRAIN_NO_EGRESS`** — the master kill switch. When set, nothing
   leaves the machine: hosted sync, webhooks, non-local
   model agents, and `add` cloning all refuse. It beats every other setting,
   including a recorded connection. The toggle fails closed: an unrecognised
   value counts as set. Intended for CI and machine-wide policy; per-repo
   intent belongs in `entire brain disconnect`. `ENTIRE_BRAIN_LOCAL_ONLY` is a
   deprecated alias — still honored, but it warns and should be migrated.
2. **Explicit flags** (`--repo-id`, `--api-url`, `--token`) on the manual
   hosted command `memories sync`.
3. **Environment overrides** for those same flags: `ENTIRE_REPO_ID`,
   `ENTIRE_API_URL`, `ENTIRE_API_TOKEN`. These fill fields but never authorize
   egress by themselves: a manual command refuses an env-only target unless the
   repo is connected, a target flag was passed on the invocation, or the legacy
   `ENTIRE_BRAIN_ALLOW_HOSTED=1` opt-in is set (deprecated; honored so existing
   env-only scripts keep working, with a one-time warning).
4. **The recorded connection** (`hosted.json`, written only by
   `entire brain connect` and `entire brain disconnect`). Background sync
   reads only this binding; the override variables above never redirect it.
   On a connected repo the manual commands also default to this binding when
   no flag or env supplies a field. Tokens are minted through the host CLI
   (`entire auth token`, which honors the host CLI's `ENTIRE_TOKEN`), and a
   minted token is only ever sent to the connected binding's own base URL —
   an explicitly supplied target always requires an explicitly supplied token.
   A disconnected repo (explicit `disconnect`, or never connected) gets no
   defaults: manual commands refuse without a full explicit target.

The invariant behind the ordering: enablement only flows from repo state
(`connect`), and the environment can only subtract. No environment variable
creates a connection or redirects background sync.

The API base URL must be `https://` everywhere; plaintext `http://` is refused
unless the host is loopback or `ENTIRE_BRAIN_ALLOW_INSECURE_API_URL` is
explicitly set (that unlock fails closed on unrecognised values).

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
- Privilege escalation through the watcher or the installer

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
