package workspace

import (
	"testing"
)

func TestWorkspaceGraphMatchesScopedPackageImportCandidates(t *testing.T) {
	edges := GraphImportCrossEdges([]RepoGraphIndex{
		{
			RepoKey: "local/app",
			Imports: []ImportRef{{
				Spec: "@acme/lib/pkg",
				Source: SymbolRef{
					RepoKey:       "local/app",
					ID:            "app:sym",
					Kind:          "function",
					Name:          "HandleAPI",
					QualifiedName: "service.HandleAPI",
					FilePath:      "service.go",
				},
				Count: 1,
			}},
		},
		{
			RepoKey: "gh/acme/lib",
			Candidates: []SymbolRef{{
				RepoKey:       "gh/acme/lib",
				ID:            "lib:sym",
				Kind:          "function",
				Name:          "Service",
				QualifiedName: "pkg.Service",
				FilePath:      "pkg/service.go",
			}},
		},
	}, 10)
	if len(edges) != 1 {
		t.Fatalf("scoped package import edges = %#v", edges)
	}
	edge := edges[0]
	if edge.RelationKind != "cross_repo_import_candidate" || edge.Endpoint != "external:import:@acme/lib/pkg" || edge.FromRepo != "local/app" || edge.ToRepo != "gh/acme/lib" || edge.ToSymbol.FilePath != "pkg/service.go" {
		t.Fatalf("unexpected scoped package import edge: %#v", edge)
	}
}

func TestWorkspaceGraphMatchesPackageRepoImportCandidates(t *testing.T) {
	edges := GraphImportCrossEdges([]RepoGraphIndex{
		{
			RepoKey: "local/app",
			Imports: []ImportRef{{
				Spec: "@acme/lib/pkg",
				Source: SymbolRef{
					RepoKey:       "local/app",
					ID:            "app:sym",
					Kind:          "function",
					Name:          "HandleAPI",
					QualifiedName: "service.HandleAPI",
					FilePath:      "service.go",
				},
				Count: 1,
			}},
		},
		{
			RepoKey: "npm/@acme/lib",
			Candidates: []SymbolRef{{
				RepoKey:       "npm/@acme/lib",
				ID:            "lib:sym",
				Kind:          "function",
				Name:          "Service",
				QualifiedName: "pkg.Service",
				FilePath:      "pkg/service.ts",
			}},
		},
	}, 10)
	if len(edges) != 1 {
		t.Fatalf("package repo import edges = %#v", edges)
	}
	edge := edges[0]
	if edge.RelationKind != "cross_repo_import_candidate" || edge.Endpoint != "external:import:@acme/lib/pkg" || edge.FromRepo != "local/app" || edge.ToRepo != "npm/@acme/lib" || edge.ToSymbol.FilePath != "pkg/service.ts" {
		t.Fatalf("unexpected package repo import edge: %#v", edge)
	}
}

func TestWorkspaceGraphMatchesGoModuleImportCandidates(t *testing.T) {
	edges := GraphImportCrossEdges([]RepoGraphIndex{
		{
			RepoKey: "local/app",
			Imports: []ImportRef{{
				Spec: "golang.org/x/sync/errgroup",
				Source: SymbolRef{
					RepoKey:       "local/app",
					ID:            "app:sym",
					Kind:          "function",
					Name:          "Run",
					QualifiedName: "service.Run",
					FilePath:      "service.go",
				},
				Count: 1,
			}},
		},
		{
			RepoKey: "gomod/golang.org/x/sync",
			Candidates: []SymbolRef{{
				RepoKey:       "gomod/golang.org/x/sync",
				ID:            "lib:sym",
				Kind:          "function",
				Name:          "WithContext",
				QualifiedName: "errgroup.WithContext",
				FilePath:      "errgroup/errgroup.go",
			}},
		},
	}, 10)
	if len(edges) != 1 {
		t.Fatalf("go module import edges = %#v", edges)
	}
	edge := edges[0]
	if edge.RelationKind != "cross_repo_import_candidate" || edge.Endpoint != "external:import:golang.org/x/sync/errgroup" || edge.FromRepo != "local/app" || edge.ToRepo != "gomod/golang.org/x/sync" || edge.ToSymbol.FilePath != "errgroup/errgroup.go" {
		t.Fatalf("unexpected go module import edge: %#v", edge)
	}
}

func TestWorkspaceGraphMatchesVersionedGoModuleImportCandidates(t *testing.T) {
	edges := GraphImportCrossEdges([]RepoGraphIndex{
		{
			RepoKey: "local/app",
			Imports: []ImportRef{{
				Spec: "github.com/acme/lib/v2/pkg",
				Source: SymbolRef{
					RepoKey:       "local/app",
					ID:            "app:sym",
					Kind:          "function",
					Name:          "Run",
					QualifiedName: "service.Run",
					FilePath:      "service.go",
				},
				Count: 1,
			}},
		},
		{
			RepoKey: "gh/acme/lib",
			Candidates: []SymbolRef{
				{
					RepoKey:       "gh/acme/lib",
					ID:            "root:file",
					Kind:          "file",
					Name:          "root",
					QualifiedName: "root",
					FilePath:      "cmd/root.go",
				},
				{
					RepoKey:       "gh/acme/lib",
					ID:            "pkg:file",
					Kind:          "file",
					Name:          "service",
					QualifiedName: "service",
					FilePath:      "pkg/service.go",
				},
			},
		},
	}, 10)
	if len(edges) != 1 {
		t.Fatalf("versioned go module import edges = %#v", edges)
	}
	edge := edges[0]
	if edge.RelationKind != "cross_repo_import_candidate" ||
		edge.Endpoint != "external:import:github.com/acme/lib/v2/pkg" ||
		edge.FromRepo != "local/app" ||
		edge.ToRepo != "gh/acme/lib" ||
		edge.ToSymbol.ID != "pkg:file" ||
		edge.ToSymbol.Direction != "import_path_target" {
		t.Fatalf("unexpected versioned go module import edge: %#v", edge)
	}
}

func TestWorkspaceGraphMatchesCargoImportCandidates(t *testing.T) {
	edges := GraphImportCrossEdges([]RepoGraphIndex{
		{
			RepoKey: "local/app",
			Imports: []ImportRef{{
				Spec: "tokio::sync",
				Source: SymbolRef{
					RepoKey:       "local/app",
					ID:            "app:sym",
					Kind:          "function",
					Name:          "run",
					QualifiedName: "app.run",
					FilePath:      "src/main.rs",
				},
				Count: 1,
			}},
		},
		{
			RepoKey: "cargo/tokio",
			Candidates: []SymbolRef{{
				RepoKey:       "cargo/tokio",
				ID:            "lib:sym",
				Kind:          "function",
				Name:          "channel",
				QualifiedName: "sync.channel",
				FilePath:      "sync/channel.rs",
			}},
		},
	}, 10)
	if len(edges) != 1 {
		t.Fatalf("cargo import edges = %#v", edges)
	}
	edge := edges[0]
	if edge.RelationKind != "cross_repo_import_candidate" || edge.Endpoint != "external:import:tokio::sync" || edge.FromRepo != "local/app" || edge.ToRepo != "cargo/tokio" || edge.ToSymbol.FilePath != "sync/channel.rs" {
		t.Fatalf("unexpected cargo import edge: %#v", edge)
	}
}

