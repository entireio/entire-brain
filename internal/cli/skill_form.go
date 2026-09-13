package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
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

	// base and rel are the two halves of Path kept apart for the write
	// boundary: base is the agent config root the skill belongs in, rel is the
	// slash-style path under it. writeSkillFile needs them separately to prove
	// the write lands inside base and passes through no symlink. They are
	// unexported so the JSON shape is unchanged.
	base string
	rel  string
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
	// The name is parsed out of the synthesis agent's `name:` frontmatter, and
	// that agent's input is session evidence — text a hostile repo can reach.
	// path.Join COLLAPSES "..", it does not reject it, so an unsanitized name
	// walks straight out of the skills directory and plants an
	// agent-instruction file at a path of the attacker's choosing. Reduce it to
	// exactly one safe path component first.
	name = safeSkillNameComponent(name)
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
		return skillDestination{Agents: agents, Path: path.Join(base, rel), base: base, rel: rel}, nil
	}

	// A harness told WHERE ITS CONFIGURATION HOME IS must be written to there,
	// not to the default the user moved away from.
	//
	// CODEX_HOME was already honoured; CLAUDE_CONFIG_DIR was not, and the two are
	// the same variable for the same reason -- Claude Code resolves its whole
	// configuration home from it, which is how an alternate or sandboxed install
	// is pointed somewhere other than ~/.claude. The asymmetry is silent, and
	// that is what makes it worth closing: ~/.claude usually still exists, so the
	// skill lands in a directory that looks right, the command reports success,
	// and the agent the file was written for never sees it. An unset variable
	// keeps the historical path byte for byte.
	specs := map[string]struct {
		globalRoot, repoSub string
		agents              []string
	}{
		"standard":        {"~/.agents", ".agents", standardAgents},
		"claude-code":     {harnessHomeRoot("CLAUDE_CONFIG_DIR", "~/.claude"), ".claude", []string{"claude-code", "opencode"}},
		"codex":           {harnessHomeRoot("CODEX_HOME", "~/.codex"), ".codex", []string{"codex"}},
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

// safeSkillNameComponent reduces a synthesized skill name to exactly one safe
// path component.
//
// safePathComponent alone is enough for containment — its output is ASCII
// [a-z0-9._-] trimmed of "-._", so it can never be empty, ".", ".." or carry a
// separator. It is not enough for identity: a name with no representable
// characters at all (an all-CJK name, say) collapses to the bare fallback, and
// every such skill would then install over the same directory. Append a short
// digest of the original name in exactly that case, so distinct names stay
// distinct while every already-representable name keeps its plain slug and
// existing installs do not move.
func safeSkillNameComponent(name string) string {
	const fallback = "skill"
	slug := safePathComponent(name, fallback, 64)
	if slug != fallback || strings.EqualFold(strings.TrimSpace(name), fallback) {
		return slug
	}
	sum := sha256.Sum256([]byte(name))
	return fallback + "-" + hex.EncodeToString(sum[:])[:8]
}

func overwriteHint(dests []skillDestination) string {
	for _, d := range dests {
		if skillFileExists(d.Path) {
			return " (add --force to overwrite existing files)"
		}
	}
	return ""
}

// skillFileExists reports whether anything already occupies a destination.
// It Lstats: os.Stat resolves the link and reports a DANGLING symlink as "does
// not exist", which would let the overwrite guard pass and the write then
// create the file the link points at, outside the skills directory entirely.
func skillFileExists(destPath string) bool {
	_, err := os.Lstat(skillFilePath(destPath))
	return err == nil
}

// writeSkillFile installs one rendered SKILL.md.
//
// A SKILL.md is an agent-instruction file, and its parent directories sit in
// the repo working tree (repo scope) or a home agent config (global scope) —
// places an untrusted checkout can pre-seed. Every component from the agent
// root down to the leaf is checked for a symlink before anything is created, so
// the write cannot be redirected out of the destination it reported in the
// --yes preview. The leaf is included in that check, which is what stops a
// dangling link from being written through.
//
// The base is checked separately, and must be: the per-component walk
// EvalSymlinks-normalizes its own root before the containment test, so a base
// that is ITSELF a link is treated as legitimate. Git stores symlinks, so an
// untrusted checkout can ship ".claude -> $HOME/.claude" and turn a repo-scope
// install into a write to the user's real home agent config.
func writeSkillFile(d skillDestination, data []byte) error {
	base := skillFilePath(d.base)
	rel := filepath.FromSlash(d.rel)
	if err := rejectSymlinkedBrainRoot(base); err != nil {
		return fmt.Errorf("refusing to write skill to %s: %w", d.Path, err)
	}
	if err := rejectExistingSymlinkPathComponents(base, rel); err != nil {
		return fmt.Errorf("refusing to write skill to %s: %w", d.Path, err)
	}
	abs := filepath.Join(base, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return fmt.Errorf("create skill dir: %w", err)
	}
	// Re-check after MkdirAll: the directories that did not exist a moment ago
	// do now, and creating them must not have traversed a link.
	if err := rejectSymlinkedBrainRoot(base); err != nil {
		return fmt.Errorf("refusing to write skill to %s: %w", d.Path, err)
	}
	if err := rejectExistingSymlinkPathComponents(base, rel); err != nil {
		return fmt.Errorf("refusing to write skill to %s: %w", d.Path, err)
	}
	if err := os.WriteFile(abs, data, 0o644); err != nil {
		return fmt.Errorf("write skill: %w", err)
	}
	return nil
}

// skillFilePath turns a slash-style destination path into an OS filesystem path,
// expanding a leading ~ first. Display/JSON keep the slash form; only writes use
// this.
func skillFilePath(p string) string {
	return filepath.FromSlash(expandHomePath(p))
}

// harnessHomeRoot is a coding agent's configuration home: the variable that
// relocates it when the user has set one, and the documented default otherwise.
//
// A relative or whitespace-only value is ignored rather than honoured. The
// result is joined with "skills/<name>/SKILL.md" and written, so a relative root
// would plant an agent-instruction file under whatever directory the command
// happened to run in -- silently, and somewhere no agent reads.
func harnessHomeRoot(envVar, fallback string) string {
	root := filepath.ToSlash(strings.TrimSpace(os.Getenv(envVar)))
	if root == "" {
		return fallback
	}
	if root == "~" || strings.HasPrefix(root, "~/") || path.IsAbs(root) {
		return root
	}
	return fallback
}

func sha256Sum(data []byte) []byte {
	sum := sha256.Sum256(data)
	return sum[:]
}
