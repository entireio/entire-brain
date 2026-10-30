package factstore

import (
	"fmt"
	"sort"
	"strings"

	"github.com/entireio/entire-brain/factmerge"
)

// LoadStoreFiles scans the facts directory and returns every stored fact
// grouped by store directory — one entry per facts.ndjson file, keyed by the
// on-disk directory name. A FILE is the unit recall reads and writers replace,
// which is not the same thing as the branch recorded on each record: the
// hosted pull mirrors the repo feed into the current branch's store without
// restamping, so one record legitimately appears in many files. Operations
// that rewrite stores must therefore iterate files, never record-branch
// buckets — writing a cross-file bucket into one file is how a store ends up
// holding the same record many times over (COR-2220).
func LoadStoreFiles(brainDir string) (map[string][]factmerge.Record, error) {
	return loadRepoFactViews(brainDir)
}

// WriteStoreFile replaces one store file in place, addressed by its
// on-disk directory name rather than a branch name. The directory name is a
// lossy slug, so there is no way back to the branch — but a file-scoped
// rewrite never needs one.
func WriteStoreFile(brainDir, dirName string, records []factmerge.Record) error {
	return writeRepoFactView(brainDir, dirName, records)
}

// LoadAllBranches returns every stored fact grouped by the branch recorded
// on each record (not the on-disk slug, which is lossy). It is the read-side
// view for the manifest and whole-store reports. Because the hosted pull
// mirrors feed records into other branches' files, a bucket here can span
// files and repeat ids across them — safe to COUNT and read, but never write a
// bucket back into a single file (see LoadStoreFiles).
func LoadAllBranches(brainDir string) (map[string][]factmerge.Record, error) {
	byFile, err := LoadStoreFiles(brainDir)
	if err != nil {
		return nil, err
	}
	byBranch := map[string][]factmerge.Record{}
	for _, records := range byFile {
		for _, record := range records {
			byBranch[record.Branch] = append(byBranch[record.Branch], record)
		}
	}
	return byBranch, nil
}

// OtherBranchBlindSpot reports active facts held on branches other than the one
// queried. It returns empty when the caller did not say which branch it
// queried, when the store cannot be read, or when no other branch holds
// anything active — in each case the caller falls through to its normal note.
func OtherBranchBlindSpot(brainDir, branch string) string {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return ""
	}
	byBranch, err := LoadAllBranches(brainDir)
	if err != nil {
		return ""
	}
	total := 0
	var names []string
	for other, records := range byBranch {
		if other == branch {
			continue
		}
		active := 0
		for _, record := range records {
			if record.Status == factmerge.StatusActive {
				active++
			}
		}
		if active == 0 {
			continue
		}
		total += active
		names = append(names, other)
	}
	if total == 0 {
		return ""
	}
	// Deterministic: LoadAllBranches returns a map, and Go randomises map
	// iteration, so an unsorted list would reorder between identical runs.
	sort.Strings(names)
	shown, suffix := names, ""
	if len(shown) > 3 {
		shown, suffix = shown[:3], ", ..."
	}
	return fmt.Sprintf("note: no matching memories on %s, but %d active memory(ies) on %d other branch(es): %s%s — retry with --branch",
		branch, total, len(names), strings.Join(shown, ", "), suffix)
}
