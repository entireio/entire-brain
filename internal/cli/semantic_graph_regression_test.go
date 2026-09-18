package cli

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"
)

func semanticRegressionGraphDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open(sqliteDriverName, filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, statement := range []string{
		`CREATE TABLE symbols (id TEXT PRIMARY KEY, name TEXT, qualified_name TEXT, kind TEXT, file_path TEXT, language TEXT)`,
		`CREATE TABLE files (path TEXT PRIMARY KEY, language TEXT)`,
		`CREATE TABLE relations (from_id TEXT, to_id TEXT, type TEXT, confidence REAL, reason TEXT, warning_codes TEXT)`,
		`CREATE TABLE runtime_traces (id INTEGER PRIMARY KEY AUTOINCREMENT, imported_at TEXT NOT NULL, source_path TEXT NOT NULL, from_id TEXT NOT NULL, to_id TEXT NOT NULL, observed_type TEXT, matched_static_edge INTEGER NOT NULL)`,
		`INSERT INTO symbols VALUES ('caller-id','Caller','pkg.Caller','function','internal/caller.go','Go')`,
		`INSERT INTO symbols VALUES ('target-id','Target','pkg.Target','method','internal/target.go','Go')`,
		`INSERT INTO relations VALUES ('caller-id','target-id','CALLS',1,'static call','[]')`,
		`INSERT INTO runtime_traces(imported_at,source_path,from_id,to_id,observed_type,matched_static_edge) VALUES ('2026-09-18T00:00:00Z','traces/run.ndjson','target-id','caller-id','OBSERVED_CALL',0)`,
		`INSERT INTO runtime_traces(imported_at,source_path,from_id,to_id,observed_type,matched_static_edge) VALUES ('2026-09-18T00:01:00Z','traces/run.ndjson','target-id','caller-id','OBSERVED_CALL',0)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("graph fixture %q: %v", statement, err)
		}
	}
	return db
}

func TestSemanticGraphCypherPredicatesDirectionsAndLimits(t *testing.T) {
	forward := `MATCH (a)-[r]->(b) WHERE a.name = "Caller" AND a.kind = "function" AND a.file_path CONTAINS "internal" AND b.qualified_name = "pkg.Target" AND b.kind = "method" AND b.language CONTAINS "Go" AND r.type = "calls" AND r.reason CONTAINS "first" AND r.reason CONTAINS "second" RETURN a,r,b LIMIT 3`
	filters, ok := parseCypherGraphQuery(forward)
	if !ok {
		t.Fatal("forward Cypher query was not parsed")
	}
	if filters.FromName != "Caller" || filters.FromKind != "function" || filters.FromNameContains != "internal" || filters.ToName != "pkg.Target" || filters.ToKind != "method" || filters.ToNameContains != "Go" || filters.Type != "CALLS" || filters.Text != "first second" || filters.Limit != 3 {
		t.Fatalf("forward filters = %+v", filters)
	}

	reverse, ok := parseCypherGraphQuery(`MATCH (a)<-[r:USES_TYPE]-(b) WHERE a.qualified_name = "pkg.Target" AND b.name CONTAINS "Caller" RETURN count(r) LIMIT 1`)
	if !ok || !reverse.Count || reverse.Type != "USES_TYPE" || reverse.ToName != "pkg.Target" || reverse.FromNameContains != "Caller" || reverse.Limit != 1 {
		t.Fatalf("reverse filters = %+v ok=%t", reverse, ok)
	}
	for _, invalid := range []string{"plain text", `MATCH (a)-[r]-(b) RETURN r`, `MATCH (a)->[r]-(b) RETURN r`} {
		if got, ok := parseCypherGraphQuery(invalid); ok {
			t.Fatalf("invalid query parsed: %q => %+v", invalid, got)
		}
	}
}

func TestSemanticGraphCypherPredicateMatrix(t *testing.T) {
	tests := []struct {
		alias, field, op, value string
		want                    graphQueryFilters
	}{
		{"a", "qualified_name", "=", "from.qual", graphQueryFilters{FromName: "from.qual"}},
		{"a", "name", "=", "from", graphQueryFilters{FromName: "from"}},
		{"a", "kind", "=", "function", graphQueryFilters{FromKind: "function"}},
		{"a", "name", "CONTAINS", "from", graphQueryFilters{FromNameContains: "from"}},
		{"a", "language", "CONTAINS", "Rust", graphQueryFilters{FromNameContains: "Rust"}},
		{"b", "name", "=", "target", graphQueryFilters{ToName: "target"}},
		{"b", "qualified_name", "=", "pkg.target", graphQueryFilters{ToName: "pkg.target"}},
		{"b", "kind", "=", "method", graphQueryFilters{ToKind: "method"}},
		{"b", "language", "CONTAINS", "Go", graphQueryFilters{ToNameContains: "Go"}},
		{"b", "file_path", "CONTAINS", "service", graphQueryFilters{ToNameContains: "service"}},
		{"r", "type", "=", "calls", graphQueryFilters{Type: "CALLS"}},
		{"r", "reason", "CONTAINS", "first", graphQueryFilters{Text: "first"}},
		{"x", "unknown", "=", "ignored", graphQueryFilters{}},
	}
	for _, tc := range tests {
		var got graphQueryFilters
		applyCypherPredicate(&got, tc.alias, tc.field, tc.op, tc.value)
		if !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%s.%s %s: got %+v want %+v", tc.alias, tc.field, tc.op, got, tc.want)
		}
	}
}

func TestSemanticRuntimeTraceFiltersMatchAndRejectEachEndpoint(t *testing.T) {
	db := semanticRegressionGraphDB(t)
	filters := graphQueryFilters{
		Type: "RUNTIME_TRACE", From: "target-id", To: "caller-id", Text: "run.ndjson", FromName: "Target", ToName: "Caller",
		FromKind: "method", ToKind: "function", FromNameContains: "target.go", ToNameContains: "caller.go",
	}
	if records, err := queryGraphRelations(db, filters, 10); err != nil || len(records) != 2 || records[0].RecordType != "runtime_trace" {
		t.Fatalf("fully filtered runtime trace records=%+v err=%v", records, err)
	}
	if records, err := queryGraphRelations(db, filters, 1); err != nil || len(records) != 1 {
		t.Fatalf("runtime trace limit did not truncate matching records=%+v err=%v", records, err)
	}
	for name, mismatch := range map[string]func(*graphQueryFilters){
		"from":          func(f *graphQueryFilters) { f.From = "other" },
		"to":            func(f *graphQueryFilters) { f.To = "other" },
		"text":          func(f *graphQueryFilters) { f.Text = "other" },
		"from name":     func(f *graphQueryFilters) { f.FromName = "Other" },
		"to name":       func(f *graphQueryFilters) { f.ToName = "Other" },
		"from kind":     func(f *graphQueryFilters) { f.FromKind = "other" },
		"to kind":       func(f *graphQueryFilters) { f.ToKind = "other" },
		"from contains": func(f *graphQueryFilters) { f.FromNameContains = "other" },
		"to contains":   func(f *graphQueryFilters) { f.ToNameContains = "other" },
	} {
		t.Run(name, func(t *testing.T) {
			got := filters
			mismatch(&got)
			records, err := queryGraphRelations(db, got, 10)
			if err != nil || len(records) != 0 {
				t.Fatalf("mismatched runtime trace filter returned records=%+v err=%v", records, err)
			}
		})
	}
}

func TestSemanticGraphQueriesDirectionsLimitsEmptyAndRuntimeTrace(t *testing.T) {
	db := semanticRegressionGraphDB(t)
	forward, ok := parseCypherGraphQuery(`MATCH (a)-[r:CALLS]->(b) WHERE a.name = "Caller" AND b.name = "Target" RETURN a,r,b LIMIT 1`)
	if !ok {
		t.Fatal("forward query did not parse")
	}
	records, err := queryGraphRelations(db, forward, 10)
	if err != nil || len(records) != 1 || records[0].FromID != "caller-id" || records[0].ToID != "target-id" {
		t.Fatalf("forward records=%+v err=%v", records, err)
	}
	reverse, ok := parseCypherGraphQuery(`MATCH (a)<-[r:CALLS]-(b) WHERE a.name = "Target" AND b.name = "Caller" RETURN a,r,b`)
	if !ok {
		t.Fatal("reverse query did not parse")
	}
	records, err = queryGraphRelations(db, reverse, 1)
	if err != nil || len(records) != 1 || records[0].FromID != "caller-id" || records[0].ToID != "target-id" {
		t.Fatalf("reverse records=%+v err=%v", records, err)
	}
	if records, err = queryGraphRelations(db, graphQueryFilters{FromName: "Missing"}, 10); err != nil || records == nil || len(records) != 0 {
		t.Fatalf("empty query must return []: records=%#v err=%v", records, err)
	}
	traceFilters := graphQueryFilters{Type: "RUNTIME_TRACE", FromName: "Target", ToKind: "function", Text: "run.ndjson"}
	if records, err = queryGraphRelations(db, traceFilters, 1); err != nil || len(records) != 1 || records[0].RecordType != "runtime_trace" || records[0].Confidence != 0.5 || !reflect.DeepEqual(records[0].WarningCodes, []string{"UNMATCHED_STATIC_EDGE"}) {
		t.Fatalf("runtime trace records=%+v err=%v", records, err)
	}
	if count, err := countGraphRelations(db, graphQueryFilters{Type: "RUNTIME_TRACE"}); err != nil || count != 2 {
		t.Fatalf("runtime trace count=%d err=%v", count, err)
	}
}
