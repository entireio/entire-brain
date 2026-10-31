package hook

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"unicode/utf8"
)

// claudeFailureEvent is the subset of Claude Code's PostToolUseFailure payload
// this hook needs. Every other field is ignored rather than rejected: the host
// owns the shape and adds to it, and a hook that failed on an unknown key would
// break the harness on the next release.
//
// The contract was CAPTURED from the running host, not inferred. The engine's
// own type declarations for this build declare PostToolUseFailureHookInput as
// carrying hook_event_name, tool_name, tool_input, tool_use_id, error,
// is_interrupt and duration_ms, and map the event to exactly ONE permitted
// output field, additionalContext. The installed binary (Claude Code 2.1.314)
// ships both the event name and is_interrupt, so the event exists and fires in
// the host this is wired into. The precedent for insisting on a capture is the
// pre-edit hook, whose first wiring read $CLAUDE_FILE_PATH -- a variable that
// does not exist -- and was installed and inert for eight days.
//
// PostToolUse would have been the WRONG event to wire, for two reasons:
//
//   - It fires after every tool call, so the brain would run on every read,
//     grep and edit only to discover that nothing failed. post-failure's whole
//     contract is that it is cheap enough to leave wired.
//   - It reports the outcome in tool_response, whose shape is per-tool and
//     declared `unknown`, so deciding "did this fail" would mean guessing at a
//     different shape per tool. PostToolUseFailure fires only on a failure and
//     hands the failure text over in one declared string.
type claudeFailureEvent struct {
	ToolName  string `json:"tool_name"`
	ToolInput struct {
		// Bash names the thing that failed; the file tools name a path.
		// Either one is the "command" half of the match query.
		Command  string `json:"command"`
		FilePath string `json:"file_path"`
	} `json:"tool_input"`
	Error string `json:"error"`
	// An interrupt is the user pressing escape. It is not a dead end, and the
	// brain has nothing to say about it.
	IsInterrupt bool `json:"is_interrupt"`
}

// ClaudeFailure is what post-failure needs out of one PostToolUseFailure event:
// what was being attempted, and what the host reported when it failed.
type ClaudeFailure struct {
	Command string
	Failure string
}

// DecodeClaudeFailureEvent reads one PostToolUseFailure event.
//
// ok is false for everything the hook must stay silent about -- a nil reader,
// no bytes, unparseable JSON, an interrupt, or an event carrying no failure
// text at all. That is the hook contract: a data problem is silence and exit 0,
// never an error, because this runs inside someone else's harness.
func DecodeClaudeFailureEvent(r io.Reader) (ClaudeFailure, bool) {
	if r == nil {
		return ClaudeFailure{}, false
	}
	data, err := io.ReadAll(io.LimitReader(r, maxClaudeHookEventBytes))
	if err != nil || len(bytes.TrimSpace(data)) == 0 {
		return ClaudeFailure{}, false
	}
	var event claudeFailureEvent
	if err := json.Unmarshal(data, &event); err != nil {
		return ClaudeFailure{}, false
	}
	// Checked before the error text, because an interrupt can carry one: the
	// host reports what the tool had emitted when the user stopped it, and
	// answering that with "known dead ends" is noise at the exact moment the
	// person has taken over.
	if event.IsInterrupt {
		return ClaudeFailure{}, false
	}
	failure := strings.TrimSpace(event.Error)
	if failure == "" {
		return ClaudeFailure{}, false
	}
	return ClaudeFailure{
		Command: failureEventCommand(event),
		Failure: failureTail(failure),
	}, true
}

// failureEventCommand names what was being attempted. Bash carries the command
// itself, the file tools carry a path, and anything else is identified by its
// tool name -- which is still worth matching on, because a memory can name a
// tool ("WebFetch is blocked on this host").
func failureEventCommand(event claudeFailureEvent) string {
	for _, candidate := range []string{event.ToolInput.Command, event.ToolInput.FilePath} {
		if value := strings.TrimSpace(candidate); value != "" {
			return value
		}
	}
	return strings.TrimSpace(event.ToolName)
}

// failureTail keeps at most the last hookFailureTailBytes of the failure text,
// matching what the piped-stdin path already does: errors cluster at the end of
// output, and an unbounded query dilutes term matching.
//
// The cut lands on a rune boundary. Slicing bytes would leave a partial rune at
// the head, which marshals into the agent's context as U+FFFD -- harmless but
// visible, and this text is read by a person debugging a hook.
func failureTail(failure string) string {
	if len(failure) <= hookFailureTailBytes {
		return failure
	}
	tail := failure[len(failure)-hookFailureTailBytes:]
	for len(tail) > 0 && !utf8.RuneStart(tail[0]) {
		tail = tail[1:]
	}
	return tail
}
