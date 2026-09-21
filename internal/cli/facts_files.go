package cli

import (
	"github.com/spf13/cobra"

	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Facts as plain files.
//
// A brain's durable facts live in an internal store, reachable through `recall`
// and the MCP tools and nowhere else. That is fine until you want to do the
// obvious thing — grep them, read one in an editor, diff two branches, paste a
// decision into a review — and discover that the only way in is a program.
//
// This writes the same facts out as one Markdown file per fact, laid out by
// taxonomy path, so ordinary tools work on them:
//
//	constraints/retry/policy/fact-57e03e4a.md
//	architecture/api/ports/fact-1b3c8686.md
//
// The export is a projection, not a second source of truth. It is written fresh
// each time, the store remains authoritative, and nothing here reads the files
// back in. That is a deliberate limit: a format people edit and a store a
// program owns will diverge, and the honest version of this feature is the one
// that says which direction is canonical rather than pretending to sync.

// factFileName is the per-fact filename. The id is already content-addressed
// and unique; the short form keeps paths readable while staying collision-free
// within a taxonomy directory.
func factFileName(id string) string {
	trimmed := strings.TrimPrefix(id, "fact:")
	if len(trimmed) > 12 {
		trimmed = trimmed[:12]
	}
	if trimmed == "" {
		trimmed = "unidentified"
	}
	return "fact-" + trimmed + ".md"
}

// factFileDir maps a taxonomy path to a directory. A fact with no path lands in
// "unfiled" rather than the export root, so the root stays a list of categories
// and an unfiled fact is visibly unfiled instead of quietly mixed in.
func factFileDir(paths []string) string {
	if len(paths) == 0 || strings.TrimSpace(paths[0]) == "" {
		return "unfiled"
	}
	segments := strings.Split(strings.TrimSpace(paths[0]), ".")
	safe := make([]string, 0, len(segments))
	for _, segment := range segments {
		segment = sanitizeFactPathSegment(segment)
		if segment != "" {
			safe = append(safe, segment)
		}
	}
	if len(safe) == 0 {
		return "unfiled"
	}
	return filepath.Join(safe...)
}

// sanitizeFactPathSegment turns a taxonomy segment into a usable directory
// name: spaces, punctuation and non-ASCII become "-", and the result is
// trimmed. That is its actual job — a segment like "retry policy" or "API/v2"
// has to become something you can cd into and tab-complete.
//
// It is NOT what keeps the export inside its directory, and it would be wrong
// to describe it that way. Containment comes from two other places: the
// taxonomy is split on ".", which destroys ".." before a segment ever exists
// (strings.Split("..", ".") is three empty strings, never ".."), and
// filepath.Join cleans the result under the root. The "." / ".." check below
// is therefore unreachable today; it is kept as a guard in case the splitting
// ever changes, not because it currently fires.
func sanitizeFactPathSegment(segment string) string {
	segment = strings.TrimSpace(segment)
	if segment == "." || segment == ".." {
		return ""
	}
	var b strings.Builder
	for _, r := range segment {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

// renderFactFile is the file body: YAML-ish front matter a human can read and a
// script can parse, then the fact itself as prose. The fact text is last and
// unindented so `grep -r` output reads as the sentence, not as metadata.
func renderFactFile(fact factRecord) string {
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "id: %s\n", fact.ID)
	if fact.Kind != "" {
		fmt.Fprintf(&b, "kind: %s\n", fact.Kind)
	}
	if len(fact.Paths) > 0 {
		fmt.Fprintf(&b, "paths: %s\n", strings.Join(fact.Paths, ", "))
	}
	if fact.Branch != "" {
		fmt.Fprintf(&b, "branch: %s\n", fact.Branch)
	}
	if fact.Status != "" {
		fmt.Fprintf(&b, "status: %s\n", fact.Status)
	}
	if fact.Origin != "" {
		fmt.Fprintf(&b, "origin: %s\n", fact.Origin)
	}
	if len(fact.Locus) > 0 {
		fmt.Fprintf(&b, "locus: %s\n", strings.Join(fact.Locus, ", "))
	}
	if !fact.CreatedAt.IsZero() {
		fmt.Fprintf(&b, "created_at: %s\n", fact.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"))
	}
	if !fact.UpdatedAt.IsZero() {
		fmt.Fprintf(&b, "updated_at: %s\n", fact.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"))
	}
	// Provenance is what separates a fact from an assertion, so it belongs in
	// the file rather than only in the store.
	for _, anchor := range fact.Provenance {
		switch {
		case anchor.Commit != "":
			fmt.Fprintf(&b, "anchor: commit %s\n", anchor.Commit)
		case anchor.SessionID != "":
			fmt.Fprintf(&b, "anchor: session %s\n", anchor.SessionID)
		case anchor.CheckpointID != "":
			fmt.Fprintf(&b, "anchor: checkpoint %s\n", anchor.CheckpointID)
		}
	}
	b.WriteString("---\n\n")
	b.WriteString(strings.TrimSpace(fact.Text))
	b.WriteString("\n")
	return b.String()
}

type factFilesResult struct {
	Dir     string   `json:"dir"`
	Written int      `json:"written"`
	Files   []string `json:"files"`
}

// factFilesManifestName records what the previous export wrote.
//
// It is what makes "rewritten on each run" safe to implement. The obvious way
// to honour that promise is to clear the directory first, and --dir points
// wherever the user says — at a notes folder, at a repository, at a home
// directory — so clearing it would delete work that has nothing to do with the
// brain. With a manifest, an export can only ever remove a file that a previous
// export recorded writing.
const factFilesManifestName = ".entire-fact-files.json"

type factFilesManifest struct {
	Files []string `json:"files"`
}

func readFactFilesManifest(dir string) factFilesManifest {
	var manifest factFilesManifest
	data, err := os.ReadFile(filepath.Join(dir, factFilesManifestName))
	if err != nil {
		// No manifest means either a first run or a directory this command has
		// never written to. Removing nothing is the only safe reading of both.
		return manifest
	}
	_ = json.Unmarshal(data, &manifest)
	return manifest
}

// pruneStaleFactFiles removes files the last export wrote and this one did not.
//
// A fact that was retracted, or whose taxonomy path moved, leaves a file behind
// otherwise — and a grep over the export then returns something the brain no
// longer believes, presented exactly like something it does.
func pruneStaleFactFiles(dir string, previous factFilesManifest, written map[string]bool) {
	var dirs []string
	for _, rel := range previous.Files {
		if written[rel] {
			continue
		}
		// The manifest lives inside a directory the user can edit, so its
		// entries are input. Two different escapes have to be refused: a path
		// that climbs out textually, and a path whose components are all plain
		// names but whose parent is a symlink — the second is invisible to any
		// string check and is what redirects a delete outside --dir.
		clean := filepath.Clean(rel)
		if filepath.IsAbs(clean) || clean == "." || strings.HasPrefix(clean, "..") {
			continue
		}
		if err := rejectExistingSymlinkPathComponents(dir, clean); err != nil {
			continue
		}
		if err := os.Remove(filepath.Join(dir, clean)); err != nil {
			continue
		}
		if parent := filepath.Dir(clean); parent != "." {
			dirs = append(dirs, parent)
		}
	}
	// Prune directories the removals emptied, deepest first, so a whole
	// taxonomy branch that no longer has facts does not linger as empty
	// folders. os.Remove refuses a non-empty directory, which is exactly the
	// guard wanted here.
	sort.Sort(sort.Reverse(sort.StringSlice(dirs)))
	for _, rel := range dirs {
		for current := rel; current != "."; current = filepath.Dir(current) {
			if err := rejectExistingSymlinkPathComponents(dir, current); err != nil {
				break
			}
			if err := os.Remove(filepath.Join(dir, current)); err != nil {
				break
			}
		}
	}
}

// writeFactFiles materialises facts under dir. Returns the relative paths
// written, sorted, so the caller can print or diff them deterministically.
//
// Files a previous export wrote and this one does not are removed, so the
// directory reflects the brain as it is now rather than the union of every
// export ever run.
func writeFactFiles(dir string, facts []factRecord) (factFilesResult, error) {
	result := factFilesResult{Dir: dir}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return result, fmt.Errorf("create export directory: %w", err)
	}
	previous := readFactFilesManifest(dir)
	// Two facts whose ids share a 12-character prefix inside one taxonomy
	// directory would land on the same filename. It takes an improbable
	// collision to happen at all, and the cost if it does is the part worth
	// guarding: the second write silently replaces the first while the result
	// still counts both, so a fact disappears from the projection and the
	// export reports success.
	claimed := make(map[string]string, len(facts))
	for _, fact := range facts {
		relDir := factFileDir(fact.Paths)
		// A symlink planted at any component of the taxonomy path redirects the
		// write out of --dir entirely. The `..` and absolute-path checks
		// elsewhere in this file do not catch that: every component is a plain
		// name and the escape happens in the filesystem, not in the string.
		// Every other place in this codebase that writes under a user-named
		// output directory checks this first.
		if err := rejectExistingSymlinkPathComponents(dir, relDir); err != nil {
			return result, fmt.Errorf("refusing to write into %s: %w", relDir, err)
		}
		absDir := filepath.Join(dir, relDir)
		if err := os.MkdirAll(absDir, 0o755); err != nil {
			return result, fmt.Errorf("create %s: %w", relDir, err)
		}
		rel := filepath.Join(relDir, factFileName(fact.ID))
		if owner, taken := claimed[rel]; taken {
			return result, fmt.Errorf("facts %s and %s both map to %s; one would silently replace the other", owner, fact.ID, rel)
		}
		claimed[rel] = fact.ID
		if err := rejectExistingSymlinkPathComponents(dir, rel); err != nil {
			return result, fmt.Errorf("refusing to write %s: %w", rel, err)
		}
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(renderFactFile(fact)), 0o644); err != nil {
			return result, fmt.Errorf("write %s: %w", rel, err)
		}
		result.Files = append(result.Files, rel)
	}
	sort.Strings(result.Files)
	result.Written = len(result.Files)

	written := make(map[string]bool, len(result.Files))
	for _, rel := range result.Files {
		written[rel] = true
	}
	pruneStaleFactFiles(dir, previous, written)

	// The manifest is written last: if anything above failed, the previous
	// manifest still describes what is actually on disk, and the next run
	// reconciles from a true record rather than an optimistic one.
	if data, err := json.MarshalIndent(factFilesManifest{Files: result.Files}, "", "  "); err == nil {
		_ = os.WriteFile(filepath.Join(dir, factFilesManifestName), append(data, '\n'), 0o644)
	}
	return result, nil
}

func newFactsFilesCommand(opts Options) *cobra.Command {
	var (
		dir        string
		branch     string
		jsonOut    bool
		includeAll bool
	)
	cmd := &cobra.Command{
		Use:   "files",
		Short: "Write durable facts out as plain Markdown files you can grep, read and diff",
		Long: "Write durable facts out as one Markdown file per fact, laid out by taxonomy path.\n\n" +
			"The export is a projection: the brain remains the source of truth, the directory is\n" +
			"rewritten on each run, and nothing reads these files back in. Edit them freely; edit\n" +
			"the brain with remember and retract.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, brainDir, resolvedBranch, err := resolveFactsTarget(cmd.Context(), opts, agentSurfaceTarget(opts, nil), branch)
			if err != nil {
				return err
			}
			facts, err := loadFacts(brainDir, resolvedBranch)
			if err != nil {
				return err
			}
			if !includeAll {
				// Retracted and superseded facts are history, not current
				// knowledge. Writing them beside live ones by default would
				// make a grep return things the brain no longer believes.
				active := facts[:0:0]
				for _, fact := range facts {
					if fact.Status == "" || fact.Status == "active" {
						active = append(active, fact)
					}
				}
				facts = active
			}
			result, err := writeFactFiles(dir, facts)
			if err != nil {
				return err
			}
			if jsonOut {
				return writeIndentedJSON(cmd.OutOrStdout(), result)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "wrote %d facts to %s\n", result.Written, result.Dir)
			for _, file := range result.Files {
				fmt.Fprintf(cmd.OutOrStdout(), "  %s\n", file)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "brain-facts", "Directory to write the files into")
	cmd.Flags().StringVar(&branch, "branch", "", "Branch to export facts from (default: current)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit the manifest as JSON")
	cmd.Flags().BoolVar(&includeAll, "all", false, "Include superseded and retracted facts")
	return cmd
}
