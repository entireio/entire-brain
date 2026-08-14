package cli

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestMemoryReadOnlyHealthClassifiesObservedOverlayJobAndFTSSchemas(t *testing.T) {
	now := time.Date(2026, 8, 10, 8, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		section   string
		observed  string
		wantState string
		wantCode  string
		prepare   func(*testing.T, string)
	}{
		{
			name: "corrupt overlay", section: "overlay", observed: "overlay", wantState: "corrupt", wantCode: memoryErrStateCorrupt,
			prepare: func(t *testing.T, brainDir string) {
				writeHealthFixtureFile(t, brainDir, historyShortTermPath, []byte("{broken\n"))
			},
		},
		{
			name: "unknown newer overlay", section: "overlay", observed: "overlay", wantState: "unsupported", wantCode: memoryErrUnsupportedVersion,
			prepare: func(t *testing.T, brainDir string) {
				writeHealthFixtureFile(t, brainDir, historyShortTermPath, []byte(fmt.Sprintf(`{"version":%d,"reconciler_version":%d,"future":true,"files":{}}`, historyShortTermVersion+1, historyShortTermReconcilerVersion)+"\n"))
			},
		},
		{
			name: "corrupt job", section: "job_inventory", observed: "job", wantState: "corrupt", wantCode: memoryErrStateCorrupt,
			prepare: func(t *testing.T, brainDir string) {
				writeHealthFixtureFile(t, brainDir, filepath.ToSlash(filepath.Join(memoryJobsDirRel, "broken.json")), []byte("{broken\n"))
			},
		},
		{
			name: "unknown newer job", section: "job_inventory", observed: "job", wantState: "unsupported", wantCode: memoryErrUnsupportedVersion,
			prepare: func(t *testing.T, brainDir string) {
				writeHealthFixtureFile(t, brainDir, filepath.ToSlash(filepath.Join(memoryJobsDirRel, "future.json")), []byte(fmt.Sprintf(`{"schema_version":%d,"job_id":"future","future":true}`, memoryJobSchemaVersion+1)+"\n"))
			},
		},
		{
			name: "corrupt FTS", section: "fts", observed: "fts", wantState: "corrupt", wantCode: memoryErrStateCorrupt,
			prepare: func(t *testing.T, brainDir string) {
				writeHealthFixtureFile(t, brainDir, historyFTSDBRelPath(), []byte("not sqlite"))
			},
		},
		{
			name: "unknown newer FTS", section: "fts", observed: "fts", wantState: "unsupported", wantCode: memoryErrUnsupportedVersion,
			prepare: func(t *testing.T, brainDir string) {
				writeHealthFTSMetaFixture(t, brainDir, fmt.Sprint(mustAtoi(t, historyFTSSchema)+1), "future")
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			brainDir := t.TempDir()
			writeHealthFixtureFile(t, brainDir, exportManifestFileName, []byte(fmt.Sprintf(`{"schema_version":%d}`, brainManifestSchemaVersion)+"\n"))
			tc.prepare(t, brainDir)
			before := healthFixtureTree(t, brainDir)

			snapshot := memoryReadOnlyHealth(brainDir, now)
			health, ok := snapshot.Payload[tc.section].(map[string]any)
			if !ok || normalizeMemoryObservedState(memoryObservedStateValue(health["state"])) != tc.wantState || health["error_code"] != tc.wantCode {
				t.Fatalf("%s health = %#v", tc.section, health)
			}
			schemas, _ := snapshot.Payload["schemas"].(map[string]any)
			observed, _ := schemas["observed_health"].(map[string]any)
			observation, _ := observed[tc.observed].(map[string]any)
			if normalizeMemoryObservedState(memoryObservedStateValue(observation["state"])) != tc.wantState || observation["error_code"] != tc.wantCode {
				t.Fatalf("observed %s schema = %#v", tc.observed, observation)
			}
			compiled, _ := schemas["compiled_capabilities"].(map[string]any)
			if compiled[tc.observed] == nil {
				t.Fatalf("compiled capability %q missing: %#v", tc.observed, compiled)
			}
			checks := doctorChecksByName(memoryDoctorChecks(snapshot))
			if got := checks["memory_schemas"]; got.State != "error" || !strings.Contains(got.Detail, tc.wantCode) {
				t.Fatalf("memory_schemas check = %+v", got)
			}
			if after := healthFixtureTree(t, brainDir); !reflect.DeepEqual(after, before) {
				t.Fatalf("read-only health changed artifact tree\nbefore=%#v\nafter=%#v", before, after)
			}
		})
	}
}