func TestWorkspaceGraphMatchesMavenImportCandidates(t *testing.T) {
	edges := GraphImportCrossEdges([]RepoGraphIndex{
		{
			RepoKey: "local/app",
			Imports: []ImportRef{{
				Spec: "com.acme.lib.Service",
				Source: SymbolRef{
					RepoKey:       "local/app",
					ID:            "app:sym",
					Kind:          "function",
					Name:          "run",
					QualifiedName: "service.run",
					FilePath:      "src/main/java/com/acme/app/App.java",
				},
				Count: 1,
			}},
		},
		{
			RepoKey: "maven/com.acme/lib",
			Candidates: []SymbolRef{{
				RepoKey:       "maven/com.acme/lib",
				ID:            "lib:sym",
				Kind:          "class",
				Name:          "Service",
				QualifiedName: "Service",
				FilePath:      "Service.java",
			}},
		},
	}, 10)
	if len(edges) != 1 {
		t.Fatalf("maven import edges = %#v", edges)
	}
	edge := edges[0]
	if edge.RelationKind != "cross_repo_import_candidate" || edge.Endpoint != "external:import:com.acme.lib.Service" || edge.FromRepo != "local/app" || edge.ToRepo != "maven/com.acme/lib" || edge.ToSymbol.FilePath != "Service.java" {
		t.Fatalf("unexpected maven import edge: %#v", edge)
	}
}

func TestWorkspaceGraphMatchesAdditionalPackageImportCandidates(t *testing.T) {
	for _, tc := range []struct {
		name          string
		spec          string
		repoKey       string
		targetName    string
		qualifiedName string
		filePath      string
	}{
		{
			name:          "nuget",
			spec:          "Newtonsoft.Json.Linq",
			repoKey:       "nuget/Newtonsoft.Json",
			targetName:    "Linq",
			qualifiedName: "Linq",
			filePath:      "Linq/JToken.cs",
		},
		{
			name:          "gem",
			spec:          "active_support/core_ext",
			repoKey:       "gem/active_support",
			targetName:    "core_ext",
			qualifiedName: "core_ext",
			filePath:      "lib/active_support/core_ext.rb",
		},
		{
			name:          "composer",
			spec:          "monolog/monolog/src/Logger",
			repoKey:       "composer/monolog/monolog",
			targetName:    "Logger",
			qualifiedName: "src/Logger",
			filePath:      "src/Logger.php",
		},
		{
			name:          "pypi hyphen underscore alias",
			spec:          "acme_client.transport",
			repoKey:       "pypi/acme-client",
			targetName:    "transport",
			qualifiedName: "transport",
			filePath:      "acme_client/transport.go",
		},
		{
			name:          "cargo hyphen underscore alias",
			spec:          "tokio_util::codec",
			repoKey:       "cargo/tokio-util",
			targetName:    "codec",
			qualifiedName: "codec",
			filePath:      "src/codec/framed.rs",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			edges := GraphImportCrossEdges([]RepoGraphIndex{
				{
					RepoKey: "local/app",
					Imports: []ImportRef{{
						Spec: tc.spec,
						Source: SymbolRef{
							RepoKey:       "local/app",
							ID:            "app:sym",
							Kind:          "function",
							Name:          "run",
							QualifiedName: "service.run",
							FilePath:      "service.go",
						},
						Count: 1,
					}},
				},
				{
					RepoKey: tc.repoKey,
					Candidates: []SymbolRef{{
						RepoKey:       tc.repoKey,
						ID:            "pkg:sym",
						Kind:          "class",
						Name:          tc.targetName,
						QualifiedName: tc.qualifiedName,
						FilePath:      tc.filePath,
					}},
				},
			}, 10)
			if len(edges) != 1 {
				t.Fatalf("%s package import edges = %#v", tc.name, edges)
			}
			edge := edges[0]
			if edge.RelationKind != "cross_repo_import_candidate" ||
				edge.Endpoint != "external:import:"+tc.spec ||
				edge.FromRepo != "local/app" ||
				edge.ToRepo != tc.repoKey ||
				edge.ToSymbol.FilePath != tc.filePath ||
				edge.ToSymbol.Direction != "import_symbol_target" {
				t.Fatalf("unexpected %s package import edge: %#v", tc.name, edge)
			}
		})
	}
}

