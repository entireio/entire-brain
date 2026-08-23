# Entire Remote Repository Key Canonicalization

## Problem

Brain derives its persistent repository key from the Git remote named `origin`. For ordinary GitHub remotes, `repoKeyFromRemote` maps the host and path to a canonical key such as `gh/entirehq/entire-api`.

An Entire proxy remote already carries that canonical key in its path:

```text
entire://aws-ap-southeast-2.entire.io/gh/entirehq/entire-api
```

Brain currently treats the cluster hostname as an ordinary unknown Git host, assigns it a local domain slug, and produces `gjk/gh/entirehq/entire-api`. The Graph provider recognizes the proxy path and emits `gh/entirehq/entire-api`. Semantic ingestion correctly rejects the resulting identity mismatch.

## Goals

- Derive the same Brain repository key from a direct forge remote and its corresponding `entire://` proxy remote when Brain's forge-domain binding matches the proxy prefix.
- Allow a user to switch `origin` between those bound-equivalent URLs without selecting a different brain.
- Preserve strict semantic snapshot identity validation.
- Preserve canonical proxy paths such as `gh/owner/repo`, `bb/workspace/repo`, `gl/group/repo`, `gt/owner/repo`, and `et/project/repo` without requiring the proxy cluster hostname to identify the forge.

## Non-goals

- Migrating, deleting, or continuing to address brains previously created under cluster-host keys such as `gjk/gh/entirehq/entire-api`.
- Considering remotes other than the one named `origin` when selecting a brain.
- Relaxing semantic validation or rewriting Graph snapshot record identifiers.

## Design

`repoKeyFromRemote` will recognize URLs whose parsed scheme is exactly `entire`, case-insensitively. For these URLs, the normalized non-empty path components are already the canonical repository key and will be joined directly. The first path component is an opaque forge/provider namespace issued by the Entire remote contract; Brain will not validate it against today's built-in host-slug table. The cluster hostname identifies the proxy endpoint, not the source-code provider, and will not receive or persist a `DomainSlugs` entry.

Direct forge remotes retain the existing, separate domain-to-prefix binding: parse the host and repository path, map known hosts such as `github.com` to their fixed provider slug, and generate a stable configured slug for unknown hosts. Proxy paths are therefore forward-compatible with future forge prefixes, but switching between a future forge's proxy and direct URLs produces the same key only after Brain's known or configured domain mapping binds that forge domain to the same prefix.

This makes equivalent origins converge:

| Origin | Repository key |
|---|---|
| `https://github.com/entirehq/entire-api.git` | `gh/entirehq/entire-api` |
| `git@github.com:entirehq/entire-api.git` | `gh/entirehq/entire-api` |
| `entire://cluster.example/gh/entirehq/entire-api` | `gh/entirehq/entire-api` |

A different canonical path still selects a different brain. The remote named `github`, branch push destination, and other auxiliary remotes do not affect identity. Direct/proxy equivalence is guaranteed only where the direct forge domain is bound to the prefix carried by the proxy path.

Malformed `entire://` URLs without both a host and a usable canonical path will not use the proxy special case. They will follow the existing unsupported-remote/local-fallback behavior rather than creating an empty or unsafe key.

## Compatibility

This intentionally changes repository identity only for newly addressed `entire://` origins. Existing data under the prior cluster-host-derived key is not migrated. After upgrading, a refresh creates or uses the corrected canonical brain path. Old derived brain data may be removed separately by the user.

Direct GitHub, GitLab, Bitbucket, Entire, Tangled, Code Storage, unknown-host, SCP-style, local-path, and symlink identity behavior remains unchanged.

## Verification

Tests will establish:

1. A direct GitHub remote and its Entire proxy remote produce the same key.
2. Current (`gh/...`, `et/...`) and future/opaque (`bb/...`, `gl/...`, `gt/...`) proxy paths are preserved as canonical keys without asserting a direct-forge equivalence where no domain binding exists.
3. Proxy cluster hosts do not create `DomainSlugs` entries.
4. Existing unknown-host slug behavior remains unchanged.
5. Semantic indexing accepts a Graph snapshot whose `repo_key` matches the canonical path inside an `entire://` origin.

Focused `internal/cli` tests will run first, followed by the repository's complete format, static-analysis, and test commands before completion.
