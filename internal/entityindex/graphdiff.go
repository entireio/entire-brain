package entityindex

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Runner runs external commands. It is the same shape as the brain's
// cli.CommandRunner seam, so the CLI passes its runner straight through and
// tests substitute a scripted one without any real binary on PATH.
type Runner interface {
	Run(ctx context.Context, dir, name string, args ...string) (stdout []byte, stderr []byte, err error)
}

// graphResult mirrors `entire graph diff --json`. producer_version is only
// emitted by newer providers; its absence is tolerated on purpose (an older
// `entire` on PATH must still produce a usable index). It is the ONLY version
// key this decodes: the provider's own `schema_version`, where other graph
// subcommands emit one, versions THAT command's envelope, not the tool, so
// copying it into the delta document's producer_version would have recorded a
// number that means something else.
type graphResult struct {
	IdentityRevision string            `json:"identity_revision,omitempty"`
	Warnings         []json.RawMessage `json:"warnings,omitempty"`
	ProducerVersion  string            `json:"producer_version"`
	Base             string            `json:"base"`
	Head             string            `json:"head"`
	Files            []graphFileChange `json:"files"`
}

type graphFileChange struct {
	Path    string              `json:"path"`
	OldPath string              `json:"old_path"`
	Status  string              `json:"status"`
	Changes []graphEntityChange `json:"changes"`
}

type graphEntityChange struct {
	Type            string `json:"type"`
	Kind            string `json:"kind"`
	Name            string `json:"name"`
	OldName         string `json:"old_name"`
	NewName         string `json:"new_name"`
	OldSignature    string `json:"old_signature"`
	NewSignature    string `json:"new_signature"`
	OldPath         string `json:"old_path"`
	NewPath         string `json:"new_path"`
	BeforeStartLine int    `json:"before_start_line"`
	AfterStartLine  int    `json:"after_start_line"`
	// No fingerprint field: the provider emits none per entity change, so
	// decoding one would only ever produce "". See EntityDelta.Fingerprint.
}

// DiffCommit computes one commit's delta document by shelling the semantic
// graph provider, exactly as the rest of the brain reaches it (a `graph`
// subcommand on the Entire CLI binary, resolved from PATH). base "" means the
// commit is a root: the empty tree is used so its entities are recorded as
// added rather than dropped.
func DiffCommit(ctx context.Context, runner Runner, repoDir, graphBinary, base, head string, now time.Time) (Delta, error) {
	return diffCommit(ctx, runner, repoDir, graphBinary, base, head, now, false, "")
}

func diffCommit(ctx context.Context, runner Runner, repoDir, graphBinary, base, head string, now time.Time, strict bool, revision string) (Delta, error) {
	if runner == nil {
		return Delta{}, fmt.Errorf("entityindex: command runner is required")
	}
	if strings.TrimSpace(graphBinary) == "" {
		graphBinary = "entire"
	}
	if strings.TrimSpace(base) == "" {
		base = EmptyTreeSHA
	}
	// --repo is passed EXPLICITLY rather than relying on the provider inheriting
	// the caller's cwd or ENTIRE_REPO_ROOT: the index is built for one named
	// repository, and a provider that silently resolved a different one would
	// write another repository's entities under these keys.
	args := []string{"graph", "diff"}
	if strings.TrimSpace(repoDir) != "" {
		args = append(args, "--repo", repoDir)
	}
	args = append(args, "--base", base, "--head", head, "--json")
	stdout, _, err := runner.Run(ctx, repoDir, graphBinary, args...)
	if err != nil {
		return Delta{}, fmt.Errorf("entityindex: graph diff %s..%s: %w", short(base), short(head), err)
	}
	if strict || revision != "" {
		var envelope graphResult
		if err := json.Unmarshal(stdout, &envelope); err != nil {
			return Delta{}, fmt.Errorf("migration requires a valid graph diff envelope: %w", err)
		}
		if envelope.IdentityRevision != revision {
			return Delta{}, fmt.Errorf("graph diff identity revision changed during indexing")
		}
		if strict {
			var fields map[string]json.RawMessage
			_ = json.Unmarshal(stdout, &fields)
			if _, ok := fields["files"]; !ok {
				return Delta{}, fmt.Errorf("migration graph diff is missing files")
			}
			if len(envelope.Warnings) > 0 {
				return Delta{}, fmt.Errorf("migration graph diff has warnings; refusing potentially incomplete history: %s", envelope.Warnings[0])
			}
			for _, file := range envelope.Files {
				for _, change := range file.Changes {
					if _, ok := entityDeltaFrom(file, change); !ok || change.Kind == "" {
						return Delta{}, fmt.Errorf("migration graph diff contains an unkeyable entity")
					}
				}
			}
		}
		if envelope.Base != base || envelope.Head != head {
			return Delta{}, fmt.Errorf("migration graph diff did not attest requested base/head")
		}
	}
	return ParseDiff(stdout, base, head, now)
}

