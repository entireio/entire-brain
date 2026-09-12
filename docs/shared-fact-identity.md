# Shared fact content identity

Shared fact-set sync verifies that each record's ID equals `RecordID(text,
paths)` and that its stored paths already equal `NormalizePaths(paths)`. The
check runs on local facts before sync publishes them and on shared heads read
by sync, proposal resolution, and CLI proposal reconciliation.

This binds an ID to normalized text and canonical taxonomy paths. It prevents
a peer from replacing that content while retaining another statement's ID.
`NormalizeText` ignores case and whitespace differences; the check does not
prevent substitutions whose meaning depends on those differences.

## What this does not authenticate

The check is not a record signature. Status, superseded-by, origin, kind,
locus, confidence, related IDs, branch, and provenance are outside the content
ID. A peer with write access to the shared head can still change those fields
under a valid ID, including retracting a statement or claiming a verified
anchor. A valid content ID is not evidence that a lifecycle change or provenance
claim was authorized. Closing that trust gap requires an authenticated record
and lifecycle protocol; changing this content check alone does not provide it.

These checks cover the shared fact-set transport. They do not add identity
verification to the separate hosted Search/Get/MultiGet response interfaces.
Treat hosted content and provenance as untrusted historical evidence.

## Rejected heads and recovery

A malformed local record is rejected before any request publishes it. Repair or
regenerate that record through its trusted authoring source before retrying.

An invalid shared record stops the whole sync or resolution. The client does
not silently discard a record and publish a replacement head: doing that could
turn corruption into loss of another member's facts, and content IDs cannot
authenticate the remaining lifecycle or provenance fields. Other members'
local stores remain available.

There is currently no automatic quarantine or client-side repair command for a
malformed shared head. Recovery requires the hosted service operator to restore
or repair the head from a trusted source through its storage administration,
then retry sync. Preserve the rejected version for diagnosis. This deliberate
fail-closed policy trades shared-sync availability for avoiding an unreviewed
rewrite; it does not prevent a malicious writer from causing denial of service.