func TestWorkspaceGraphMatchesKubernetesResourceCandidates(t *testing.T) {
	tests := []struct {
		name       string
		endpoint   string
		targetID   string
		targetName string
		filePath   string
	}{
		{
			name:       "service",
			endpoint:   "external:config:kubernetes/service/api",
			targetID:   "platform:resource:Service.api",
			targetName: "Service.api",
			filePath:   "k8s/service.yaml",
		},
		{
			name:       "configmap",
			endpoint:   "external:config:kubernetes/configmap/podinfo-values",
			targetID:   "platform:resource:ConfigMap.podinfo-values",
			targetName: "ConfigMap.podinfo-values",
			filePath:   "k8s/podinfo-values.yaml",
		},
		{
			name:       "secret",
			endpoint:   "external:config:kubernetes/secret/podinfo-secret-values",
			targetID:   "platform:resource:Secret.podinfo-secret-values",
			targetName: "Secret.podinfo-secret-values",
			filePath:   "k8s/podinfo-secret-values.yaml",
		},
		{
			name:       "namespaced-secret-qualified",
			endpoint:   "external:config:kubernetes/secret/default/podinfo-secret-values",
			targetID:   "platform:resource:Secret.default.podinfo-secret-values",
			targetName: "Secret.default.podinfo-secret-values",
			filePath:   "k8s/default-podinfo-secret-values.yaml",
		},
		{
			name:       "namespaced-secret-short-fallback",
			endpoint:   "external:config:kubernetes/secret/default/podinfo-secret-values",
			targetID:   "platform:resource:Secret.podinfo-secret-values",
			targetName: "Secret.podinfo-secret-values",
			filePath:   "k8s/podinfo-secret-values.yaml",
		},
		{
			name:       "custom-resource",
			endpoint:   "external:config:kubernetes/broker/default",
			targetID:   "platform:resource:Broker.default",
			targetName: "Broker.default",
			filePath:   "k8s/knative-broker.yaml",
		},
		{
			name:       "custom-resource-channel",
			endpoint:   "external:config:kubernetes/inmemorychannel/user-events",
			targetID:   "platform:resource:InMemoryChannel.user-events",
			targetName: "InMemoryChannel.user-events",
			filePath:   "k8s/knative-channel.yaml",
		},
		{
			name:       "custom-resource-revision",
			endpoint:   "external:config:kubernetes/revision/user-api-00001",
			targetID:   "platform:resource:Revision.user-api-00001",
			targetName: "Revision.user-api-00001",
			filePath:   "k8s/knative-revision.yaml",
		},
		{
			name:       "gateway",
			endpoint:   "external:config:kubernetes/gateway/public",
			targetID:   "platform:resource:Gateway.public",
			targetName: "Gateway.public",
			filePath:   "k8s/gateway.yaml",
		},
		{
			name:       "vpa-workload-target",
			endpoint:   "external:config:kubernetes/deployment/api",
			targetID:   "platform:resource:Deployment.api",
			targetName: "Deployment.api",
			filePath:   "k8s/deployment.yaml",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			edges := GraphResourceCrossEdges(map[string]*ExternalContractAggregate{
				"RESOURCE_DEPENDS_ON\x00" + tc.endpoint: {
					Endpoint:   tc.endpoint,
					Type:       "RESOURCE_DEPENDS_ON",
					RepoCounts: map[string]int{"local/app": 1},
					Participants: []SymbolRef{{
						RepoKey:       "local/app",
						ID:            "app:resource:Deployment.api",
						Kind:          "resource",
						Name:          "Deployment.api",
						QualifiedName: "Deployment.api",
						FilePath:      "k8s/deployment.yaml",
						Direction:     "to_endpoint",
						Count:         1,
					}},
				},
			}, []RepoGraphIndex{
				{
					RepoKey: "local/app",
				},
				{
					RepoKey: "local/platform",
					Candidates: []SymbolRef{{
						RepoKey:       "local/platform",
						ID:            tc.targetID,
						Kind:          "resource",
						Name:          tc.targetName,
						QualifiedName: tc.targetName,
						FilePath:      tc.filePath,
					}},
				},
			}, 10)
			if len(edges) != 1 {
				t.Fatalf("kubernetes resource candidate edges = %#v", edges)
			}
			edge := edges[0]
			if edge.RelationKind != "cross_repo_resource_candidate" ||
				edge.Endpoint != tc.endpoint ||
				edge.Type != "RESOURCE_DEPENDS_ON" ||
				edge.FromRepo != "local/app" ||
				edge.ToRepo != "local/platform" ||
				edge.ToSymbol.ID != tc.targetID ||
				edge.ToSymbol.Direction != "external_resource_target" {
				t.Fatalf("unexpected kubernetes resource candidate edge: %#v", edge)
			}
		})
	}
}

func TestWorkspaceGraphMatchesComposeServiceResourceCandidates(t *testing.T) {
	edges := GraphResourceCrossEdges(map[string]*ExternalContractAggregate{
		"RESOURCE_DEPENDS_ON\x00external:config:compose/service/db": {
			Endpoint:   "external:config:compose/service/db",
			Type:       "RESOURCE_DEPENDS_ON",
			RepoCounts: map[string]int{"local/app": 1},
			Participants: []SymbolRef{{
				RepoKey:       "local/app",
				ID:            "app:resource:compose.service.api",
				Kind:          "resource",
				Name:          "compose.service.api",
				QualifiedName: "compose.service.api",
				FilePath:      "compose.yaml",
				Direction:     "to_endpoint",
				Count:         1,
			}},
		},
	}, []RepoGraphIndex{
		{
			RepoKey: "local/app",
		},
		{
			RepoKey: "local/platform",
			Candidates: []SymbolRef{{
				RepoKey:       "local/platform",
				ID:            "platform:resource:compose.service.db",
				Kind:          "resource",
				Name:          "compose.service.db",
				QualifiedName: "compose.service.db",
				FilePath:      "docker-compose.yml",
			}},
		},
	}, 10)
	if len(edges) != 1 {
		t.Fatalf("compose resource candidate edges = %#v", edges)
	}
	edge := edges[0]
	if edge.RelationKind != "cross_repo_resource_candidate" ||
		edge.Endpoint != "external:config:compose/service/db" ||
		edge.Type != "RESOURCE_DEPENDS_ON" ||
		edge.FromRepo != "local/app" ||
		edge.ToRepo != "local/platform" ||
		edge.ToSymbol.ID != "platform:resource:compose.service.db" ||
		edge.ToSymbol.Direction != "external_resource_target" {
		t.Fatalf("unexpected compose resource candidate edge: %#v", edge)
	}
}

func TestWorkspaceGraphReportsCrossRepoChannelFlows(t *testing.T) {
	edges := GraphChannelCrossEdges(map[string]*ExternalContractAggregate{
		"EMITS\x00external:channel:user.created": {
			Endpoint:   "external:channel:user.created",
			Type:       "EMITS",
			RepoCounts: map[string]int{"local/api": 1},
			Participants: []SymbolRef{{
				RepoKey:       "local/api",
				ID:            "api:sym:createUser",
				Kind:          "function",
				Name:          "createUser",
				QualifiedName: "users.createUser",
				FilePath:      "src/users.ts",
				Direction:     "to_endpoint",
				Count:         1,
			}},
		},
		"LISTENS_ON\x00external:channel:user.created": {
			Endpoint:   "external:channel:user.created",
			Type:       "LISTENS_ON",
			RepoCounts: map[string]int{"local/worker": 1, "local/api": 1},
			Participants: []SymbolRef{
				{
					RepoKey:       "local/worker",
					ID:            "worker:sym:onUserCreated",
					Kind:          "function",
					Name:          "onUserCreated",
					QualifiedName: "events.onUserCreated",
					FilePath:      "src/events.ts",
					Direction:     "to_endpoint",
					Count:         1,
				},
				{
					RepoKey:       "local/api",
					ID:            "api:sym:localAudit",
					Kind:          "function",
					Name:          "localAudit",
					QualifiedName: "users.localAudit",
					FilePath:      "src/users.ts",
					Direction:     "to_endpoint",
					Count:         1,
				},
			},
		},
	}, 10)
	if len(edges) != 1 {
		t.Fatalf("channel flow edges = %#v", edges)
	}
	edge := edges[0]
	if edge.RelationKind != "cross_repo_channel_flow" ||
		edge.Endpoint != "external:channel:user.created" ||
		edge.Type != "EMITS" ||
		edge.FromRepo != "local/api" ||
		edge.ToRepo != "local/worker" ||
		edge.FromSymbol.QualifiedName != "users.createUser" ||
		edge.ToSymbol.QualifiedName != "events.onUserCreated" {
		t.Fatalf("unexpected channel flow edge: %#v", edge)
	}
}

