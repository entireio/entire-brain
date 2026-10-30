package base

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"time"
)

// HostEntireBinary is the host CLI the plugin shells out to; PATH comes from
// the caller's environment (the daemon plist bakes PATH at install time).
const HostEntireBinary = "entire"

// HostedShellOutTimeout bounds every shell-out to the host CLI on the hosted
// sync path (auth token, repo view). The callers are the watch tick and the
// session-end hook — a tick must stay a tick, so a stalled host CLI fails
// fast instead of hanging the caller; the ambient ctx carries only signal
// cancellation, no deadline.
const HostedShellOutTimeout = 30 * time.Second

// DecodeHostCLIJSON decodes the FIRST JSON value on the host CLI's stdout and
// ignores whatever follows it.
//
// The host `entire` CLI appends human notices to the SAME stream its --json
// output goes to. Released versions before the update notice moved to stderr
// print "\nUpdate available! x -> y\nRelease notes: ..." from a post-run hook,
// so `entire checkpoint explain --json` emits a complete JSON value and THEN a
// banner. A strict json.Unmarshal over the whole buffer fails that with
// "invalid character 'U' after top-level value" -- and only on a first-ever
// run, because the 24h version-check cache suppresses the banner on the next
// one. That made a deterministic first-run failure look like flakiness: setup
// reported "session export failed" once and then silently healed.
//
// Tolerating trailing bytes is the durable fix: the JSON value is the contract,
// anything after it is chatter this process does not own. Leading garbage stays
// an error, because that would mean the value is not first and no prefix of the
// stream can be trusted.
func DecodeHostCLIJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(target); err != nil {
		if errors.Is(err, io.EOF) {
			// json.Unmarshal's wording for an empty buffer, preserved so callers
			// and their tests keep reading one message for one condition.
			return errors.New("unexpected end of JSON input")
		}
		return err
	}
	return nil
}
