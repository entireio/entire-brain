package cli

// Compact v1 transcripts (Entire Transcript Format, the compact
// transcript.jsonl the default export reads): every agent's turns become
// {"v":1,"type":"user"|"assistant","content":[...]} with the content blocks at
// the record's top level, user text blocks without a "type", the turn time in
// "ts", and tool output inlined as tool_use.result. One fixture per agent
// whose converter writes the format (claude-code, codex, pi), for the
// tool-op and recall-evidence readers. They are built from the format spec,
// not captured from live agent sessions.

const compactOpsClaudeTranscript = `{"v":1,"agent":"claude-code","cli_version":"0.12.2","type":"user","ts":"2026-10-01T10:00:00Z","content":[{"text":"<system-reminder>wrapper that must not open an exchange</system-reminder>"}]}
{"v":1,"agent":"claude-code","cli_version":"0.12.2","type":"user","ts":"2026-10-01T10:00:01Z","content":[{"id":"u1","text":"Fix   the flaky lock test\non Windows"}]}
{"v":1,"agent":"claude-code","cli_version":"0.12.2","type":"assistant","ts":"2026-10-01T10:00:05Z","id":"msg_1","model":"claude-sonnet-4","input_tokens":10,"output_tokens":20,"content":[{"type":"text","text":"Looking at the lock timeout now."},{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"go test ./..."},"result":{"output":"state.transcriptPath = \"sessions/main\"\nFAIL TestLock SQLITE_BUSY raw tool output must never leak","status":"error"}}]}
{"v":1,"agent":"claude-code","cli_version":"0.12.2","type":"assistant","ts":"2026-10-01T10:00:09Z","id":"msg_2","content":[{"type":"text","text":"Root cause: the write lock timeout is too short on Windows; raising it."}]}
{"v":1,"agent":"claude-code","cli_version":"0.12.2","type":"user","ts":"2026-10-01T10:01:00Z","content":[{"text":"Now update the docs"}]}
{"v":1,"agent":"claude-code","cli_version":"0.12.2","type":"assistant","ts":"2026-10-01T10:01:05Z","id":"msg_3","content":"Docs updated."}
`

const compactOpsCodexTranscript = `{"v":1,"agent":"codex","cli_version":"0.12.2","type":"user","ts":"2026-10-01T10:00:00Z","content":[{"text":"Why does the export cursor skip sessions?"}]}
{"v":1,"agent":"codex","cli_version":"0.12.2","type":"assistant","ts":"2026-10-01T10:00:05Z","model":"gpt-5-codex","input_tokens":10,"output_tokens":20,"content":[{"type":"tool_use","id":"call_1","name":"exec_command","input":{"cmd":"grep -rn cursor internal/cli"},"result":{"output":"state.transcriptPath = \"sessions/main\"","status":"success"}},{"type":"text","text":"The cursor reuses unchanged transcripts; only new checkpoints are exported."}]}
{"v":1,"agent":"codex","cli_version":"0.12.2","type":"assistant","ts":"2026-10-01T10:00:06Z","input_tokens":5,"output_tokens":7,"content":[]}
{"v":1,"agent":"codex","cli_version":"0.12.2","type":"user","ts":"2026-10-01T10:01:00Z","content":[{"text":"Now patch it"}]}
{"v":1,"agent":"codex","cli_version":"0.12.2","type":"assistant","ts":"2026-10-01T10:01:05Z","content":[{"type":"tool_use","id":"call_2","name":"apply_patch","input":{"input":"*** Begin Patch\n*** Update File: internal/cli/export.go\n*** End Patch"},"result":{"output":"Success. Updated the following files:\nM internal/cli/export.go","status":"success"}},{"type":"text","text":"Patched the export cursor."}]}
`

const compactOpsPiTranscript = `{"v":1,"agent":"pi","cli_version":"0.12.2","type":"user","ts":"2026-10-01T10:00:00Z","content":[{"type":"image","data":"entire-asset:assets/screenshot.png","mimeType":"image/png"},{"text":"Summarize the retry policy"}]}
{"v":1,"agent":"pi","cli_version":"0.12.2","type":"assistant","ts":"2026-10-01T10:00:05Z","model":"claude-sonnet-4","content":[{"type":"tool_use","id":"tc_1","name":"Read","input":{"path":"retry.go"},"result":{"output":"state.transcriptPath = \"sessions/main\"\nraw tool result must never leak","status":"success"}},{"type":"text","text":"Retries use exponential backoff with a 5-attempt cap."}]}
`
