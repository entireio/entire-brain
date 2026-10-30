package semstore

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
	"github.com/entireio/entire-brain/internal/cli/internal/sqlitedriver"
)

const (
	DirName    = "semantic"
	SQLiteName = "semantic.sqlite"
)

const graphStoreHandlePrefix = "graph-store-v1:"

// ErrStoreRetired rejects a persisted pre-0.2.0 source: Brain no longer
// reads its own snapshot/generation layout, only Graph's live store handle.
var ErrStoreRetired = errors.New("code context store is Graph-owned")

func ValidateDeclaredStore(brainDir string, source *bmanifest.SemanticSource) (string, error) {
	if source.GraphStore != nil {
		return source.StorePath, nil
	}
	return "", fmt.Errorf("%w; run `%s`", ErrStoreRetired, base.BrainInvocation(os.LookupEnv, base.SemanticIndexVerb))
}

func GraphStoreSource(d bmanifest.GraphStoreDescriptor, brainDir string) *bmanifest.SemanticSource {
	d.BrainDir = brainDir
	encoded, _ := json.Marshal(struct {
		Path        string `json:"path"`
		EntryDigest string `json:"entry_digest"`
		BrainDir    string `json:"brain_dir"`
	}{d.Path, d.EntryDigest, brainDir})
	h := d.Header
	// fetchGraphStore already refused a schema this build cannot read at all
	// (ADR 0001's major boundary); a newer supported-major minor still reaches
	// here and must be recorded as a warning before SchemaVersion is copied, so
	// the persisted source does not silently claim full ingestion.
	warnings := append(append([]bmanifest.SemanticWarning(nil), h.Warnings...), semanticSchemaWarnings(h.SchemaVersion)...)
	return &bmanifest.SemanticSource{Provider: "entire-graph", ProviderVersion: h.ProviderVersion, IdentityRevision: h.IdentityRevision, SchemaVersion: h.SchemaVersion, Commit: d.Commit, Tree: d.Tree, Profile: h.Profile, Symbols: d.Symbols, Relations: d.Relations, Externals: d.Externals, Files: d.Files, NoEgressVerified: true, StoreOwner: "graph", StorePath: graphStoreHandlePrefix + base64.RawURLEncoding.EncodeToString(encoded), GraphStore: &d, Languages: h.Languages, LanguageTiers: h.LanguageTiers, Capabilities: h.Capabilities, Warnings: warnings, PartialFailures: h.PartialFailures, Completeness: h.Completeness, Stats: h.Stats, ProfileLimits: h.ProfileLimits, RelationSet: h.RelationSet, CompletenessLevel: CompletenessLevelFromStats(h.Stats), Trust: TrustForCompleteness(CompletenessLevelFromStats(h.Stats))}
}

// semanticSchemaWarnings returns the minor-skew warning for a live Graph store
// header, or nil. ValidateSchema's hard major refusal already ran in
// fetchGraphStore before any caller reaches this point, so only the
// non-fatal, supported-major case remains to check here.
func semanticSchemaWarnings(schemaVersion string) []bmanifest.SemanticWarning {
	if skew := SchemaMinorSkewWarning(schemaVersion); skew != nil {
		return []bmanifest.SemanticWarning{*skew}
	}
	return nil
}

// OpenReadDB opens the provider's versioned projection. TEMP VIEWs
// are connection-local aliases, not a second store. A single connection keeps
// those aliases visible throughout the operation. mode=ro protects Graph even
// if a future caller accidentally attempts a write.
func OpenReadDB(handle string) (*sql.DB, error) {
	if !strings.HasPrefix(handle, graphStoreHandlePrefix) {
		return sql.Open(sqlitedriver.Name, brainstore.SQLiteReadOnlyDSN(handle))
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(handle, graphStoreHandlePrefix))
	if err != nil {
		return nil, err
	}
	var d bmanifest.GraphStoreDescriptor
	if err = json.Unmarshal(raw, &d); err != nil {
		return nil, err
	}
	db, err := sql.Open(sqlitedriver.Name, brainstore.SQLiteReadOnlyDSN(d.Path))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	fail := func(err error) (*sql.DB, error) { db.Close(); return nil, err }
	var digest string
	if err = db.QueryRow("SELECT value FROM meta WHERE key='entry_digest'").Scan(&digest); err != nil {
		return fail(err)
	}
	var actual string
	if err = json.Unmarshal([]byte(digest), &actual); err != nil || actual != d.EntryDigest {
		return fail(errors.New("Graph store changed after resolution; retry the operation"))
	}
	for _, table := range []string{"symbols", "relations", "files", "reverse_relations", "symbol_fts"} {
		brainView := "brain_" + table + "_v1"
		if _, err = db.Exec("CREATE TEMP VIEW " + table + " AS SELECT * FROM main." + brainView); err != nil {
			return fail(graphStoreViewMismatchError(brainView, err))
		}
		// SQLite does not resolve a view's SELECT until it is actually used, so
		// CREATE TEMP VIEW above succeeds even when brainView no longer exists.
		// table_info forces that resolution now, zero rows, so a provider that
		// renamed or dropped a view fails right here with a named error instead
		// of a raw "no such table" from whatever caller happens to query it
		// first.
		if _, err = db.Exec("PRAGMA table_info(" + table + ")"); err != nil {
			return fail(graphStoreViewMismatchError(brainView, err))
		}
	}
	if err = loadGraphTraceTable(db, d.BrainDir); err != nil {
		return fail(err)
	}
	return db, nil
}

// graphStoreViewMismatchError names both binaries: either direction can rename
// or drop a brain_*_v1 view, so a raw sqlite "no such table" does not say
// which one is wrong. The cause is only wrapped, not inspected -- a schema
// mismatch is the likely reason but not the only one a view can fail to
// resolve (a corrupted or concurrently-locked store can too), so the message
// hedges between both remedies instead of asserting a diagnosis it can't verify.
func graphStoreViewMismatchError(brainView string, cause error) error {
	return fmt.Errorf("Graph store view %s could not be resolved: %w (upgrade entire-brain or Graph to matching versions if this is a schema mismatch, or refresh the index if the store is corrupted)", brainView, cause)
}
