package releasecheck

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
)

type benchmarkArchive struct {
	SchemaVersion int                 `json:"schema_version"`
	SourceCommit  string              `json:"source_commit"`
	Experiments   []archiveExperiment `json:"experiments"`
	Files         []archiveFile       `json:"files"`
}

type archiveExperiment struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Question    string   `json:"question"`
	Conclusion  string   `json:"conclusion"`
	Limitations string   `json:"limitations"`
	Records     []string `json:"records"`
	Context     []string `json:"context"`
}

type archiveFile struct {
	Path       string   `json:"path"`
	SHA256     string   `json:"sha256"`
	Role       string   `json:"role"`
	References []string `json:"references,omitempty"`
}

func TestHistoricalBenchmarkArchive(t *testing.T) {
	if err := validateBenchmarkArchive(os.DirFS("../../benchmarks/agent-brain")); err != nil {
		t.Fatal(err)
	}
}

// Only read static records. Historical commands in JSON and Markdown are data;
// this check never runs a benchmark, imports a fixture, or requires Git history.
func validateBenchmarkArchive(root fs.FS) error {
	entries, err := fs.ReadDir(root, ".")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == "archive-manifest.json" && !entry.Type().IsRegular() {
			return fmt.Errorf("archive manifest must be a regular file")
		}
	}
	data, err := fs.ReadFile(root, "archive-manifest.json")
	if err != nil {
		return err
	}
	var manifest benchmarkArchive
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("archive manifest has trailing content")
	}
	if manifest.SchemaVersion != 1 || !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(manifest.SourceCommit) {
		return fmt.Errorf("archive requires schema version 1 and a full source commit")
	}
	if len(manifest.Files) == 0 || len(manifest.Experiments) == 0 {
		return fmt.Errorf("archive inventory and experiment index must not be empty")
	}
	files := map[string]archiveFile{}
	for _, file := range manifest.Files {
		if !fs.ValidPath(file.Path) || file.Path == "." || strings.ContainsAny(file.Path, `\:`) {
			return fmt.Errorf("invalid archive path: %q", file.Path)
		}
		if file.Path == "README.md" || file.Path == "archive-manifest.json" {
			return fmt.Errorf("editorial archive files cannot be historical records: %s", file.Path)
		}
		switch path.Ext(file.Path) {
		case ".md", ".json", ".jsonl", ".ndjson", ".txt", ".log", ".fixture":
		default:
			return fmt.Errorf("unsupported archive file type: %s", file.Path)
		}
		if _, exists := files[file.Path]; exists {
			return fmt.Errorf("duplicate archive path: %s", file.Path)
		}
		hash, err := hex.DecodeString(file.SHA256)
		if err != nil || len(hash) != sha256.Size || file.SHA256 != strings.ToLower(file.SHA256) {
			return fmt.Errorf("invalid SHA-256: %s", file.Path)
		}
		if file.Role != "record" && file.Role != "context" {
			return fmt.Errorf("invalid archive role: %s", file.Path)
		}
		files[file.Path] = file
	}
	for _, file := range manifest.Files {
		seen := map[string]bool{}
		for _, ref := range file.References {
			if _, ok := files[ref]; !ok || ref == file.Path || seen[ref] {
				return fmt.Errorf("invalid archive reference from %s to %s", file.Path, ref)
			}
			seen[ref] = true
		}
	}
	indexed := map[string]bool{}
	ids := map[string]bool{}
	for _, experiment := range manifest.Experiments {
		if experiment.ID == "" || ids[experiment.ID] || strings.TrimSpace(experiment.Title) == "" ||
			strings.TrimSpace(experiment.Question) == "" || strings.TrimSpace(experiment.Conclusion) == "" ||
			strings.TrimSpace(experiment.Limitations) == "" || len(experiment.Records) == 0 {
			return fmt.Errorf("incomplete or duplicate experiment: %s", experiment.ID)
		}
		ids[experiment.ID] = true
		for role, refs := range map[string][]string{"record": experiment.Records, "context": experiment.Context} {
			seen := map[string]bool{}
			for _, ref := range refs {
				if file, ok := files[ref]; !ok || file.Role != role || seen[ref] {
					return fmt.Errorf("invalid %s reference in experiment %s: %s", role, experiment.ID, ref)
				}
				seen[ref], indexed[ref] = true, true
			}
		}
	}
	for name := range files {
		if !indexed[name] {
			return fmt.Errorf("archive file has no experiment entry: %s", name)
		}
	}
	seen := map[string]bool{}
	err = fs.WalkDir(root, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("archive contains a symlink or special file: %s", name)
		}
		if name == "README.md" || name == "archive-manifest.json" {
			return nil
		}
		file, ok := files[name]
		if !ok {
			return fmt.Errorf("archive file missing from inventory: %s", name)
		}
		data, err := fs.ReadFile(root, name)
		if err != nil {
			return err
		}
		hash := sha256.Sum256(data)
		if hex.EncodeToString(hash[:]) != file.SHA256 {
			return fmt.Errorf("archive SHA-256 mismatch: %s", name)
		}
		seen[name] = true
		return nil
	})
	if err != nil {
		return err
	}
	for name := range files {
		if !seen[name] {
			return fmt.Errorf("missing archive file: %s", name)
		}
	}
	return nil
}

