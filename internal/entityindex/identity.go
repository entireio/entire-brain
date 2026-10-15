package entityindex

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/entireio/entire-brain/internal/providerdetail"
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
		return "", identityError(binary, err)
	}
	var info struct {
		IdentityRevision string `json:"identity_revision"`
	}
	if err := json.Unmarshal(out, &info); err != nil {
		return "", identityError(binary, err)
	}
	if len(info.IdentityRevision) > 128 || strings.ContainsAny(info.IdentityRevision, "\r\n\x00") {
		return "", fmt.Errorf("invalid graph identity revision")
	}
	return info.IdentityRevision, nil
}

// identityFailure carries the SUMMARISED text a reader sees while keeping the
// original error reachable, so errors.Is/errors.As on a context deadline or an
// exec.Error still work across the summarisation.
type identityFailure struct {
	message string
	cause   error
}

func (e *identityFailure) Error() string { return e.message }
func (e *identityFailure) Unwrap() error { return e.cause }

// identityError reports a failed `graph version --json` the way the semantic
// index reports the identical cause.
//
// It used to be fmt.Errorf("read graph identity revision: %w", err), which
// embedded the subprocess's entire stderr verbatim. When the entire-graph
// plugin is not installed that stderr is the host CLI's full --help screen,
// so a first run printed 60 lines of command groups ending in
// `Did you mean this? grant` -- implying the reader had mistyped a command
// they never typed -- twice, once in the progress line and once in the
// summary block, and pushed setup's real 12-line report below the fold.
// The semantic index already answered the same cause in one line naming the
// install step; this is that answer, from the same shared rule, so the two
// components cannot disagree. The "read graph identity revision" prefix is
// kept: callers and the doctor's provider axis match on it.
func identityError(binary string, err error) error {
	detail := strings.TrimSpace(err.Error())
	message := "read graph identity revision"
	if summary := providerdetail.Summary(detail); summary != "" {
		message += ": " + summary
	}
	// Detection runs against the FULL detail, never the summary, so the marker
	// is still found when the host CLI prints it below its usage text.
	if providerdetail.Missing(detail) {
		message += "; " + providerdetail.MissingRemedy(binary, os.LookupEnv)
	}
	return &identityFailure{message: message, cause: err}
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