func TestWorkspaceGraphReportsCrossRepoGraphQLCalls(t *testing.T) {
	edges := GraphGraphQLCrossEdges(map[string]*ExternalContractAggregate{
		"HANDLES_GRAPHQL\x00external:graphql:query user": {
			Endpoint:   "external:graphql:query user",
			Type:       "HANDLES_GRAPHQL",
			RepoCounts: map[string]int{"local/web": 1, "local/api": 1},
			Participants: []SymbolRef{
				{
					RepoKey:       "local/web",
					ID:            "web:sym:fetchUser",
					Kind:          "function",
					Name:          "fetchUser",
					QualifiedName: "client.fetchUser",
					FilePath:      "src/client.ts",
					Direction:     "to_endpoint",
					Count:         1,
				},
				{
					RepoKey:       "local/schema",
					ID:            "schema:sym:Query.user",
					Kind:          "graphql_schema_field",
					Name:          "Query.user",
					QualifiedName: "Query.user",
					FilePath:      "schema.graphql",
					Direction:     "to_endpoint",
					Count:         1,
				},
				{
					RepoKey:       "local/api",
					ID:            "api:sym:Query.user",
					Kind:          "graphql_resolver",
					Name:          "Query.user",
					QualifiedName: "Query.user",
					FilePath:      "src/resolvers.ts",
					Direction:     "to_endpoint",
					Count:         1,
				},
			},
		},
		"HANDLES_GRAPHQL\x00external:graphql:query viewer": {
			Endpoint:   "external:graphql:query viewer",
			Type:       "HANDLES_GRAPHQL",
			RepoCounts: map[string]int{"local/web": 1, "local/other": 1},
			Participants: []SymbolRef{
				{RepoKey: "local/web", ID: "web:sym:fetchViewer", Kind: "function", Name: "fetchViewer", Count: 1},
				{RepoKey: "local/other", ID: "other:sym:fetchViewer", Kind: "function", Name: "fetchViewer", Count: 1},
			},
		},
	}, 10)
	if len(edges) != 3 {
		t.Fatalf("graphql cross edges = %#v", edges)
	}
	seen := map[string]CrossEdge{}
	for _, edge := range edges {
		seen[edge.RelationKind+"->"+edge.ToSymbol.Kind] = edge
		if edge.FromSymbol.Kind == "graphql_schema_field" && edge.RelationKind != "cross_repo_graphql_schema_resolver" {
			t.Fatalf("schema field was used as GraphQL operation source: %#v", edge)
		}
	}
	resolverEdge := seen["cross_repo_graphql_call->graphql_resolver"]
	if resolverEdge.Endpoint != "external:graphql:query user" ||
		resolverEdge.Type != "CALLS" ||
		resolverEdge.FromRepo != "local/web" ||
		resolverEdge.ToRepo != "local/api" {
		t.Fatalf("unexpected GraphQL resolver cross edge: %#v", resolverEdge)
	}
	schemaEdge := seen["cross_repo_graphql_schema->graphql_schema_field"]
	if schemaEdge.Endpoint != "external:graphql:query user" ||
		schemaEdge.Type != "CALLS" ||
		schemaEdge.FromRepo != "local/web" ||
		schemaEdge.ToRepo != "local/schema" {
		t.Fatalf("unexpected GraphQL schema cross edge: %#v", schemaEdge)
	}
	schemaResolverEdge := seen["cross_repo_graphql_schema_resolver->graphql_resolver"]
	if schemaResolverEdge.Endpoint != "external:graphql:query user" ||
		schemaResolverEdge.Type != "CALLS" ||
		schemaResolverEdge.FromRepo != "local/schema" ||
		schemaResolverEdge.ToRepo != "local/api" {
		t.Fatalf("unexpected GraphQL schema resolver cross edge: %#v", schemaResolverEdge)
	}
}

func TestWorkspaceGraphReportsCrossRepoGraphQLSchemaResolverWithoutOperation(t *testing.T) {
	edges := GraphGraphQLCrossEdges(map[string]*ExternalContractAggregate{
		"HANDLES_GRAPHQL\x00external:graphql:query user": {
			Endpoint:   "external:graphql:query user",
			Type:       "HANDLES_GRAPHQL",
			RepoCounts: map[string]int{"local/schema": 1, "local/api": 1},
			Participants: []SymbolRef{
				{
					RepoKey:       "local/schema",
					ID:            "schema:sym:Query.user",
					Kind:          "graphql_schema_field",
					Name:          "Query.user",
					QualifiedName: "Query.user",
					FilePath:      "schema.graphql",
					Direction:     "to_endpoint",
					Count:         1,
				},
				{
					RepoKey:       "local/api",
					ID:            "api:sym:Query.user",
					Kind:          "graphql_resolver",
					Name:          "Query.user",
					QualifiedName: "Query.user",
					FilePath:      "src/user.resolvers.ts",
					Direction:     "to_endpoint",
					Count:         1,
				},
			},
		},
	}, 10)
	if len(edges) != 1 {
		t.Fatalf("graphql schema resolver-only cross edges = %#v", edges)
	}
	edge := edges[0]
	if edge.Endpoint != "external:graphql:query user" ||
		edge.Type != "CALLS" ||
		edge.RelationKind != "cross_repo_graphql_schema_resolver" ||
		edge.FromRepo != "local/schema" ||
		edge.ToRepo != "local/api" ||
		edge.FromSymbol.Kind != "graphql_schema_field" ||
		edge.ToSymbol.Kind != "graphql_resolver" {
		t.Fatalf("unexpected GraphQL schema resolver-only cross edge: %#v", edge)
	}
}

func TestWorkspaceGraphImportCandidatesPreferImportedSymbols(t *testing.T) {
	edges := GraphImportCrossEdges([]RepoGraphIndex{
		{
			RepoKey: "local/app",
			Imports: []ImportRef{{
				Spec: "requests.auth.HTTPBasicAuth",
				Source: SymbolRef{
					RepoKey:       "local/app",
					ID:            "app:sym",
					Kind:          "function",
					Name:          "run",
					QualifiedName: "service.run",
					FilePath:      "service.go",
				},
				Count: 1,
			}},
		},
		{
			RepoKey: "pypi/requests",
			Candidates: []SymbolRef{
				{
					RepoKey:       "pypi/requests",
					ID:            "module:sym",
					Kind:          "module",
					Name:          "auth",
					QualifiedName: "auth",
					FilePath:      "requests/auth.go",
				},
				{
					RepoKey:       "pypi/requests",
					ID:            "class:sym",
					Kind:          "class",
					Name:          "HTTPBasicAuth",
					QualifiedName: "auth.HTTPBasicAuth",
					FilePath:      "requests/auth.go",
				},
			},
		},
	}, 10)
	if len(edges) != 1 {
		t.Fatalf("Go import symbol edges = %#v", edges)
	}
	edge := edges[0]
	if edge.RelationKind != "cross_repo_import_candidate" ||
		edge.Endpoint != "external:import:requests.auth.HTTPBasicAuth" ||
		edge.ToRepo != "pypi/requests" ||
		edge.ToSymbol.ID != "class:sym" ||
		edge.ToSymbol.Direction != "import_symbol_target" {
		t.Fatalf("unexpected Go import symbol edge: %#v", edge)
	}
}