func TestArchiveIntegrityRejectsDamagedRecordsAndReferences(t *testing.T) {
	for _, scenario := range []string{
		"valid", "changed record", "missing record", "extra file", "duplicate path", "missing reference",
		"traversal", "windows separator", "missing limitation", "invalid hash", "invalid source commit",
		"unindexed record", "wrong reference role", "symlink record", "symlink manifest", "executable file", "trailing JSON",
	} {
		t.Run(scenario, func(t *testing.T) {
			root := fstest.MapFS{
				"result.json":     {Data: []byte(`{"passed":1,"failed":1}`)},
				"tasks/task.json": {Data: []byte(`{"question":"historical task","validation":["retired command"]}`)},
			}
			manifest := benchmarkArchive{
				SchemaVersion: 1, SourceCommit: strings.Repeat("a", 40),
				Experiments: []archiveExperiment{{ID: "example", Title: "Example", Question: "Question", Conclusion: "Mixed result", Limitations: "Historical only", Records: []string{"result.json"}, Context: []string{"tasks/task.json"}}},
			}
			for _, name := range []string{"result.json", "tasks/task.json"} {
				hash := sha256.Sum256(root[name].Data)
				role := "record"
				if strings.HasPrefix(name, "tasks/") {
					role = "context"
				}
				manifest.Files = append(manifest.Files, archiveFile{Path: name, SHA256: hex.EncodeToString(hash[:]), Role: role})
			}
			manifest.Files[0].References = []string{"tasks/task.json"}
			switch scenario {
			case "changed record":
				root["result.json"].Data = []byte(`{"passed":2,"failed":0}`)
			case "missing record":
				delete(root, "result.json")
			case "extra file":
				root["unlisted.json"] = &fstest.MapFile{Data: []byte("{}")}
			case "duplicate path":
				manifest.Files = append(manifest.Files, manifest.Files[0])
			case "missing reference":
				manifest.Files[0].References = []string{"tasks/missing.json"}
			case "traversal":
				manifest.Files[0].Path = "../result.json"
			case "windows separator":
				manifest.Files[0].Path = `..\result.json`
			case "missing limitation":
				manifest.Experiments[0].Limitations = ""
			case "invalid hash":
				manifest.Files[0].SHA256 = "abcd"
			case "invalid source commit":
				manifest.SourceCommit = "main"
			case "unindexed record":
				manifest.Experiments[0].Context = nil
			case "wrong reference role":
				manifest.Files[1].Role = "record"
			case "symlink record":
				root["result.json"].Mode = fs.ModeSymlink
			case "executable file":
				manifest.Files[0].Path = "runner.sh"
			}
			data, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			root["archive-manifest.json"] = &fstest.MapFile{Data: data}
			if scenario == "symlink manifest" {
				root["archive-manifest.json"].Mode = fs.ModeSymlink
			}
			if scenario == "trailing JSON" {
				root["archive-manifest.json"].Data = append(data, []byte(`{}`)...)
			}
			err = validateBenchmarkArchive(root)
			if (err == nil) != (scenario == "valid") {
				t.Fatalf("%s: %v", scenario, err)
			}
		})
	}
}
