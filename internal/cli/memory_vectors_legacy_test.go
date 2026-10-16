package cli

import (
	"fmt"
	"strings"
	"testing"
)

// A legacy vector-progress leaf that fails the identity/bounds invariants must
// classify as corrupt, not as a healthy pending/current state (trail #278
// review finding: the validation was dropped with the deleted writer).
func TestDecodeMemoryVectorProgressRejectsInvalidIdentityOrBounds(t *testing.T) {
	sha := strings.Repeat("1", 64)
	valid := fmt.Sprintf(`{"schema_version":%d,"model_id":"model","source_digest":"sha256:%s","complete_source_digest":"sha256:%s","reset_complete":true,"pending":false,"updated_at":"2026-08-09T00:00:00Z"}`,
		memoryVectorSchema, sha, sha)
	if _, err := decodeMemoryVectorProgress([]byte(valid)); err != nil {
		t.Fatalf("valid leaf rejected: %v", err)
	}
	for name, leaf := range map[string]string{
		"empty model id":             strings.Replace(valid, `"model_id":"model"`, `"model_id":""`, 1),
		"zero updated_at":            strings.Replace(valid, `"updated_at":"2026-08-09T00:00:00Z"`, `"updated_at":"0001-01-01T00:00:00Z"`, 1),
		"negative count":             strings.Replace(valid, `"reset_complete":true`, `"reset_complete":true,"history_added":-1`, 1),
		"bad source digest":          strings.Replace(valid, "sha256:"+sha, "sha256:zz", 1),
		"settled leaf without reset": strings.Replace(valid, `"reset_complete":true`, `"reset_complete":false`, 1),
		"unknown error code":         strings.Replace(valid, `"reset_complete":true`, `"reset_complete":true,"error_code":"something_else"`, 1),
	} {
		if _, err := decodeMemoryVectorProgress([]byte(leaf)); err == nil || !strings.Contains(err.Error(), memoryErrStateCorrupt) {
			t.Errorf("%s: want corrupt error, got %v", name, err)
		}
	}
}
