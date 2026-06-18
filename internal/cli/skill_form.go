package cli

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
)

// Skill install destinations + skill-memory recording (Pattern Consolidation).
//
// There is exactly ONE skill-creation path: `patterns skills form <task-id>`
// (skill_cmd.go), which synthesizes a SKILL.md from a corroborated task
// candidate. Skills are deliberately NOT formed from raw procedures/practices —
// those are diagnostic evidence, not skill candidates. The helpers here (install
// destinations, the OS-path boundary, and the skill-memory upsert) are shared by
// the repo and workspace skill-form paths.

type skillDestination struct {
	Agents []string `json:"agents"`
	Path   string   `json:"path"`
}

// recordSkillDecision upserts a skill-memory record by pattern/task id, preserving
// the rest of the user-state file and the original creation time.
func recordSkillDecision(brainDir string, rec skillMemoryRecord) error {
	existing, err := loadBrainSkillMemory(brainDir)
	if err != nil {
		return err
	}
	out := make([]skillMemoryRecord, 0, len(existing)+1)
	replaced := false
	for _, r := range existing {
		if r.PatternID == rec.PatternID {
			if !r.CreatedAt.IsZero() {
				rec.CreatedAt = r.CreatedAt
			}
			out = append(out, rec)
			replaced = true
			continue
		}
		out = append(out, r)
	}
	if !replaced {
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PatternID < out[j].PatternID })
	return writeBrainSkillMemory(brainDir, out)
}

// skillDestinations resolves the install paths for a target+scope. "standard" is
// the cross-agent .agents/skills path (read by copilot-cli, cursor, gemini, pi);
// the holdouts get their own roots. "all" writes the minimal covering set.
func skillDestinations(target, scope, name, repoDir string) ([]skillDestination, error) {
	standardAgents := []string{"copilot-cli", "cursor", "gemini", "pi"}
	// Destination paths are kept slash-style for stable, cross-platform display
	// and JSON; they are converted to an OS path only at the write boundary
	// (skillFilePath). This keeps tests and output identical on Windows.
	rel := path.Join("skills", name, "SKILL.md")

	mk := func(globalRoot, repoSub string, agents []string) (skillDestination, error) {
		var base string
		switch scope {
		case "global":
			base = globalRoot
		case "repo":
			if repoDir == "" {
				return skillDestination{}, fmt.Errorf("repo scope requires a local repository")
			}
			base = filepath.ToSlash(filepath.Join(repoDir, repoSub))
		default:
			return skillDestination{}, fmt.Errorf("invalid scope %q (want global|repo)", scope)
		}
		return skillDestination{Agents: agents, Path: path.Join(base, rel)}, nil
	}

	codexGlobal := filepath.ToSlash(os.Getenv("CODEX_HOME"))
	if codexGlobal == "" {
		codexGlobal = "~/.codex"
	}
	specs := map[string]struct {
		globalRoot, repoSub string
		agents              []string
	}{
		"standard":        {"~/.agents", ".agents", standardAgents},
		"claude-code":     {"~/.claude", ".claude", []string{"claude-code", "opencode"}},
		"codex":           {codexGlobal, ".codex", []string{"codex"}},
		"factoryai-droid": {"~/.factory", ".factory", []string{"factoryai-droid"}},
	}

	var order []string
	switch target {
	case "all":
		order = []string{"standard", "claude-code", "codex", "factoryai-droid"}
	case "standard", "claude-code", "codex", "factoryai-droid":
		order = []string{target}
	default:
		return nil, fmt.Errorf("invalid target %q (want standard|claude-code|codex|factoryai-droid|all)", target)
	}

	var dests []skillDestination
	for _, key := range order {
		s := specs[key]
		d, err := mk(s.globalRoot, s.repoSub, s.agents)
		if err != nil {
			return nil, err
		}
		dests = append(dests, d)
	}
	return dests, nil
}

func overwriteHint(dests []skillDestination) string {
	for _, d := range dests {
		if _, err := os.Stat(skillFilePath(d.Path)); err == nil {
			return " (add --force to overwrite existing files)"
		}
	}
	return ""
}

// skillFilePath turns a slash-style destination path into an OS filesystem path,
// expanding a leading ~ first. Display/JSON keep the slash form; only writes use
// this.
func skillFilePath(p string) string {
	return filepath.FromSlash(expandHomePath(p))
}

func sha256Sum(data []byte) []byte {
	sum := sha256.Sum256(data)
	return sum[:]
}
