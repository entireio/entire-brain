package cli

import "testing"

func TestWorkspaceResolutionPayloadSelectsBestCrossRepoSymbols(t *testing.T) {
	const (
		consumer = "local/app"
		library  = "npm/@acme/lib"
	)
	source := workspaceGraphSymbolRef{
		RepoKey: consumer, ID: "app:run", Kind: "function", Name: "run",
		QualifiedName: "app.run", FilePath: "src/app.ts",
	}
	indexes := []workspaceRepoGraphIndex{
		{
			RepoKey: consumer,
			Imports: []workspaceGraphImportRef{{
				Spec: "@acme/lib/api/Client", Source: source, Count: 3,
			}},
			ExternalSymbols: []workspaceGraphExternalSymbolRef{{
				Spec: "api.Client", Type: "CALLS", Source: source, Count: 2,
			}},
		},
		{
			RepoKey: library,
			Candidates: []workspaceGraphSymbolRef{
				// These malformed and file records look exact but cannot be targets.
				{RepoKey: library, ID: "", Kind: "class", Name: "Client", QualifiedName: "api.Client", FilePath: "src/api/empty.ts"},
				{RepoKey: library, ID: "lib:file", Kind: "file", Name: "Client", QualifiedName: "api.Client", FilePath: "src/api/client.ts"},
				// Both selectors should prefer the exact qualified name over name-only
				// and qualified-name suffix matches.
				{RepoKey: library, ID: "lib:exact", Kind: "class", Name: "APIClient", QualifiedName: "api.Client", FilePath: "src/api/client.ts"},
				{RepoKey: library, ID: "lib:name", Kind: "class", Name: "Client", QualifiedName: "other.Client", FilePath: "src/other/client.ts"},
				{RepoKey: library, ID: "lib:suffix", Kind: "class", Name: "ClientImpl", QualifiedName: "internal.api.Client", FilePath: "src/internal/client.ts"},
			},
		},
		{
			RepoKey: "npm/@acme/unrelated",
			Candidates: []workspaceGraphSymbolRef{{
				RepoKey: "npm/@acme/unrelated", ID: "wrong:client", Kind: "class",
				Name: "Other", QualifiedName: "other.Other", FilePath: "other/other.ts",
			}},
		},
	}

	crossEdges := workspaceGraphImportCrossEdges(indexes, 20)
	crossEdges = append(crossEdges, workspaceGraphExternalSymbolCrossEdges(indexes, 20)...)
	if len(crossEdges) != 2 {
		t.Fatalf("cross edges = %#v, want one import and one external-symbol edge", crossEdges)
	}

	seen := map[string]workspaceGraphCrossEdge{}
	for _, edge := range crossEdges {
		seen[edge.RelationKind] = edge
		if edge.ToRepo != library {
			t.Fatalf("edge escaped matching repository: %#v", edge)
		}
	}
	imp := seen["cross_repo_import_candidate"]
	if imp.ToSymbol.ID != "lib:exact" || imp.ToSymbol.Direction != "import_symbol_target" || imp.SharedCount != 3 {
		t.Fatalf("import edge = %#v", imp)
	}
	ext := seen["cross_repo_external_symbol"]
	if ext.ToSymbol.ID != "lib:exact" || ext.ToSymbol.Direction != "external_symbol_target" || ext.SharedCount != 2 {
		t.Fatalf("external-symbol edge = %#v", ext)
	}
}