func TestWorkspaceGraphImportCandidatesPreferSubpathFileTargets(t *testing.T) {
	edges := GraphImportCrossEdges([]RepoGraphIndex{
		{
			RepoKey: "local/app",
			Imports: []ImportRef{{
				Spec: "@acme/ui/button",
				Source: SymbolRef{
					RepoKey:       "local/app",
					ID:            "app:sym",
					Kind:          "function",
					Name:          "render",
					QualifiedName: "app.render",
					FilePath:      "src/app.ts",
				},
				Count: 1,
			}},
		},
		{
			RepoKey: "gh/acme/web/packages/ui",
			Candidates: []SymbolRef{
				{
					RepoKey:       "gh/acme/web/packages/ui",
					ID:            "alpha:file",
					Kind:          "file",
					Name:          "alpha",
					QualifiedName: "alpha",
					FilePath:      "src/alpha.ts",
				},
				{
					RepoKey:       "gh/acme/web/packages/ui",
					ID:            "button:file",
					Kind:          "file",
					Name:          "button",
					QualifiedName: "button",
					FilePath:      "src/button.ts",
				},
			},
		},
	}, 10)
	if len(edges) != 1 {
		t.Fatalf("subpath import file edges = %#v", edges)
	}
	edge := edges[0]
	if edge.RelationKind != "cross_repo_import_candidate" ||
		edge.Endpoint != "external:import:@acme/ui/button" ||
		edge.ToRepo != "gh/acme/web/packages/ui" ||
		edge.ToSymbol.ID != "button:file" ||
		edge.ToSymbol.Direction != "import_path_target" {
		t.Fatalf("unexpected subpath import file edge: %#v", edge)
	}
}

func TestWorkspaceImportPathMatchesSubpath(t *testing.T) {
	tests := []struct {
		path    string
		subpath string
		want    bool
	}{
		{path: "src/button.ts", subpath: "button", want: true},
		{path: "src/api/routes.ts", subpath: "api/routes", want: true},
		{path: "src/api/routes.ts", subpath: "routes", want: true},
		{path: "requests/auth.go", subpath: "auth", want: true},
		{path: "tokio/sync/channel.rs", subpath: "sync::channel", want: true},
		{path: "pkg/service.go", subpath: "v2/pkg", want: true},
		{path: "src/alpha.ts", subpath: "button", want: false},
	}
	for _, tt := range tests {
		if got := importPathMatchesSubpath(tt.path, tt.subpath); got != tt.want {
			t.Fatalf("workspaceImportPathMatchesSubpath(%q, %q) = %v, want %v", tt.path, tt.subpath, got, tt.want)
		}
	}
}

func TestWorkspaceGraphMatchesCanonicalRouteTemplateCalls(t *testing.T) {
	edges := GraphRouteCallCrossEdges(map[string]*ExternalContractAggregate{
		"HTTP_CALLS\x00external:route:/api/users/{id}": {
			Endpoint: "external:route:/api/users/{id}",
			Type:     "HTTP_CALLS",
			Participants: []SymbolRef{{
				RepoKey:       "local/client",
				ID:            "local/client:go:client.go:function:CallUser",
				Kind:          "function",
				Name:          "CallUser",
				QualifiedName: "client.CallUser",
				Count:         1,
			}},
		},
		"HANDLES_ROUTE\x00external:route:/api/users/:userID": {
			Endpoint: "external:route:/api/users/:userID",
			Type:     "HANDLES_ROUTE",
			Participants: []SymbolRef{{
				RepoKey:       "local/service",
				ID:            "local/service:ts:routes.ts:function:showUser",
				Kind:          "function",
				Name:          "showUser",
				QualifiedName: "routes.showUser",
				Count:         1,
			}},
		},
		"HANDLES_ROUTE\x00external:route:/api/projects/<project_id>": {
			Endpoint: "external:route:/api/projects/<project_id>",
			Type:     "HANDLES_ROUTE",
			Participants: []SymbolRef{{
				RepoKey:       "local/other",
				ID:            "local/other:go:routes.go:function:show_project",
				Kind:          "function",
				Name:          "show_project",
				QualifiedName: "routes.show_project",
				Count:         1,
			}},
		},
	}, 10)
	if len(edges) != 1 {
		t.Fatalf("expected exactly one canonical route edge, got %#v", edges)
	}
	edge := edges[0]
	if edge.Endpoint != "external:route:/api/users/{param}" ||
		edge.RelationKind != "cross_repo_route_call" ||
		edge.FromRepo != "local/client" ||
		edge.ToRepo != "local/service" ||
		edge.FromSymbol.QualifiedName != "client.CallUser" ||
		edge.ToSymbol.QualifiedName != "routes.showUser" {
		t.Fatalf("unexpected canonical route edge: %#v", edge)
	}
}

func TestWorkspaceGraphMatchesTrailingSlashRouteCalls(t *testing.T) {
	edges := GraphRouteCallCrossEdges(map[string]*ExternalContractAggregate{
		"HTTP_CALLS\x00external:route:/health": {
			Endpoint: "external:route:/health",
			Type:     "HTTP_CALLS",
			Participants: []SymbolRef{{
				RepoKey:       "local/client",
				ID:            "local/client:go:client.go:function:CheckHealth",
				Kind:          "function",
				Name:          "CheckHealth",
				QualifiedName: "client.CheckHealth",
				Count:         1,
			}},
		},
		"HANDLES_ROUTE\x00external:route:/health/": {
			Endpoint: "external:route:/health/",
			Type:     "HANDLES_ROUTE",
			Participants: []SymbolRef{{
				RepoKey:       "local/service",
				ID:            "local/service:go:routes.go:function:health",
				Kind:          "function",
				Name:          "health",
				QualifiedName: "routes.health",
				Count:         1,
			}},
		},
	}, 10)
	if len(edges) != 1 {
		t.Fatalf("expected trailing-slash route edge, got %#v", edges)
	}
	edge := edges[0]
	if edge.Endpoint != "external:route:/health" ||
		edge.RelationKind != "cross_repo_route_call" ||
		edge.FromRepo != "local/client" ||
		edge.ToRepo != "local/service" ||
		edge.FromSymbol.QualifiedName != "client.CheckHealth" ||
		edge.ToSymbol.QualifiedName != "routes.health" {
		t.Fatalf("unexpected trailing-slash route edge: %#v", edge)
	}
}

