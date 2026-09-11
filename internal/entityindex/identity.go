package entityindex

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// IdentityKey was used by the pre-release global migration. It is ignored;
// parser selection is local and must never be changed by a synced marker.
const IdentityKey = "brain:entities-identity-revision"

// ProviderIdentity is an additive provider capability, separate from the wire
// schema and release version. Legacy providers omit it and use the empty value.
func ProviderIdentity(ctx context.Context, runner Runner, repo, binary string) (string, error) {
	if runner == nil {
		return "", fmt.Errorf("graph identity requires a command runner")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if binary == "" {
		binary = "entire"
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, _, err := runner.Run(ctx, repo, binary, "graph", "version", "--json")
	if err != nil {
		return "", fmt.Errorf("read graph identity revision: %w", err)
	}
	var info struct {
		IdentityRevision string `json:"identity_revision"`
	}
	if err := json.Unmarshal(out, &info); err != nil {
		return "", fmt.Errorf("read graph identity revision: %w", err)
	}
	if len(info.IdentityRevision) > 128 || strings.ContainsAny(info.IdentityRevision, "\r\n\x00") {
		return "", fmt.Errorf("invalid graph identity revision")
	}
	return info.IdentityRevision, nil
}

// RevisionKey isolates parser-derived records without rewriting legacy keys.
// An empty revision retains the frozen producer contract for older peers.
func RevisionKey(key, revision string) string {
	if revision == "" {
		return key
	}
	return "brain:entities-revision:" + hexSegment(revision) + ":" + hexSegment(key)
}

func originalRevisionKey(key, revision string) (string, bool) {
	if revision == "" {
		return key, !strings.HasPrefix(key, "brain:entities-revision:")
	}
	prefix := "brain:entities-revision:" + hexSegment(revision) + ":"
	if !strings.HasPrefix(key, prefix) {
		return "", false
	}
	raw, err := hex.DecodeString(strings.TrimPrefix(key, prefix))
	return string(raw), err == nil
}

// anyRevisionKey identifies source records when carrying history across revisions.
func anyRevisionKey(key string) string {
	const prefix = "brain:entities-revision:"
	if !strings.HasPrefix(key, prefix) {
		return key
	}
	parts := strings.Split(strings.TrimPrefix(key, prefix), ":")
	if len(parts) != 2 {
		return ""
	}
	revision, err := hex.DecodeString(parts[0])
	if err != nil || len(revision) == 0 {
		return ""
	}
	raw, err := hex.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	return string(raw)
}