// ParseDiff maps raw `entire graph diff --json` output onto the frozen delta
// document. It is separated from DiffCommit so the mapping is testable against
// recorded provider output with no process involved.
func ParseDiff(stdout []byte, base, head string, now time.Time) (Delta, error) {
	trimmed := strings.TrimSpace(string(stdout))
	delta := Delta{
		SchemaVersion: SchemaVersion,
		Producer:      Producer,
		Base:          base,
		Head:          head,
		ComputedAt:    now.UTC().Format(time.RFC3339),
		Entities:      []EntityDelta{},
	}
	if trimmed == "" {
		// A provider that found nothing semantic may print nothing at all. That
		// is an empty delta, not a failure: the commit is still indexed, so it
		// is never re-diffed.
		return delta, nil
	}
	var result graphResult
	if err := json.Unmarshal([]byte(trimmed), &result); err != nil {
		return Delta{}, fmt.Errorf("entityindex: parse graph diff for %s: %w", short(head), err)
	}
	delta.ProducerVersion = strings.TrimSpace(result.ProducerVersion)
	for _, file := range result.Files {
		for _, change := range file.Changes {
			entity, ok := entityDeltaFrom(file, change)
			if !ok {
				continue
			}
			delta.Entities = append(delta.Entities, entity)
		}
	}
	// Deterministic order so the same commit always yields byte-identical JSON
	// (the document is content-compared for idempotence and stored in git).
	sort.SliceStable(delta.Entities, func(i, j int) bool {
		if delta.Entities[i].Path != delta.Entities[j].Path {
			return delta.Entities[i].Path < delta.Entities[j].Path
		}
		if delta.Entities[i].Kind != delta.Entities[j].Kind {
			return delta.Entities[i].Kind < delta.Entities[j].Kind
		}
		if delta.Entities[i].Name != delta.Entities[j].Name {
			return delta.Entities[i].Name < delta.Entities[j].Name
		}
		return delta.Entities[i].Change < delta.Entities[j].Change
	})
	return delta, nil
}

// entityDeltaFrom folds one provider change onto the contract's shape. A change
// with no name or no resolvable path cannot be keyed, so it is dropped rather
// than written under a degenerate key.
func entityDeltaFrom(file graphFileChange, change graphEntityChange) (EntityDelta, bool) {
	name := firstNonEmpty(change.NewName, change.Name)
	if strings.TrimSpace(name) == "" {
		return EntityDelta{}, false
	}
	path := firstNonEmpty(change.NewPath, file.Path)
	if strings.TrimSpace(path) == "" {
		return EntityDelta{}, false
	}
	oldPath := firstNonEmpty(change.OldPath, file.OldPath)
	if oldPath == path {
		oldPath = ""
	}
	oldName := change.OldName
	if oldName == name {
		oldName = ""
	}
	startLine := change.AfterStartLine
	if startLine == 0 {
		// A removal has no after-image; keep the pre-change line so the record
		// still points somewhere useful.
		startLine = change.BeforeStartLine
	}
	return EntityDelta{
		Change:       normalizeChange(change.Type),
		Kind:         change.Kind,
		Name:         name,
		OldName:      oldName,
		Path:         path,
		OldPath:      oldPath,
		Signature:    change.NewSignature,
		OldSignature: change.OldSignature,
		StartLine:    startLine,
	}, true
}

// normalizeChange maps the provider's change vocabulary onto the contract's.
// Anything unrecognized folds to "modified": an unknown change type still means
// the entity was touched by this commit, which is the whole question the index
// answers, so folding is strictly better than dropping the record.
func normalizeChange(providerType string) string {
	switch strings.ToLower(strings.TrimSpace(providerType)) {
	case "added":
		return ChangeAdded
	case "removed", "deleted":
		return ChangeRemoved
	case "renamed":
		return ChangeRenamed
	case "moved":
		return ChangeMoved
	default: // body_changed, signature_changed, …
		return ChangeModified
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