func TestMemoryReadOnlyHealthReportsAbsentVectorAndCanonicalSourceSchema(t *testing.T) {
	brainDir := t.TempDir()
	now := time.Date(2026, 8, 10, 8, 30, 0, 0, time.UTC)
	writeHealthFixtureFile(t, brainDir, exportManifestFileName, []byte(fmt.Sprintf(`{"schema_version":%d}`, brainManifestSchemaVersion)+"\n"))

	snapshot := memoryReadOnlyHealth(brainDir, now)
	vector, _ := snapshot.Payload["vector_progress"].(map[string]any)
	if vector["state"] != "absent" || vector["schema_state"] != "absent" || vector["present"] != false || vector["supported_schema_version"] != memoryVectorSchema {
		t.Fatalf("absent vector health = %#v", vector)
	}
	if _, claimed := vector["schema_version"]; claimed {
		t.Fatalf("absent vector falsely claims an observed schema: %#v", vector)
	}
	source, _ := snapshot.Payload["source_manifest"].(map[string]any)
	if source["state"] != "current" || source["schema_version"] != brainManifestSchemaVersion || source["supported_schema_version"] != brainManifestSchemaVersion || source["authority"] != "canonical_brain_manifest" {
		t.Fatalf("source manifest health = %#v", source)
	}
	canonical, _ := snapshot.Payload["canonical_sessions"].(map[string]any)
	if canonical["source_manifest_schema_version"] != brainManifestSchemaVersion || canonical["supported_source_manifest_schema_version"] != brainManifestSchemaVersion {
		t.Fatalf("canonical source schema = %#v", canonical)
	}
	schemas, _ := snapshot.Payload["schemas"].(map[string]any)
	observed, _ := schemas["observed_health"].(map[string]any)
	vectorObserved, _ := observed["vector_progress"].(map[string]any)
	if vectorObserved["state"] != "absent" || vectorObserved["supported_schema_version"] != memoryVectorSchema {
		t.Fatalf("absent vector schema observation = %#v", vectorObserved)
	}
}

func TestMemoryReadOnlyHealthDoesNotResolveConfiguredProvider(t *testing.T) {
	brainDir := t.TempDir()
	if err := saveMemoryConfig(brainDir, memoryConfig{SchemaVersion: memoryConfigSchemaVersion, Abstracts: memoryAbstractsConfig{
		Enabled: true, Automatic: true, Provider: "codex", Model: "configured", HostedEgressAllowed: true,
	}}); err != nil {
		t.Fatal(err)
	}
	oldFactory := memoryAbstractorFactory
	calls := 0
	memoryAbstractorFactory = func(memoryAbstractsConfig) (ConversationAbstractor, error) {
		calls++
		return nil, fmt.Errorf("status must not resolve providers")
	}
	t.Cleanup(func() { memoryAbstractorFactory = oldFactory })

	snapshot := memoryReadOnlyHealth(brainDir, time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC))
	if calls != 0 {
		t.Fatalf("read-only health resolved provider %d times", calls)
	}
	install, _ := snapshot.Payload["install"].(map[string]any)
	provider, _ := install["provider_egress"].(map[string]any)
	if provider["provider_state"] != "configured_unverified" || provider["effective_state"] != "available_unverified" {
		t.Fatalf("provider health = %#v", provider)
	}
}

