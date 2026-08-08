# Publication sanitization

The checked-in benchmark corpus is a publication-safe derivative of the
original local run artifacts. It is not byte-identical to the private producer
artifacts.

The publication pass makes only these classes of substitutions:

- developer and contributor home directories become stable `/redacted/...`
  paths;
- contributor identities in provenance become generic contributor labels;
- host-specific temporary directories become `/redacted/tmp`;
- internal cell, core, and authentication endpoints in legacy C0701 test
  material become reserved `example.invalid` endpoints; and
- machine-local repository and session-date inputs become portable relative
  paths or explicit environment-variable inputs.

Metrics, task prompts, outcomes, scores, timestamps, commits, and validation
results are otherwise retained. Content-addressed bindings were regenerated
against the sanitized files, so current SHA-256 values prove the public corpus,
not the unavailable private originals.

The C0701 runner uses each config's `validation_files`; the neighboring
`patches/` files are legacy traceability artifacts and are not runner inputs.
Their publication-sanitized form must not be treated as an applicable patch
against a private historical checkout.

One AWS-shaped access-key ID remains intentionally in the redaction validation
fixture and its historical failure evidence. It is synthetic test data retained
to preserve the redaction regression contract, not a live credential.

`test_publication_safety.py` scans every tracked file in CI for the audited
private identifiers, machine paths, held-branch names, and operational endpoint
patterns so they cannot be reintroduced silently.

## Repository control at publication

GitHub's API will not enable secret scanning or push protection on this private
repository while GitHub Advanced Security is disabled. At the public-visibility
cutover, enable both repository settings and verify their status. The CI scan
above is the in-repository guard until that platform control becomes available.
