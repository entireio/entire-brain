package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// This file is the third part of the outbound HTTP policy: how much of a SUCCESS
// body a hosted-API client is allowed to pull into memory.
//
// errbody.go already bounds the error path, and internal/cli's publish bounds its
// own 200 with an explicit io.LimitReader. Everything else read a hosted 200 with
//
//	json.NewDecoder(resp.Body).Decode(&out)
//
// which has no ceiling at all. json.Decoder streams, but the VALUES it decodes are
// allocated in full: the fact-set head's `data` field is a base64 []byte, so a
// response claiming a multi-gigabyte fact-set is a multi-gigabyte allocation on the
// member's machine before a single record is parsed. Measured against a mock that
// streams a well-formed `{"found":true,...,"data":"<8 GiB of base64>"}`, `facts
// sync` peaked at 9.8 GB RSS and the proposal list at 10 GB — an OOM on any laptop,
// and the workspace daemon is long-lived, so it takes the machine down with it.
// The request timeout cannot help: it bounds TIME, and a LAN peer can deliver tens
// of gigabytes inside a five-minute budget.
//
// The ceiling is deliberately far above any real payload (see MaxJSONBodyBytes) and
// a body that exceeds it is an ERROR, never a truncation. Truncating would be worse
// than the OOM it prevents: a fact-set head cut short at a record boundary parses
// cleanly as a SHORTER fact set, and the very next CAS would push that shortened
// set back as the new head — silently deleting every other member's facts.

// MaxJSONBodyBytes bounds a hosted-API JSON success body.
//
// 64 MiB is roughly three orders of magnitude above a real payload — the largest
// fact-set in this repo's own brain is ~150 KB for 244 records, and factmerge caps a
// single NDJSON line at 16 MiB — while being small enough that the worst case is an
// allocation a laptop shrugs off rather than one that ends the process.
//
// It is a var, not a const, so tests can shrink it.
var MaxJSONBodyBytes int64 = 64 << 20

// ErrBodyTooLarge reports a hosted-API response body over MaxJSONBodyBytes. It is a
// sentinel so a caller can tell "the peer sent something absurd" from "the peer sent
// something malformed"; both are refusals, but only one indicates a hostile or badly
// broken server rather than a version skew.
var ErrBodyTooLarge = errors.New("httpx: response body exceeds the hosted-API size limit")

// DecodeJSONBody decodes a hosted-API JSON response body into dst under
// MaxJSONBodyBytes, returning ErrBodyTooLarge when the peer sends more.
//
// It reads one byte past the limit precisely so "exactly at the limit" and "over the
// limit" are distinguishable: without the extra byte a body that filled the ceiling
// exactly would be indistinguishable from a truncated one.
//
// It is for 2xx bodies. Non-2xx bodies go through ErrorDetail/ErrorSuffix, which
// apply their own, much tighter bound.
func DecodeJSONBody(resp *http.Response, dst any) error {
	if resp == nil || resp.Body == nil {
		return errors.New("httpx: no response body to decode")
	}
	limit := MaxJSONBodyBytes
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return err
	}
	if int64(len(raw)) > limit {
		return fmt.Errorf("%w (%d bytes)", ErrBodyTooLarge, limit)
	}
	return json.Unmarshal(raw, dst)
}