func TestWorkspaceGraphMatchesFrontendOptionalRouteTemplateCalls(t *testing.T) {
	edges := GraphRouteCallCrossEdges(map[string]*ExternalContractAggregate{
		"HTTP_CALLS\x00external:route:/docs/{lang}": {
			Endpoint: "external:route:/docs/{lang}",
			Type:     "HTTP_CALLS",
			Participants: []SymbolRef{{
				RepoKey:       "local/client",
				ID:            "local/client:ts:client.ts:function:loadDocs",
				Kind:          "function",
				Name:          "loadDocs",
				QualifiedName: "client.loadDocs",
				Count:         1,
			}},
		},
		"HANDLES_ROUTE\x00external:route:/docs/[[lang]]": {
			Endpoint: "external:route:/docs/[[lang]]",
			Type:     "HANDLES_ROUTE",
			Participants: []SymbolRef{{
				RepoKey:       "local/web",
				ID:            "local/web:ts:src/routes/docs/[[lang]]/+server.ts:function:GET",
				Kind:          "function",
				Name:          "GET",
				QualifiedName: "routes.docs.GET",
				Count:         1,
			}},
		},
		"HTTP_CALLS\x00external:route:/blog/{slug}": {
			Endpoint: "external:route:/blog/{slug}",
			Type:     "HTTP_CALLS",
			Participants: []SymbolRef{{
				RepoKey:       "local/client",
				ID:            "local/client:ts:client.ts:function:loadBlog",
				Kind:          "function",
				Name:          "loadBlog",
				QualifiedName: "client.loadBlog",
				Count:         1,
			}},
		},
		"HANDLES_ROUTE\x00external:route:/blog/[...slug]": {
			Endpoint: "external:route:/blog/[...slug]",
			Type:     "HANDLES_ROUTE",
			Participants: []SymbolRef{{
				RepoKey:       "local/web",
				ID:            "local/web:ts:src/routes/blog/[...slug]/+server.ts:function:GET",
				Kind:          "function",
				Name:          "GET",
				QualifiedName: "routes.blog.GET",
				Count:         1,
			}},
		},
	}, 10)
	if len(edges) != 2 {
		t.Fatalf("expected frontend route template edges, got %#v", edges)
	}
	seen := map[string]CrossEdge{}
	for _, edge := range edges {
		seen[edge.Endpoint] = edge
	}
	if edge := seen["external:route:/docs/{param}"]; edge.RelationKind != "cross_repo_route_call" || edge.FromRepo != "local/client" || edge.ToRepo != "local/web" || edge.ToSymbol.QualifiedName != "routes.docs.GET" {
		t.Fatalf("unexpected optional frontend route edge: %#v", edge)
	}
	if edge := seen["external:route:/blog/{param}"]; edge.RelationKind != "cross_repo_route_call" || edge.FromRepo != "local/client" || edge.ToRepo != "local/web" || edge.ToSymbol.QualifiedName != "routes.blog.GET" {
		t.Fatalf("unexpected catch-all frontend route edge: %#v", edge)
	}
}

func TestWorkspaceGraphMatchesWildcardRouteTemplateCalls(t *testing.T) {
	edges := GraphRouteCallCrossEdges(map[string]*ExternalContractAggregate{
		"HTTP_CALLS\x00external:route:/assets/{path}": {
			Endpoint: "external:route:/assets/{path}",
			Type:     "HTTP_CALLS",
			Participants: []SymbolRef{{
				RepoKey:       "local/client",
				ID:            "local/client:rb:client.rb:method:fetch_asset",
				Kind:          "method",
				Name:          "fetch_asset",
				QualifiedName: "Client.fetch_asset",
				Count:         1,
			}},
		},
		"HANDLES_ROUTE\x00external:route:/assets/*path": {
			Endpoint: "external:route:/assets/*path",
			Type:     "HANDLES_ROUTE",
			Participants: []SymbolRef{{
				RepoKey:       "local/web",
				ID:            "local/web:rb:config/routes.rb:method:AssetsController.show",
				Kind:          "method",
				Name:          "show",
				QualifiedName: "AssetsController.show",
				Count:         1,
			}},
		},
	}, 10)
	if len(edges) != 1 {
		t.Fatalf("expected wildcard route edge, got %#v", edges)
	}
	edge := edges[0]
	if edge.Endpoint != "external:route:/assets/{param}" ||
		edge.RelationKind != "cross_repo_route_call" ||
		edge.FromRepo != "local/client" ||
		edge.ToRepo != "local/web" ||
		edge.ToSymbol.QualifiedName != "AssetsController.show" {
		t.Fatalf("unexpected wildcard route edge: %#v", edge)
	}
}

func TestWorkspaceGraphMatchesPackageRepoExternalSymbols(t *testing.T) {
	edges := GraphExternalSymbolCrossEdges([]RepoGraphIndex{
		{
			RepoKey: "local/app",
			ExternalSymbols: []ExternalSymbolRef{{
				Spec: "@acme/lib/pkg.Service",
				Type: "CALLS",
				Source: SymbolRef{
					RepoKey:       "local/app",
					ID:            "app:sym",
					Kind:          "function",
					Name:          "HandleAPI",
					QualifiedName: "service.HandleAPI",
					FilePath:      "service.go",
				},
				Count: 1,
			}},
		},
		{
			RepoKey: "npm/@acme/lib",
			Candidates: []SymbolRef{{
				RepoKey:       "npm/@acme/lib",
				ID:            "lib:sym",
				Kind:          "function",
				Name:          "Service",
				QualifiedName: "pkg.Service",
				FilePath:      "pkg/service.ts",
			}},
		},
	}, 10)
	if len(edges) != 1 {
		t.Fatalf("package repo external symbol edges = %#v", edges)
	}
	edge := edges[0]
	if edge.RelationKind != "cross_repo_external_symbol" || edge.Endpoint != "external:symbol:@acme/lib/pkg.Service" || edge.FromRepo != "local/app" || edge.ToRepo != "npm/@acme/lib" || edge.ToSymbol.QualifiedName != "pkg.Service" {
		t.Fatalf("unexpected package repo external symbol edge: %#v", edge)
	}
}

