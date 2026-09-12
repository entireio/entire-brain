# Repairing invalid proposal IDs

Hosted proposal listing and resolution reject IDs that do not match the decision
they name. A malformed entry blocks the branch's normal queue operations rather
than allowing an agent to resolve a substituted decision.

Use the explicit repair command to inspect the rejected entries without resolving
proposals or changing facts. The same hosted opt-in, no-egress, credentials, repo,
and branch settings as other `facts proposals` commands apply.

```sh
entire brain facts proposals repair --branch main --json
```

Review the returned `invalid` entries and save the output for diagnosis. To remove
only those malformed entries, copy the exact `ref` from that preview:

```sh
entire brain facts proposals repair --branch main --apply --if-ref '<preview ref>'
```

The command preserves valid proposals and never writes the shared fact head or
local fact store. Both the preview ref check and the server's compare-and-swap
must match. If the queue changed, preview it again; repair does not automatically
retry against entries that were not in the reviewed snapshot. Normal proposal
listing and resolution keep their identity checks; repair is an explicit
maintenance operation, not a verification bypass.

Missing IDs from older servers are derived as usual and are not removed. This
command repairs proposal ID/content mismatches only. It does not authenticate
provenance, repair malformed JSON, or repair a shared fact-set head.
