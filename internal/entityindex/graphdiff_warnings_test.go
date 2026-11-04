package entityindex

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestMigrationDiffWarnings(t *testing.T) {
	unsupported := func(path, severity string) string {
		return fmt.Sprintf(`{"code":"W_UNSUPPORTED_FILE","severity":%q,"file_path":%q}`, severity, path)
	}
	benign := unsupported(".brainignore", "info")
	cases := []struct {
		name     string
		warnings string
		wantOK   bool
	}{
		{"none", `[]`, true},
		{"legacy absent warnings", `null`, true},
		{"brain ignore", "[" + benign + "]", true},
		{"multiple metadata files", "[" + benign + "," + unsupported("go.mod", "info") + "]", true},
		{"source file", "[" + unsupported("main.go", "info") + "]", false},
		{"unknown language", "[" + unsupported("source.unknown", "info") + "]", false},
		{"unknown configuration", "[" + unsupported("settings.custom", "info") + "]", false},
		{"warning severity", "[" + unsupported(".brainignore", "warning") + "]", false},
		{"error severity", "[" + unsupported(".brainignore", "error") + "]", false},
		{"missing severity", `[{"code":"W_UNSUPPORTED_FILE","file_path":".brainignore"}]`, false},
		{"missing path", `[{"code":"W_UNSUPPORTED_FILE","severity":"info"}]`, false},
		{"empty path", "[" + unsupported("", "info") + "]", false},
		{"absolute path", "[" + unsupported("/tmp/.brainignore", "info") + "]", false},
		{"parent path", "[" + unsupported("../.brainignore", "info") + "]", false},
		{"unclean path", "[" + unsupported("src/../.brainignore", "info") + "]", false},
		{"trailing slash", "[" + unsupported(".brainignore/", "info") + "]", false},
		{"parse failure on metadata", `[{"code":"W_PARSE_ERROR","severity":"info","file_path":".brainignore"}]`, false},
		{"unknown warning", `[{"code":"W_FUTURE_WARNING","severity":"info","file_path":".brainignore"}]`, false},
		{"string warning", `["partial result"]`, false},
		{"null warning", `[null]`, false},
		{"empty object", `[{}]`, false},
		{"wrong field type", `[{"code":"W_UNSUPPORTED_FILE","severity":"info","file_path":42}]`, false},
		{"benign then parse failure", "[" + benign + `,{"code":"W_PARSE_ERROR"}]`, false},
		{"parse failure then benign", `[{"code":"W_PARSE_ERROR"},` + benign + "]", false},
	}
	for _, name := range []string{".gitignore", ".gitattributes", ".gitmodules", ".editorconfig", "go.mod", "go.sum"} {
		cases = append(cases, struct {
			name, warnings string
			wantOK         bool
		}{name, "[" + unsupported("nested/"+name, "info") + "]", true})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newFakeRunner()
			base, head := strings.Repeat("a", 40), strings.Repeat("b", 40)
			envelope := graphResult{Base: base, Head: head, IdentityRevision: "4", Files: []graphFileChange{{Path: "main.go", Changes: []graphEntityChange{{Type: "modified", Kind: "function", Name: "Keep"}}}}}
			if err := json.Unmarshal([]byte(tc.warnings), &envelope.Warnings); err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(envelope)
			if err != nil {
				t.Fatal(err)
			}
			r.set(graphDiffKey(base, head), string(raw))
			delta, err := diffCommit(context.Background(), r, testRepoDir, "", base, head, fixedNow()(), true, "4")
			if tc.wantOK {
				if err != nil {
					t.Fatal(err)
				}
				if len(delta.Entities) != 1 || delta.Entities[0].Name != "Keep" {
					t.Fatalf("lost code history: %+v", delta)
				}
			} else if err == nil || !strings.Contains(err.Error(), "refusing potentially incomplete history") {
				t.Fatalf("unsafe warning accepted: %v", err)
			}
		})
	}
}