func TestWorkspaceGraphMatchesMavenExternalSymbols(t *testing.T) {
	edges := GraphExternalSymbolCrossEdges([]RepoGraphIndex{
		{
			RepoKey: "local/app",
			ExternalSymbols: []ExternalSymbolRef{{
				Spec: "com.acme.lib.Service",
				Type: "CALLS",
				Source: SymbolRef{
					RepoKey:       "local/app",
					ID:            "app:sym",
					Kind:          "function",
					Name:          "Run",
					QualifiedName: "service.Run",
					FilePath:      "App.java",
				},
				Count: 1,
			}},
		},
		{
			RepoKey: "maven/com.acme/lib",
			Candidates: []SymbolRef{{
				RepoKey:       "maven/com.acme/lib",
				ID:            "lib:sym",
				Kind:          "class",
				Name:          "Service",
				QualifiedName: "Service",
				FilePath:      "Service.java",
			}},
		},
	}, 10)
	if len(edges) != 1 {
		t.Fatalf("maven external symbol edges = %#v", edges)
	}
	edge := edges[0]
	if edge.RelationKind != "cross_repo_external_symbol" || edge.Endpoint != "external:symbol:com.acme.lib.Service" || edge.FromRepo != "local/app" || edge.ToRepo != "maven/com.acme/lib" || edge.ToSymbol.QualifiedName != "Service" {
		t.Fatalf("unexpected maven external symbol edge: %#v", edge)
	}
}

func TestWorkspaceGraphMatchesAdditionalPackageExternalSymbols(t *testing.T) {
	for _, tc := range []struct {
		name          string
		spec          string
		repoKey       string
		targetName    string
		qualifiedName string
		filePath      string
	}{
		{
			name:          "nuget",
			spec:          "Newtonsoft.Json.Linq.JToken",
			repoKey:       "nuget/Newtonsoft.Json",
			targetName:    "JToken",
			qualifiedName: "Linq.JToken",
			filePath:      "Src/Newtonsoft.Json/Linq/JToken.cs",
		},
		{
			name:          "pypi hyphen underscore alias",
			spec:          "acme_client.transport.Client",
			repoKey:       "pypi/acme-client",
			targetName:    "Client",
			qualifiedName: "transport.Client",
			filePath:      "acme_client/transport.go",
		},
		{
			name:          "cargo hyphen underscore alias",
			spec:          "tokio_util::codec::Framed",
			repoKey:       "cargo/tokio-util",
			targetName:    "Framed",
			qualifiedName: "codec.Framed",
			filePath:      "src/codec/framed.rs",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			edges := GraphExternalSymbolCrossEdges([]RepoGraphIndex{
				{
					RepoKey: "local/app",
					ExternalSymbols: []ExternalSymbolRef{{
						Spec: tc.spec,
						Type: "CALLS",
						Source: SymbolRef{
							RepoKey:       "local/app",
							ID:            "app:sym",
							Kind:          "function",
							Name:          "Run",
							QualifiedName: "service.Run",
							FilePath:      "Program.cs",
						},
						Count: 1,
					}},
				},
				{
					RepoKey: tc.repoKey,
					Candidates: []SymbolRef{{
						RepoKey:       tc.repoKey,
						ID:            "pkg:sym",
						Kind:          "class",
						Name:          tc.targetName,
						QualifiedName: tc.qualifiedName,
						FilePath:      tc.filePath,
					}},
				},
			}, 10)
			if len(edges) != 1 {
				t.Fatalf("%s external symbol edges = %#v", tc.name, edges)
			}
			edge := edges[0]
			if edge.RelationKind != "cross_repo_external_symbol" ||
				edge.Endpoint != "external:symbol:"+tc.spec ||
				edge.FromRepo != "local/app" ||
				edge.ToRepo != tc.repoKey ||
				edge.ToSymbol.QualifiedName != tc.qualifiedName ||
				edge.ToSymbol.Direction != "external_symbol_target" {
				t.Fatalf("unexpected %s external symbol edge: %#v", tc.name, edge)
			}
		})
	}
}

func TestWorkspaceGraphMatchesColonSeparatedPackageExternalSymbols(t *testing.T) {
	edges := GraphExternalSymbolCrossEdges([]RepoGraphIndex{
		{
			RepoKey: "local/app",
			ExternalSymbols: []ExternalSymbolRef{{
				Spec: "tokio::sync::channel",
				Type: "CALLS",
				Source: SymbolRef{
					RepoKey:       "local/app",
					ID:            "app:sym",
					Kind:          "function",
					Name:          "run",
					QualifiedName: "app.run",
					FilePath:      "src/main.rs",
				},
				Count: 1,
			}},
		},
		{
			RepoKey: "cargo/tokio",
			Candidates: []SymbolRef{{
				RepoKey:       "cargo/tokio",
				ID:            "tokio:sym",
				Kind:          "function",
				Name:          "channel",
				QualifiedName: "sync.channel",
				FilePath:      "tokio/src/sync/channel.rs",
			}},
		},
	}, 10)
	if len(edges) != 1 {
		t.Fatalf("colon-separated package external symbol edges = %#v", edges)
	}
	edge := edges[0]
	if edge.RelationKind != "cross_repo_external_symbol" ||
		edge.Endpoint != "external:symbol:tokio::sync::channel" ||
		edge.FromRepo != "local/app" ||
		edge.ToRepo != "cargo/tokio" ||
		edge.ToSymbol.QualifiedName != "sync.channel" {
		t.Fatalf("unexpected colon-separated package external symbol edge: %#v", edge)
	}
}

func TestWorkspaceExternalSymbolTargetMatchesGitHubRepoPrefixes(t *testing.T) {
	target, ok := ExternalSymbolTarget([]SymbolRef{{
		RepoKey:       "gh/acme/lib",
		ID:            "sym",
		Kind:          "function",
		Name:          "Service",
		QualifiedName: "pkg.Service",
		FilePath:      "pkg/service.go",
	}}, "github.com/acme/lib/pkg.Service")
	if !ok || target.ID != "sym" || target.Direction != "external_symbol_target" {
		t.Fatalf("github-prefixed external symbol target = %#v, %v", target, ok)
	}
}

func TestWorkspaceExternalSymbolTargetMatchesGitHubMonorepoPackagePrefixes(t *testing.T) {
	target, ok := ExternalSymbolTarget([]SymbolRef{{
		RepoKey:       "gh/acme/web/packages/ui",
		ID:            "sym",
		Kind:          "function",
		Name:          "Button",
		QualifiedName: "button.Button",
		FilePath:      "src/button.ts",
	}}, "@acme/ui/button.Button")
	if !ok || target.ID != "sym" || target.Direction != "external_symbol_target" {
		t.Fatalf("github monorepo package external symbol target = %#v, %v", target, ok)
	}
}