func TestWorkspaceResolutionPayloadUsesDeterministicTiesAndRejectsNonMatches(t *testing.T) {
	source := workspaceGraphSymbolRef{RepoKey: "local/app", ID: "app:run", Kind: "function", Name: "run"}
	indexes := []workspaceRepoGraphIndex{
		{
			RepoKey: "local/app",
			Imports: []workspaceGraphImportRef{
				{Spec: "@acme/lib/api/Client", Source: source, Count: 1},
				{Spec: "@acme/missing/Client", Source: source, Count: 1},
			},
			ExternalSymbols: []workspaceGraphExternalSymbolRef{
				{Spec: "Client", Type: "CALLS", Source: source, Count: 1},
				{Spec: "DoesNotExist", Type: "CALLS", Source: source, Count: 1},
			},
		},
		{
			RepoKey: "npm/@acme/lib",
			Candidates: []workspaceGraphSymbolRef{
				{RepoKey: "npm/@acme/lib", ID: "lib:z", Kind: "class", Name: "Client", QualifiedName: "Client", FilePath: "z/client.ts"},
				{RepoKey: "npm/@acme/lib", ID: "lib:b", Kind: "class", Name: "Client", QualifiedName: "Client", FilePath: "a/client.ts"},
				{RepoKey: "npm/@acme/lib", ID: "lib:a", Kind: "class", Name: "Client", QualifiedName: "Client", FilePath: "a/client.ts"},
				{RepoKey: "npm/@acme/lib", ID: "lib:file", Kind: "file", Name: "Client", QualifiedName: "Client", FilePath: "README.md"},
			},
		},
	}

	imports := workspaceGraphImportCrossEdges(indexes, 10)
	externals := workspaceGraphExternalSymbolCrossEdges(indexes, 10)
	if len(imports) != 1 || imports[0].ToSymbol.ID != "lib:a" {
		t.Fatalf("deterministic import resolution = %#v", imports)
	}
	if len(externals) != 1 || externals[0].ToSymbol.ID != "lib:a" {
		t.Fatalf("deterministic external resolution = %#v", externals)
	}
	if imports[0].FromRepo == imports[0].ToRepo || externals[0].FromRepo == externals[0].ToRepo {
		t.Fatalf("cross-repo resolver emitted a same-repo edge: imports=%#v externals=%#v", imports, externals)
	}
}

func TestWorkspaceResolutionPayloadFallsBackToCodeTargets(t *testing.T) {
	candidates := []workspaceGraphSymbolRef{
		{RepoKey: "npm/@acme/lib", ID: "lib:docs", Kind: "section", Name: "Overview", FilePath: "README.md"},
		{RepoKey: "npm/@acme/lib", ID: "lib:file", Kind: "file", Name: "client", FilePath: "src/client.ts"},
		{RepoKey: "npm/@acme/lib", ID: "lib:code", Kind: "function", Name: "createClient", FilePath: "src/client.ts"},
		{RepoKey: "npm/@acme/lib", ID: "lib:package", Kind: "package", Name: "lib", FilePath: "package.json"},
	}
	for _, tc := range []struct {
		name      string
		spec      string
		direction string
	}{
		{name: "root import", spec: "@acme/lib", direction: "import_path_target"},
		{name: "unknown subpath", spec: "@acme/lib/not/present"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			edges := workspaceGraphImportCrossEdges([]workspaceRepoGraphIndex{
				{
					RepoKey: "local/app",
					Imports: []workspaceGraphImportRef{{
						Spec:   tc.spec,
						Source: workspaceGraphSymbolRef{RepoKey: "local/app", ID: "app:run", Kind: "function"},
						Count:  1,
					}},
				},
				{RepoKey: "npm/@acme/lib", Candidates: candidates},
			}, 10)
			if len(edges) != 1 || edges[0].ToSymbol.ID != "lib:package" || edges[0].ToSymbol.Direction != tc.direction {
				t.Fatalf("fallback edge = %#v", edges)
			}
		})
	}

	if edges := workspaceGraphImportCrossEdges([]workspaceRepoGraphIndex{
		{RepoKey: "local/app", Imports: []workspaceGraphImportRef{{Spec: "@acme/empty", Count: 1}}},
		{RepoKey: "npm/@acme/empty"},
	}, 10); len(edges) != 0 {
		t.Fatalf("empty target repository produced edges: %#v", edges)
	}
	if _, ok := workspaceExternalSymbolTarget(candidates, "   "); ok {
		t.Fatal("blank external symbol unexpectedly resolved")
	}
}
