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
go test ./internal/cli -run 'TestWorkspaceGraphReportsCrossRepoGraphQLCalls|TestWorkspaceGraphReportsCrossRepoGraphQLSchemaResolverWithoutOperation' -count=1
go test ./internal/cli -run TestRefreshSkipsCurrentSemanticIndex
go test ./internal/cli -run 'TestBrainReviewUsesRuntimeTraceForRankingAndContext|TestBrainReviewMapsAnomalyToFinding|TestMCPBrainReviewTool|TestBrainBriefJSONUsesSemanticContextAndLiveOverlay|TestRegressionChangedOperandReportsEachRegressedHintedFile'
go test ./...
go test ./internal/cli -run 'TestWorkspaceImportMatchesGitHubRepoKeys|TestWorkspaceExternalSymbolTargetMatchesGitHubMonorepoPackageAliases|TestWorkspaceExternalSymbolTargetMatchesGitHubMonorepoPackagePrefixes|TestWorkspaceGraphMatchesScopedPackageImportCandidates' -count=1
go test ./internal/cli -run TestWorkspaceGraphReportsCrossRepoGraphQLCalls -count=1
```

## Results

- Focused semantic/workspace graph tests passed.
- GitHub monorepo package-dir hyphen/underscore alias tests passed for both
  import candidate matching and external-symbol target matching.
- GraphQL workspace graph tests passed for operation-to-resolver,
  operation-to-schema-field, and schema-field-to-resolver cross edges,
  including schema-field-to-resolver edges when no operation participant is
  present.
- Warm-refresh semantic no-op proof passed: after an initial refresh, a second
  refresh with the same HEAD does not call `entire sem snapshot` again.
- Full repository tests passed:
  - `github.com/ashtom/entire-brain/internal/cli`: latest local run 97.370s
  - `github.com/ashtom/entire-brain/internal/config`: cached
  - `github.com/ashtom/entire-brain/internal/tui`: cached

## Coverage

- Graph query accepts the existing filter syntax plus the Cypher-style
  `MATCH (a)-[r:TYPE]->(b) WHERE ... RETURN ... LIMIT n` subset, including
  `RETURN count(r)` / `RETURN count(*)` aggregate counts and relation
  predicates such as `WHERE r.type = "CALLS"`.
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
  such as `{id}`/`:id`/`<id>`/`[id]`/`[[id]]`/`[...slug]`/`*path`, with
  trailing-slash-insensitive route matching for non-root paths, directed
  GraphQL edges from operation participants to concrete `graphql_resolver` and
  `graphql_schema_field` symbols, plus schema-field-to-resolver edges, on the
  same `external:graphql:<operation>` endpoint,
  repo-key-matched unresolved import
  candidates including GitHub `@owner/repo` scoped package imports and
  package-keyed `cargo/<crate>`, `gomod/<module-path>`,
  `maven/<group>/<artifact>`, `npm/<name>`, `pypi/<name>`, `nuget/<name>`,
  `gem/<name>`, and `composer/<vendor>/<package>` workspace repos, with
  hyphen/underscore import aliases for Python packages, Rust crates, and Ruby
  gems where ecosystem import names commonly differ from package names, with
  import-candidate targets preferring the matching terminal symbol when an
  import spec names a class/function such as `requests.auth.HTTPBasicAuth`,
  Kubernetes external config resource endpoints such as
  `external:config:kubernetes/service/api`,
  `external:config:kubernetes/configmap/podinfo-values`, and
  `external:config:kubernetes/secret/podinfo-secret-values`, plus custom
  resources such as `external:config:kubernetes/broker/default`,
  `external:config:kubernetes/inmemorychannel/user-events`, and
  `external:config:kubernetes/revision/user-api-00001`, plus Gateway endpoints
  such as `external:config:kubernetes/gateway/public`, resolved to
  matching local `Service.api`, `ConfigMap.podinfo-values`,
  `Secret.podinfo-secret-values`, `Broker.default`, and
  `InMemoryChannel.user-events`, `Revision.user-api-00001`, and
  `Gateway.public` resource symbols in another workspace repo,
  Docker Compose
  service config endpoints such as `external:config:compose/service/db`
  resolved to matching `compose.service.db` resource symbols in another
  workspace repo, plus exact
  `external:symbol:<qualified-name>` references, repo-prefix-qualified external
  symbol references, and package-prefix-qualified external symbols, including
  `::`-separated package symbols such as `tokio::sync::channel`, matched to
  symbols defined in another workspace repo. External-symbol matching also
  uses file-path-qualified symbol aliases such as
  `@acme/lib/api/routes.Handler` to match shallow local symbols declared in
  nested module files.
- MCP exposes `brain_workspace_graph` for the same workspace graph contracts
  and `cross_edges` JSON.
- Current semantic indexes are reused on warm refresh instead of forcing an
  unchanged provider snapshot.

## Remaining Honesty Notes

- The graph query language is a supported Cypher-style subset, not full Cypher.
- Workspace cross-repo graph edges are explicit in `graph.json`, but they are
  still derived from shared external endpoint contracts, exact route endpoint
  matches, GraphQL operation/resolver/schema-field and
  schema-field-to-resolver endpoint matches,
  Kubernetes workload/resource and Docker Compose service external config resource
  candidates, repo-key/scoped-package/GitHub monorepo package/package-key-matched unresolved import candidates with
  terminal-symbol preference, source-path/subpath target preference, and
  versioned Go module subpath normalization such as `v2/pkg` -> `pkg`,
  canonical route templates including frontend
  optional/catch-all bracket params, wildcard path params, and trailing-slash
  equivalence, and exact/repo-prefix/package-prefix
  external-symbol name matches including file-path-qualified symbol aliases and
  `::`-separated package symbols. Package-key matching covers
  cargo/gomod/maven/npm/pypi/nuget/gem/composer keys and hyphen/underscore
  aliases for Python packages, Rust crates, Ruby gems, and GitHub
  `gh/<owner>/<repo>/packages/<name>` workspace package dirs across import
  candidates and external-symbol targets, but this is still not
  full cross-repo compiler/type-aware symbol resolution.
- Runtime trace facts are now visible in `brief` and review reports, and
  influence likely-file/review ranking. Proof-ready agent-value evals remain
  open before memory-aware lift can be claimed.