func TestWorkspaceExternalSymbolTargetMatchesGitHubMonorepoPackageAliases(t *testing.T) {
	target, ok := ExternalSymbolTarget([]SymbolRef{{
		RepoKey:       "gh/acme/web/packages/my-lib",
		ID:            "sym",
		Kind:          "function",
		Name:          "Button",
		QualifiedName: "button.Button",
		FilePath:      "src/button.ts",
	}}, "@acme/my_lib/button.Button")
	if !ok || target.ID != "sym" || target.Direction != "external_symbol_target" {
		t.Fatalf("github monorepo package alias external symbol target = %#v, %v", target, ok)
	}
}

func TestWorkspaceExternalSymbolTargetMatchesPackageRepoPrefixes(t *testing.T) {
	target, ok := ExternalSymbolTarget([]SymbolRef{{
		RepoKey:       "npm/@acme/lib",
		ID:            "sym",
		Kind:          "function",
		Name:          "Service",
		QualifiedName: "pkg.Service",
		FilePath:      "pkg/service.ts",
	}}, "@acme/lib/pkg.Service")
	if !ok || target.ID != "sym" || target.Direction != "external_symbol_target" {
		t.Fatalf("package-prefixed external symbol target = %#v, %v", target, ok)
	}
}

func TestWorkspaceExternalSymbolTargetMatchesFileQualifiedSymbols(t *testing.T) {
	target, ok := ExternalSymbolTarget([]SymbolRef{
		{
			RepoKey:       "npm/@acme/lib",
			ID:            "broad",
			Kind:          "function",
			Name:          "Handler",
			QualifiedName: "other.Handler",
			FilePath:      "src/other.ts",
		},
		{
			RepoKey:       "npm/@acme/lib",
			ID:            "target",
			Kind:          "function",
			Name:          "Handler",
			QualifiedName: "Handler",
			FilePath:      "src/api/routes.ts",
		},
	}, "@acme/lib/api/routes.Handler")
	if !ok || target.ID != "target" || target.Direction != "external_symbol_target" {
		t.Fatalf("file-qualified external symbol target = %#v, %v", target, ok)
	}
}

func TestWorkspaceExternalSymbolTargetMatchesColonSeparatedPackagePrefixes(t *testing.T) {
	target, ok := ExternalSymbolTarget([]SymbolRef{{
		RepoKey:       "cargo/tokio",
		ID:            "sym",
		Kind:          "function",
		Name:          "channel",
		QualifiedName: "sync.channel",
		FilePath:      "src/sync/channel.rs",
	}}, "tokio::sync::channel")
	if !ok || target.ID != "sym" || target.Direction != "external_symbol_target" {
		t.Fatalf("colon-separated package external symbol target = %#v, %v", target, ok)
	}
}

func TestWorkspaceImportMatchesGitHubRepoKeys(t *testing.T) {
	subpath, ok := importMatchesRepo("github.com/acme/lib/pkg/sub", "gh/acme/lib")
	if !ok || subpath != "pkg/sub" {
		t.Fatalf("github import match = %q, %v", subpath, ok)
	}
	subpath, ok = importMatchesRepo("acme/lib/pkg", "gh/acme/lib")
	if !ok || subpath != "pkg" {
		t.Fatalf("owner/repo import match = %q, %v", subpath, ok)
	}
	subpath, ok = importMatchesRepo("@acme/lib/pkg", "gh/acme/lib")
	if !ok || subpath != "pkg" {
		t.Fatalf("scoped package import match = %q, %v", subpath, ok)
	}
	if _, ok := importMatchesRepo("github.com/acme/other/pkg", "gh/acme/lib"); ok {
		t.Fatalf("unrelated repo import should not match")
	}
	subpath, ok = importMatchesRepo("@acme/ui/button", "gh/acme/web/packages/ui")
	if !ok || subpath != "button" {
		t.Fatalf("github monorepo package import match = %q, %v", subpath, ok)
	}
	subpath, ok = importMatchesRepo("github.com/acme/web/packages/ui/button", "gh/acme/web/packages/ui")
	if !ok || subpath != "button" {
		t.Fatalf("github monorepo path import match = %q, %v", subpath, ok)
	}
	subpath, ok = importMatchesRepo("@acme/my_lib/button", "gh/acme/web/packages/my-lib")
	if !ok || subpath != "button" {
		t.Fatalf("github monorepo package alias import match = %q, %v", subpath, ok)
	}
}

func TestWorkspaceImportMatchesPackageRepoKeys(t *testing.T) {
	subpath, ok := importMatchesRepo("@acme/lib/pkg", "npm/@acme/lib")
	if !ok || subpath != "pkg" {
		t.Fatalf("npm scoped import match = %q, %v", subpath, ok)
	}
	subpath, ok = importMatchesRepo("requests/auth", "pypi/requests")
	if !ok || subpath != "auth" {
		t.Fatalf("pypi import match = %q, %v", subpath, ok)
	}
	subpath, ok = importMatchesRepo("golang.org/x/sync/errgroup", "gomod/golang.org/x/sync")
	if !ok || subpath != "errgroup" {
		t.Fatalf("go module import match = %q, %v", subpath, ok)
	}
	subpath, ok = importMatchesRepo("tokio::sync", "cargo/tokio")
	if !ok || subpath != "sync" {
		t.Fatalf("cargo import match = %q, %v", subpath, ok)
	}
	subpath, ok = importMatchesRepo("com.acme.lib.Service", "maven/com.acme/lib")
	if !ok || subpath != "Service" {
		t.Fatalf("maven import match = %q, %v", subpath, ok)
	}
	subpath, ok = importMatchesRepo("Newtonsoft.Json.Linq", "nuget/Newtonsoft.Json")
	if !ok || subpath != "Linq" {
		t.Fatalf("nuget import match = %q, %v", subpath, ok)
	}
	subpath, ok = importMatchesRepo("active_support/core_ext", "gem/active_support")
	if !ok || subpath != "core_ext" {
		t.Fatalf("gem import match = %q, %v", subpath, ok)
	}
	subpath, ok = importMatchesRepo("monolog/monolog/src/Logger", "composer/monolog/monolog")
	if !ok || subpath != "src/Logger" {
		t.Fatalf("composer import match = %q, %v", subpath, ok)
	}
	subpath, ok = importMatchesRepo("acme_client.transport", "pypi/acme-client")
	if !ok || subpath != "transport" {
		t.Fatalf("pypi hyphen/underscore import match = %q, %v", subpath, ok)
	}
	subpath, ok = importMatchesRepo("tokio_util::codec", "cargo/tokio-util")
	if !ok || subpath != "codec" {
		t.Fatalf("cargo hyphen/underscore import match = %q, %v", subpath, ok)
	}
	if _, ok := importMatchesRepo("@acme/other/pkg", "npm/@acme/lib"); ok {
		t.Fatalf("unrelated package import should not match")
	}
}