func TestMemoryReadOnlyHealthReportsAbstractAndEgressObservedVersions(t *testing.T) {
	now := time.Date(2026, 8, 10, 9, 30, 0, 0, time.UTC)
	t.Run("unknown newer abstract", func(t *testing.T) {
		brainDir := t.TempDir()
		digest := strings.Repeat("a", 64)
		writeHealthFixtureFile(t, brainDir, filepath.ToSlash(filepath.Join(abstractsDirRel, digest+".json")), []byte(fmt.Sprintf(`{"schema_version":%d,"future":true}`, abstractSchemaVersion+1)+"\n"))
		snapshot := memoryReadOnlyHealth(brainDir, now)
		install, _ := snapshot.Payload["install"].(map[string]any)
		abstracts, _ := install["abstracts"].(map[string]any)
		if abstracts["state"] != "unsupported" || abstracts["schema_version"] != abstractSchemaVersion+1 || !reflect.DeepEqual(abstracts["schema_versions_observed"], []int{abstractSchemaVersion + 1}) {
			t.Fatalf("abstract health = %#v", abstracts)
		}
		schemas, _ := snapshot.Payload["schemas"].(map[string]any)
		observed, _ := schemas["observed_health"].(map[string]any)
		artifact, _ := observed["abstract_artifact"].(map[string]any)
		if normalizeMemoryObservedState(memoryObservedStateValue(artifact["state"])) != "unsupported" || artifact["schema_version"] != abstractSchemaVersion+1 {
			t.Fatalf("abstract schema observation = %#v", artifact)
		}
	})

	t.Run("unknown newer egress", func(t *testing.T) {
		brainDir := t.TempDir()
		ref := conversationSessionIDPrefix + strings.Repeat("b", 64)
		writeHealthFixtureFile(t, brainDir, abstractEgressReceiptRel(ref), []byte(fmt.Sprintf(`{"schema_version":%d,"future":true}`, abstractEgressSchemaVersion+1)+"\n"))
		snapshot := memoryReadOnlyHealth(brainDir, now)
		install, _ := snapshot.Payload["install"].(map[string]any)
		abstracts, _ := install["abstracts"].(map[string]any)
		egress, _ := abstracts["hosted_egress"].(map[string]any)
		if egress["state"] != "unsupported" || egress["schema_version"] != abstractEgressSchemaVersion+1 || !reflect.DeepEqual(egress["schema_versions_observed"], []int{abstractEgressSchemaVersion + 1}) {
			t.Fatalf("egress health = %#v", egress)
		}
	})

	t.Run("legacy egress migration", func(t *testing.T) {
		brainDir := t.TempDir()
		receipt := abstractEgressReceipt{
			SchemaVersion: abstractEgressLegacyVersion,
			OperationID:   "0123456789abcdefabcd",
			SessionRef:    conversationSessionIDPrefix + strings.Repeat("c", 64),
			SessionDigest: "sha256:" + strings.Repeat("d", 64),
			Provider:      "codex",
			Model:         "legacy",
			StartedAt:     now.Add(-time.Minute),
			FinishedAt:    now,
			Status:        "completed",
		}
		data, err := json.Marshal(receipt)
		if err != nil {
			t.Fatal(err)
		}
		writeHealthFixtureFile(t, brainDir, filepath.ToSlash(filepath.Join(abstractEgressDirRel, receipt.OperationID+".json")), append(data, '\n'))
		snapshot := memoryReadOnlyHealth(brainDir, now)
		install, _ := snapshot.Payload["install"].(map[string]any)
		abstracts, _ := install["abstracts"].(map[string]any)
		egress, _ := abstracts["hosted_egress"].(map[string]any)
		if egress["state"] != memoryErrMigrationRequired || egress["schema_version"] != abstractEgressLegacyVersion || !reflect.DeepEqual(egress["schema_versions_observed"], []int{abstractEgressLegacyVersion}) {
			t.Fatalf("legacy egress health = %#v", egress)
		}
	})
}

func writeHealthFixtureFile(t *testing.T, brainDir, rel string, data []byte) {
	t.Helper()
	path := filepath.Join(brainDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeHealthFTSMetaFixture(t *testing.T, brainDir, schema, fingerprint string) {
	t.Helper()
	path := filepath.Join(brainDir, filepath.FromSlash(historyFTSDBRelPath()))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open(sqliteDriverName, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE history_fts_meta(key TEXT PRIMARY KEY, value TEXT)`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	for key, value := range map[string]string{"schema": schema, "fingerprint": fingerprint} {
		if _, err := db.Exec(`INSERT INTO history_fts_meta(key, value) VALUES (?, ?)`, key, value); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func healthFixtureTree(t *testing.T, root string) map[string]string {
	t.Helper()
	result := map[string]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if info.IsDir() {
			result[filepath.ToSlash(rel)+"/"] = info.Mode().String()
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		result[filepath.ToSlash(rel)] = fmt.Sprintf("%s:%x", info.Mode(), data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func doctorChecksByName(checks []doctorCheckResult) map[string]doctorCheckResult {
	result := make(map[string]doctorCheckResult, len(checks))
	for _, check := range checks {
		result[check.Name] = check
	}
	return result
}

func mustAtoi(t *testing.T, value string) int {
	t.Helper()
	var parsed int
	if _, err := fmt.Sscan(value, &parsed); err != nil {
		t.Fatal(err)
	}
	return parsed
}
