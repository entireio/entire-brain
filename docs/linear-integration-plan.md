# Linear integration implementation

The draft proposal is superseded by [the host-neutral workflow and input contract](linear-workflow.md). The implementation separates Linear MCP remote access from repository-local issue evidence, default retrieval, pinned briefs, explicit work links, and observed write receipts.

Brain owns no Linear credentials or HTTP client. Selected projects define scope. The agent imports active issues and the requested 90-day closed-issue window, refreshes before work, and performs requested writes through the host.

Validation lives in `internal/issues/store_test.go` and `internal/cli/issues_test.go`, alongside existing CLI/MCP, retrieval, workspace, transport, bundle, and platform tests. See [the verification report](linear-verification.md) for acceptance results.
