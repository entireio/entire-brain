package cli

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Episode tool/command extraction (Pattern Consolidation, Phase 2).
//
// From an episode's raw work segment, recover the ordered tool-call names and the
// ordered normalized shell commands. These are the operational shape procedure
// detection groups on. Deterministic and token-free; handles the Codex
// (response_item), Claude/pi (assistant content blocks) dialects. Document-form
// (opencode) drops tool output, so sequences are empty there — acceptable, those
// transcripts carry no tool calls in the parsed conversation.

// shellTools are tool names whose input carries a shell command we normalize into
// the command sequence. Other tools (apply_patch, read, write, …) contribute to
// the tool sequence only.
var shellTools = map[string]bool{
	"exec_command": true, "bash": true, "shell": true,
	"run_command": true, "local_shell": true, "run_terminal_cmd": true,
}

// multiVerbPrograms take a meaningful subcommand (git -> "git commit", go ->
// "go test"); for everything else the program name alone is the normalized form.
var multiVerbPrograms = map[string]bool{
	"git": true, "go": true, "npm": true, "pnpm": true, "yarn": true,
	"cargo": true, "docker": true, "kubectl": true, "gh": true, "make": true,
	"mise": true, "terraform": true, "helm": true, "pip": true, "pip3": true,
	"python": true, "python3": true, "node": true, "brew": true,
}

var envAssignmentPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

type episodeToolCall struct {
	name    string
	command string // raw shell command for shell tools; "" otherwise
}

// episodeToolAndCommandSequences returns the ordered tool names and ordered
// normalized shell commands performed in the work segment.
func episodeToolAndCommandSequences(workText string) (tools []string, commands []string) {
	for _, line := range strings.Split(workText, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var obj map[string]any
		if json.Unmarshal([]byte(line), &obj) != nil {
			continue
		}
		for _, call := range toolCallsFromObj(obj) {
			if call.name == "" {
				continue
			}
			tools = append(tools, call.name)
			if norm := normalizeCommand(call.command); norm != "" {
				commands = append(commands, norm)
			}
		}
	}
	return tools, commands
}

func toolCallsFromObj(obj map[string]any) []episodeToolCall {
	switch jsonString(obj["type"]) {
	case "response_item":
		p := jsonMap(obj["payload"])
		switch jsonString(p["type"]) {
		case "function_call":
			name := strings.ToLower(strings.TrimSpace(jsonString(p["name"])))
			return []episodeToolCall{{name: name, command: shellCommandFromInput(name, p["arguments"])}}
		case "custom_tool_call":
			return []episodeToolCall{{name: strings.ToLower(strings.TrimSpace(jsonString(p["name"])))}}
		}
	case "assistant":
		return toolCallsFromContent(jsonMap(obj["message"])["content"])
	case "message": // pi wraps every turn as {"type":"message","message":{role,content}}
		m := jsonMap(obj["message"])
		if jsonString(m["role"]) == "assistant" {
			return toolCallsFromContent(m["content"])
		}
	}
	return nil
}

func toolCallsFromContent(content any) []episodeToolCall {
	blocks, ok := content.([]any)
	if !ok {
		return nil
	}
	var out []episodeToolCall
	for _, b := range blocks {
		bm := jsonMap(b)
		t := jsonString(bm["type"])
		if t != "tool_use" && t != "tool" {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(firstNonEmptyString(bm["name"], bm["tool"])))
		out = append(out, episodeToolCall{name: name, command: shellCommandFromInput(name, bm["input"])})
	}
	return out
}

// shellCommandFromInput extracts the raw command string from a shell tool's input
// (a JSON object, or a JSON-encoded string for Codex arguments). cmd may be a
// string or an argv array.
func shellCommandFromInput(name string, raw any) string {
	if !shellTools[name] {
		return ""
	}
	m := asObject(raw)
	if m == nil {
		return ""
	}
	switch v := firstNonNil(m["cmd"], m["command"]).(type) {
	case string:
		return v
	case []any:
		parts := make([]string, 0, len(v))
		for _, x := range v {
			parts = append(parts, fmt.Sprint(x))
		}
		return strings.Join(parts, " ")
	}
	return ""
}

func asObject(raw any) map[string]any {
	switch t := raw.(type) {
	case map[string]any:
		return t
	case string:
		var m map[string]any
		if json.Unmarshal([]byte(t), &m) == nil {
			return m
		}
	}
	return nil
}

func firstNonNil(values ...any) any {
	for _, v := range values {
		if v != nil {
			return v
		}
	}
	return nil
}

// normalizeCommand reduces a shell command to a stable grouping key: the program
// (basenamed) plus, for multi-verb tools, its subcommand — e.g.
// "git rev-parse --abbrev-ref HEAD && …" -> "git rev-parse",
// "sed -n '1,5p' f.go" -> "sed". It cuts at the first shell operator so a
// chained command keys on its first program, and skips leading env assignments
// and wrappers (sudo/env/time). Returns "" for an empty command.
func normalizeCommand(cmd string) string {
	fields := strings.Fields(cmd)
	i := 0
	for i < len(fields) {
		f := fields[i]
		if f == "sudo" || f == "env" || f == "time" || f == "command" || f == "nohup" || envAssignmentPattern.MatchString(f) {
			i++
			continue
		}
		break
	}
	if i >= len(fields) || isShellOperator(fields[i]) {
		return ""
	}
	prog := strings.ToLower(commandBasename(fields[i]))
	if prog == "" {
		return ""
	}
	if multiVerbPrograms[prog] && i+1 < len(fields) {
		next := fields[i+1]
		if !isShellOperator(next) && !strings.HasPrefix(next, "-") {
			return prog + " " + strings.ToLower(next)
		}
	}
	return prog
}

func isShellOperator(token string) bool {
	switch token {
	case "&&", "||", "|", ";", "|&", "&":
		return true
	}
	return false
}

func commandBasename(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[i+1:]
	}
	return path
}
