package agentsetup

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const activationPrefix = "<!-- entire-agent-activation: "
const activationSuffix = " -->"

type activationRecord struct {
	SchemaVersion int      `json:"schema_version"`
	Enabled       []string `json:"enabled"`
}

func renderActivation(active map[string]bool) string {
	guide := GraphGuide
	enabled := []string{"graph"}
	if active["brain"] {
		guide, enabled = BrainGuide(), []string{"brain"}
		if active["graph"] {
			guide, enabled = CombinedGuide, []string{"graph", "brain"}
		}
	}
	record, _ := json.Marshal(activationRecord{SchemaVersion: 1, Enabled: enabled})
	return guide + "\n" + activationPrefix + string(record) + activationSuffix + "\n"
}

func parseActivation(content string) (map[string]bool, bool, error) {
	const marker = "<!-- entire-agent-activation:"
	if !strings.Contains(content, marker) {
		return nil, false, nil
	}
	invalid := func() (map[string]bool, bool, error) {
		return nil, true, fmt.Errorf("invalid or unsupported agent activation metadata; repair before regenerating")
	}
	if strings.Count(content, marker) != 1 {
		return invalid()
	}
	var payload string
	for _, line := range strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n") {
		if strings.HasPrefix(line, activationPrefix) && strings.HasSuffix(line, activationSuffix) {
			payload = strings.TrimSuffix(strings.TrimPrefix(line, activationPrefix), activationSuffix)
		}
	}
	if payload == "" {
		return invalid()
	}
	decoder := json.NewDecoder(strings.NewReader(payload))
	decoder.DisallowUnknownFields()
	var record activationRecord
	if err := decoder.Decode(&record); err != nil {
		return invalid()
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return invalid()
	}
	if record.SchemaVersion != 1 || len(record.Enabled) == 0 {
		return invalid()
	}
	active := map[string]bool{}
	for _, product := range record.Enabled {
		if (product != "graph" && product != "brain") || active[product] {
			return invalid()
		}
		active[product] = true
	}
	return active, true, nil
}

// Metadata is authoritative once present. Before migration, preserve products
// represented by recognized generated guides and legacy managed blocks. A
// historical combined guide cannot reveal whether both activations were explicit.
func readActivation(repo string) (map[string]bool, error) {
	root, err := os.OpenRoot(repo)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	read := func(name string) (string, bool, error) {
		info, err := inspectInstructionFile(root, name, filepath.Join(repo, name))
		if err != nil {
			return "", false, err
		}
		if info == nil {
			return "", false, nil
		}
		data, err := readContainedFile(root, name, maxManagedLandingBytes)
		return strings.ReplaceAll(string(data), "\r\n", "\n"), true, err
	}
	content, present, err := read(Path)
	if err != nil {
		return nil, err
	}
	if present {
		active, found, err := parseActivation(content)
		if err != nil || found {
			return active, err
		}
	}
	active := map[string]bool{}
	recognize := func(text string) bool {
		heading, _, _ := strings.Cut(text, "\n")
		switch heading {
		case "# Entire repository agent guide — Graph and Brain":
			active["graph"], active["brain"] = true, true
		case "# Entire repository agent guide — Graph", "# entire-graph — instructions for coding agents (follow directly)":
			active["graph"] = true
		case "# Entire repository agent guide — Brain", "# Entire Brain coding-agent guide":
			active["brain"] = true
		default:
			return false
		}
		return true
	}
	if present && !recognize(content) {
		return nil, fmt.Errorf("unrecognized agent guide %s; preserve or migrate its content before regenerating", Path)
	}
	for _, name := range []string{".entire/graph-agent.md", ".entire/brain-agent.md", "AGENTS.md", "CLAUDE.md"} {
		text, exists, err := read(name)
		if err != nil {
			return nil, err
		}
		if !exists {
			continue
		}
		if name == "AGENTS.md" || name == "CLAUDE.md" {
			if _, err := migrateMarkers([]byte(text)); err != nil {
				return nil, err
			}
			for _, product := range []string{"graph", "brain"} {
				if strings.Contains(text, "<!-- entire-"+product+":begin -->") {
					active[product] = true
				}
			}
		} else if text != legacyRedirect && !recognize(text) {
			return nil, fmt.Errorf("unrecognized legacy agent guide %s; preserve or migrate its content before regenerating", name)
		}
	}
	return active, nil
}
