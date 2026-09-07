package entityindex

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ashtom/entire-brain/internal/factgitmeta"
)

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

// CheckIdentity prevents mixing history computed with different parser rules.
func CheckIdentity(store *factgitmeta.MetaStore, revision string) error {
	previous, _, err := store.ReadString(projectTarget, IdentityKey)
	if err != nil {
		return err
	}
	state, err := store.State()
	if err != nil {
		return err
	}
	for _, v := range state.Strings {
		if v.Key != ForwardKey || v.Target.Type != CommitTarget("x").Type {
			continue
		}
		var delta Delta
		if err := json.Unmarshal([]byte(v.Value), &delta); err != nil {
			return fmt.Errorf("invalid entity history delta: %w", err)
		}
		if previous != revision || delta.IdentityRevision != revision {
			return fmt.Errorf("entity history identity revision changed from %q to %q; run `entire brain entities migrate` before backfill or history queries", delta.IdentityRevision, revision)
		}
	}
	return nil
}
