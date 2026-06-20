# Full Plan Local Proof - 2026-06-20

Branch: `codex/full-plan-implementation`

## Commands

```sh
go test ./internal/cli -run 'TestSemanticGraphCommandsUseSQLiteStore|TestWorkspaceGraphReportsSharedExternalContracts'
go test ./internal/cli -run 'TestWorkspaceGraphReportsCrossRepoImportCandidates|TestWorkspaceGraphMatchesScopedPackageImportCandidates|TestWorkspaceImportMatchesGitHubRepoKeys'
go test ./internal/cli -run 'TestWorkspaceGraphMatchesMavenImportCandidates|TestWorkspaceGraphMatchesMavenExternalSymbols|TestWorkspaceImportMatchesPackageRepoKeys'
go test ./internal/cli -run 'TestWorkspaceGraphImportCandidatesPreferImportedSymbols|TestWorkspaceGraphMatchesMavenImportCandidates|TestWorkspaceImportMatchesPackageRepoKeys' -count=1
go test ./internal/cli -run 'TestWorkspaceGraphMatchesKubernetesResourceCandidates|TestWorkspaceGraphReportsCrossRepoImportCandidates|TestWorkspaceGraphReportsSharedExternalContracts' -count=1
go test ./internal/cli -run 'TestWorkspaceGraphReportsCrossRepoGraphQLCalls|TestWorkspaceGraphReportsCrossRepoRouteCalls|TestWorkspaceGraphReportsSharedExternalContracts' -count=1
go test ./internal/cli -run TestWorkspaceGraphReportsCrossRepoGraphQLCalls -count=1
go test ./internal/cli -run TestRefreshSkipsCurrentSemanticIndex
go test ./internal/cli -run 'TestBrainReviewUsesRuntimeTraceForRankingAndContext|TestBrainReviewMapsAnomalyToFinding|TestMCPBrainReviewTool|TestBrainBriefJSONUsesSemanticContextAndLiveOverlay|TestRegressionChangedOperandReportsEachRegressedHintedFile'
go test ./...
```

## Results

- Focused semantic/workspace graph tests passed.
- Warm-refresh semantic no-op proof passed: after an initial refresh, a second
  refresh with the same HEAD does not call `entire sem snapshot` again.
- Full repository tests passed:
  - `github.com/ashtom/entire-brain/internal/cli`: latest local run 97.370s
  - `github.com/ashtom/entire-brain/internal/config`: cached
  - `github.com/ashtom/entire-brain/internal/tui`: cached

## Coverage

- Graph query accepts the existing filter syntax plus the Cypher-style
  `MATCH (a)-[r:TYPE]->(b) WHERE ... RETURN ... LIMIT n` subset.
- Runtime trace ingestion persists queryable `RUNTIME_TRACE` graph facts and
  folds them into graph schema/metrics, trace-path traversal, and
  `brief --json` semantic context.
- Runtime trace facts influence diff-less review ranking and are included as
  optional `runtime_traces` context in review reports when they match the review
  query.
- Workspace graph inspection writes a persisted `workspaces/<name>/graph.json`
  artifact with aggregate contracts and explicit `cross_edges` connecting
  symbols/resources in different repos through shared external endpoints and
  directed route-call edges from `HTTP_CALLS` clients to `HANDLES_ROUTE`
  handlers on the same route endpoint or equivalent canonical route template
  such as `{id}`/`:id`/`<id>`/`[id]`, directed GraphQL edges from operation
  participants to concrete `graphql_resolver` and `graphql_schema_field`
  symbols on the same `external:graphql:<operation>` endpoint,
  repo-key-matched unresolved import
  candidates including GitHub `@owner/repo` scoped package imports and
  package-keyed `cargo/<crate>`, `gomod/<module-path>`,
  `maven/<group>/<artifact>`, `npm/<name>`, `pypi/<name>`, `nuget/<name>`,
  `gem/<name>`, and `composer/<vendor>/<package>` workspace repos, with
  import-candidate targets preferring the matching terminal symbol when an
  import spec names a class/function such as `requests.auth.HTTPBasicAuth`,
  Kubernetes external config resource endpoints such as
  `external:config:kubernetes/service/api`,
  `external:config:kubernetes/configmap/podinfo-values`, and
  `external:config:kubernetes/secret/podinfo-secret-values`, plus custom
  resources such as `external:config:kubernetes/broker/default`, resolved to
  matching local `Service.api`, `ConfigMap.podinfo-values`,
  `Secret.podinfo-secret-values`, and `Broker.default` resource symbols in
  another workspace repo,
  Docker Compose
  service config endpoints such as `external:config:compose/service/db`
  resolved to matching `compose.service.db` resource symbols in another
  workspace repo, plus exact
  `external:symbol:<qualified-name>` references, repo-prefix-qualified external
  symbol references, and package-prefix-qualified external symbols matched to
  symbols defined in another workspace repo.
- MCP exposes `brain_workspace_graph` for the same workspace graph contracts
  and `cross_edges` JSON.
- Current semantic indexes are reused on warm refresh instead of forcing an
  unchanged provider snapshot.

## Remaining Honesty Notes

- The graph query language is a supported Cypher-style subset, not full Cypher.
- Workspace cross-repo graph edges are explicit in `graph.json`, but they are
  still derived from shared external endpoint contracts, exact route endpoint
  matches, GraphQL operation/resolver/schema-field endpoint matches,
  Kubernetes resource and Docker Compose service external config resource
  candidates, repo-key/scoped-package/package-key-matched unresolved import candidates with
  terminal-symbol preference, and exact/repo-prefix/package-prefix
  external-symbol name matches. Package-key matching covers
  cargo/gomod/maven/npm/pypi/nuget/gem/composer keys, but this is still not
  full cross-repo compiler/type-aware symbol resolution.
- Runtime trace facts are now visible in `brief` and review reports, and
  influence likely-file/review ranking. Proof-ready agent-value evals remain
  open before memory-aware lift can be claimed.
