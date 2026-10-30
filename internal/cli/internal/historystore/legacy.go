package historystore

import (
	"time"

	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
)

// LegacyIdentity is a complete strong identity derived from one fully
// read and parsed legacy index. Observed* binds a later best-effort manifest
// update to the exact source generation the caller used.
type LegacyIdentity struct {
	ObservedGeneratedAt time.Time
	ObservedIndexPath   string
	ObservedRecords     int
	IndexBytes          int64
	IndexSHA256         string
	RecordsFingerprint  string
}

func sourceNeedsIdentityUpgrade(source *bmanifest.HistorySource) bool {
	return source != nil && (source.IndexBytes == 0 || source.IndexSHA256 == "" || source.RecordsFingerprint == "")
}

// DeriveLegacyIdentity accepts only a complete, internally consistent
// legacy generation. Existing non-empty fields remain authoritative: a partial
// migration with a conflicting field is not repaired opportunistically.
func DeriveLegacyIdentity(source *bmanifest.HistorySource, data []byte, index Index) *LegacyIdentity {
	if !sourceNeedsIdentityUpgrade(source) || source.IndexPath == "" || source.GeneratedAt.IsZero() ||
		source.Records < 0 || len(index.Records) != source.Records || !index.GeneratedAt.Equal(source.GeneratedAt) {
		return nil
	}
	identity := &LegacyIdentity{
		ObservedGeneratedAt: source.GeneratedAt,
		ObservedIndexPath:   source.IndexPath,
		ObservedRecords:     source.Records,
		IndexBytes:          int64(len(data)),
		IndexSHA256:         IndexBytesFingerprint(data),
		RecordsFingerprint:  RecordsFingerprint(index.Records),
	}
	if source.IndexBytes != 0 && source.IndexBytes != identity.IndexBytes {
		return nil
	}
	if source.IndexSHA256 != "" && source.IndexSHA256 != identity.IndexSHA256 {
		return nil
	}
	if source.RecordsFingerprint != "" && source.RecordsFingerprint != identity.RecordsFingerprint {
		return nil
	}
	return identity
}

func (identity *LegacyIdentity) SourceWithIdentity(source *bmanifest.HistorySource) *bmanifest.HistorySource {
	if identity == nil || source == nil {
		return source
	}
	upgraded := *source
	upgraded.IndexBytes = identity.IndexBytes
	upgraded.IndexSHA256 = identity.IndexSHA256
	upgraded.RecordsFingerprint = identity.RecordsFingerprint
	return &upgraded
}
