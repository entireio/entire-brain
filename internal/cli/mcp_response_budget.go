package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// MCP results drop whole tail rows to fit the exact serialized budget.
// Truncation markers participate in every size check; unreducible envelopes fail.
const (
	// mcpToolResponseDefaultMaxBytes bounds a tool result to what an AGENT can
	// accept, which is far below the transport frame limit.
	//
	// This was 128 KiB, chosen as "below the transport frame limit" (4 MiB).
	// That is the wrong consumer. The budget is spent in an agent's context
	// window, and JSON of this shape runs about 2.5 characters per token --
	// ids, quotes, braces and punctuation are not free -- so 128 KiB is roughly
	// 52k tokens, double a typical client's whole tool-result ceiling.
	//
	// Measured from a real session: brain_status returned 124,295 characters,
	// brain_brief 125,822 and brain_impact 61,580. Every one of them was UNDER
	// the 128 KiB budget, so the trimming here worked exactly as designed, and
	// every one was then REJECTED by the client as exceeding its token limit.
	// brain_impact is the tight bound: 61,580 characters is already ~24.6k
	// tokens at 2.5 chars/token, so the client's ceiling sits near 25k.
	//
	// 32 KiB is ~13k tokens: generous for a single tool result, and half the
	// smallest result that was actually refused. Trimming still returns rows
	// plus a marker, which is strictly better than an error carrying nothing.
	mcpToolResponseDefaultMaxBytes = 32 * 1024

	// mcpToolResponseMaxBytesEnv raises or lowers the budget for a client whose
	// window differs. The default is chosen for the smallest limit observed,
	// not the largest, because being under costs rows while being over costs
	// the whole result.
	mcpToolResponseMaxBytesEnv = "ENTIRE_BRAIN_MCP_MAX_BYTES"

	// mcpResponseTruncatedKey / mcpResponseTruncationKey are the envelope
	// markers. The boolean matches the retrieval surface's existing
	// response_truncated field so a client already handling one handles both.
	mcpResponseTruncatedKey  = "response_truncated"
	mcpResponseTruncationKey = "response_truncation"

	// mcpResponseTrimMaxDepth bounds how deep a row list may be nested before
	// it stops being treated as a row list. Six covers every current payload
	// (brain_impact's impact.relations, brain_brief's status.semantic.freshness.warnings)
	// without walking into unbounded nesting.
	mcpResponseTrimMaxDepth = 6

	// mcpResponseTrimMaxProbes bounds the total number of measurements. A
	// bisection over one list costs about log2(rows) probes, so this covers
	// several lists of any size a tool can produce; the cap exists so a
	// pathological document fails loudly instead of spinning.
	mcpResponseTrimMaxProbes = 128

	// Reserve this many rows per list before sharing remaining space proportionally.
	// If the floor does not fit, use one shared width across all lists.
	// mcpResponseTrimFairRowsAt128K is the per-list guarantee that was tuned
	// against the original 128 KiB budget. It is not a free-standing number: it
	// is a number of rows that fitted in THAT budget, so it has to move with the
	// budget. Left fixed at 64 under a 32 KiB budget, phase 1 can never satisfy
	// two lists (128 rows of space in a quarter of the room), so every list
	// silently drops to the same shared width and the wider list is cut to the
	// narrower one's size.
	mcpResponseTrimFairRowsAt128K = 64
	mcpResponseTrimFairRowsBudget = 128 * 1024
	mcpResponseTrimFairRowsFloor  = 8

	// Truncated array roots move under this key so markers stay outside data rows.
	// Untruncated roots keep their original shape.
	mcpResponseTrimArrayKey = "items"
)

// mcpResponseBudgetNote is appended to every size-bounding integer argument's
// description, so the half of the contract that is not expressible as a JSON
// Schema bound is still on the schema a client plans against.
const mcpResponseBudgetNote = " (removable rows may be trimmed with response_truncated; otherwise errors)"

// mcpServerInstructions states the surface-wide half of the contract once, in
// the place MCP defines for it, rather than paying for it in every tool's
// schema. The per-argument note above is the short form a client reads while
// planning a call at the ceiling; this is the full one.
var mcpServerInstructions = fmt.Sprintf("Tool results are bounded to %d bytes. Oversize JSON results with removable row lists "+
	"drop whole tail rows and set %q: true; each shortened list reports {path, returned, total} under %q. "+
	"A result that cannot be reduced to the budget returns an error asking you to narrow the request. "+
	"Limit and depth arguments bound requested work, not serialized bytes. The total is the number of rows "+
	"produced by this call before trimming, already capped by its limit; it is not the corpus count. "+
	"Raising the limit may retrieve more candidates but does not establish the total available. "+
	"Each list keeps its first %d rows when space allows; remaining space is shared proportionally. "+
	"If that floor does not fit, lists use a shared width. Matching pagination counts are updated to returned rows. "+
	"Truncated array roots are wrapped in {%q: [...]} so markers remain at the document root.",
	mcpToolResponseMaxBytes, mcpResponseTruncatedKey, mcpResponseTruncationKey,
	mcpResponseTrimFairRows(), mcpResponseTrimArrayKey)

// mcpRowCountKeys are sibling fields that state how many rows a list carries.
// Trimming the list without repairing them would replace the old dishonesty
// (a ceiling nothing satisfies) with a new one (a count that disagrees with the
// rows beside it). Only a field whose value still equals the pre-trim row count
// is touched, so a field that counts something else is left alone.
var mcpRowCountKeys = map[string]bool{"count": true, "returned": true, "result_count": true, "returned_count": true}

// mcpRowList is one trimmable array inside a tool's JSON document, together
// with the write-back that puts a prefix of it where it came from. The full
// slice is retained so the search can walk back up: a probe that kept too few
// rows has to be undone, not lived with.
type mcpRowList struct {
	path   string
	all    []any
	kept   int
	write  func([]any)
	repair func(int)
}

func (l *mcpRowList) total() int { return len(l.all) }

func (l *mcpRowList) keep(n int) {
	if n < 0 {
		n = 0
	}
	if n > len(l.all) {
		n = len(l.all)
	}
	l.kept = n
	l.write(l.all[:n])
	if l.repair != nil {
		l.repair(n)
	}
}

// mcpToolFrameSize measures the bytes the peer actually receives for this text:
// the JSON-RPC envelope plus the escaped content item plus the frame header.
// Measuring the text alone undercounts quotes, backslashes and newlines.
func mcpToolFrameSize(ctx context.Context, text string) (int, bool) {
	result := map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}
	size, err := mcpToolResultTransportSize(ctx, result)
	if err != nil {
		return 0, false
	}
	return size, true
}

// mcpBoundedToolText is the whole outbound contract in one place: return the
// text unchanged when it fits, a row-truncated document carrying an explicit
// marker when it can be reduced, and an actionable refusal when it cannot.
func mcpBoundedToolText(ctx context.Context, tool, text string) (string, error) {
	size := len(text)
	// A frame is never smaller than the text it carries, so a text already over
	// budget is over budget without measuring -- and measuring means escaping a
	// document that can be tens of megabytes. Only a text that might still fit
	// is measured exactly, because escaping can push it over on its own.
	if size <= mcpToolResponseMaxBytes {
		measured, ok := mcpToolFrameSize(ctx, text)
		if !ok {
			// Measuring failed, not the result. Returning the result unmeasured
			// is the pre-existing behaviour and strictly better than failing a
			// call that may well be fine.
			return text, nil
		}
		if measured <= mcpToolResponseMaxBytes {
			return text, nil
		}
		size = measured
	}
	if trimmed, ok := mcpTrimToolResponseRows(ctx, text, size); ok {
		return trimmed, nil
	}
	return "", fmt.Errorf(
		"%s produced a %d byte result, over the %d byte MCP response budget (the %d byte MCP frame limit still applies); it could not be reduced by dropping result rows, so narrow the request (lower limit, or drop details)",
		tool, size, mcpToolResponseMaxBytes, maxMCPFrameBytes)
}

// mcpTrimToolResponseRows drops complete tail rows and measures the exact frame.
// It first reserves mcpResponseTrimFairRows() per list, then shares space in
// proportion to original list lengths. If the floor cannot fit, lists share
// one width. No row contents are rewritten. Non-JSON results, documents with
// no row lists, and envelopes that remain oversized return false.
func mcpTrimToolResponseRows(ctx context.Context, text string, size int) (string, bool) {
	doc, ok := mcpDecodeJSONDocument(text)
	if !ok {
		return "", false
	}
	lists := mcpRowListsInDocument(&doc)
	if len(lists) == 0 {
		return "", false
	}
	marker := mcpTruncationMarker{doc: &doc, lists: lists}
	widest := 0
	for _, list := range lists {
		if list.total() > widest {
			widest = list.total()
		}
	}

	probes := 0
	// probe applies a row allocation and measures the result, with the marker
	// applied. The marker is therefore inside every measurement and can never be
	// the thing that pushes a document back over budget.
	probe := func(rows func(*mcpRowList) int) (encoded string, fits, measured bool) {
		for _, list := range lists {
			list.keep(rows(list))
		}
		data, err := jsonOutputBytes(marker.document())
		if err != nil {
			return "", false, false
		}
		frame, ok := mcpToolFrameSize(ctx, string(data))
		if !ok {
			return "", false, false
		}
		probes++
		return string(data), frame <= mcpToolResponseMaxBytes, true
	}

	// uniform is the shared width: every list gets the same number of rows.
	uniform := func(width int) func(*mcpRowList) int {
		return func(list *mcpRowList) int { return min(width, list.total()) }
	}
	// share is the proportional allocation, parameterised by the rows the WIDEST
	// list keeps so the search below can bisect a single integer. Every other
	// list keeps the same fraction of its own rows, floored at the fair share.
	share := func(widestRows int) func(*mcpRowList) int {
		return func(list *mcpRowList) int {
			// int64 throughout: widestRows * total overflows a 32-bit int.
			rows := int((int64(widestRows)*int64(list.total()) + int64(widest) - 1) / int64(widest))
			if rows < mcpResponseTrimFairRows() {
				rows = mcpResponseTrimFairRows()
			}
			return min(rows, list.total())
		}
	}

	// Phase 1: can every list keep its fair share? share(mcpResponseTrimFairRows())
	// is exactly uniform(mcpResponseTrimFairRows()), so this doubles as the low end
	// of the phase-2 search.
	encoded, fits, measured := probe(uniform(mcpResponseTrimFairRows()))
	if !measured {
		return "", false
	}
	if !fits {
		return mcpTrimSharedWidth(&marker, probe, uniform, mcpResponseTrimFairRows(), &probes)
	}
	best := encoded

	// Phase 2: bisect the widest list's row count. Document size is monotone in
	// it, so lo is the largest count known to fit and hi the smallest known not
	// to. widest is the document exactly as the tool produced it, which is how we
	// got here, so hi starts out known not to fit.
	lo, hi := mcpResponseTrimFairRows(), widest
	// Seed the search from how far over budget the document is, so a 33 MB answer
	// starts near its allocation instead of halving down to it.
	if seed := int(int64(widest) * int64(mcpToolResponseMaxBytes) / int64(size)); seed > lo && seed < hi {
		encoded, fits, measured := probe(share(seed))
		if !measured {
			return "", false
		}
		if fits {
			lo, best = seed, encoded
		} else {
			hi = seed
		}
	}
	for hi-lo > 1 && probes < mcpResponseTrimMaxProbes {
		mid := lo + (hi-lo)/2
		encoded, fits, measured := probe(share(mid))
		if !measured {
			return "", false
		}
		if fits {
			lo, best = mid, encoded
		} else {
			hi = mid
		}
	}

	// Phase 3: the proportional step is coarse — one more row for the widest list
	// is several more rows across the document — so it can stop well short of the
	// budget. brain_brief landed at 118,246 bytes of a 131,072 byte budget at the
	// advertised ceiling while a SMALLER limit returned 120,652, which is the
	// same "more returns less" in bytes. Spend what is left on the widest list
	// alone, where a row is cheapest per unit of answer.
	widestRows := lo
	allocation := share(lo)
	top := func(rows int) func(*mcpRowList) int {
		return func(list *mcpRowList) int {
			if list.total() == widest {
				return min(rows, list.total())
			}
			return allocation(list)
		}
	}
	for lo, hi = widestRows, widest; hi-lo > 1 && probes < mcpResponseTrimMaxProbes; {
		mid := lo + (hi-lo)/2
		encoded, fits, measured := probe(top(mid))
		if !measured {
			return "", false
		}
		if fits {
			lo, best = mid, encoded
		} else {
			hi = mid
		}
	}
	return best, true
}

// mcpTrimSharedWidth is the fallback for a document too wide to give every list
// its fair share: hold every list to one shared width, the largest that fits.
// This is what keeps a thirteen-list brief and an impact answer at the
// advertised ceiling from returning a section with no rows in it.
func mcpTrimSharedWidth(
	marker *mcpTruncationMarker,
	probe func(func(*mcpRowList) int) (string, bool, bool),
	uniform func(int) func(*mcpRowList) int,
	ceiling int,
	probes *int,
) (string, bool) {
	lo, hi := 0, ceiling
	best, haveBest := "", false
	for hi-lo > 1 && *probes < mcpResponseTrimMaxProbes {
		mid := lo + (hi-lo)/2
		encoded, fits, measured := probe(uniform(mid))
		if !measured {
			return "", false
		}
		if fits {
			lo, best, haveBest = mid, encoded, true
		} else {
			hi = mid
		}
	}
	if haveBest {
		return best, true
	}
	// Not one row fits beside the envelope. An empty, flagged list is still an
	// answer; an envelope that does not fit on its own is not.
	encoded, fits, measured := probe(uniform(0))
	if !measured || !fits {
		return "", false
	}
	return encoded, true
}

// mcpDecodeJSONDocument decodes with UseNumber so ids and counts round-trip
// exactly; decoding into float64 would rewrite a large integer id in
// exponential form on the way back out.
func mcpDecodeJSONDocument(text string) (any, bool) {
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	var doc any
	if err := decoder.Decode(&doc); err != nil {
		return nil, false
	}
	// A document with trailing content is not the single JSON value these tools
	// emit; leave it to the refusal path rather than silently reshaping it.
	if decoder.More() {
		return nil, false
	}
	return doc, true
}

// mcpRowListsInDocument finds every trimmable array. Arrays reached through
// object fields are row lists; arrays *inside* a row are part of that row and
// are left whole, so a returned row is always the row the tool produced.
func mcpRowListsInDocument(doc *any) []*mcpRowList {
	var out []*mcpRowList
	switch typed := (*doc).(type) {
	case []any:
		if len(typed) > 0 {
			out = append(out, &mcpRowList{path: mcpResponseTrimArrayKey, all: typed, kept: len(typed), write: func(rows []any) { *doc = rows }})
		}
	case map[string]any:
		mcpCollectRowLists(typed, "", 1, nil, &out)
	}
	return out
}

func mcpCollectRowLists(node map[string]any, path string, depth int, ancestors []map[string]any, out *[]*mcpRowList) {
	if depth > mcpResponseTrimMaxDepth {
		return
	}
	keys := make([]string, 0, len(node))
	for key := range node {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		childPath := key
		if path != "" {
			childPath = path + "." + key
		}
		switch child := node[key].(type) {
		case []any:
			if len(child) == 0 {
				continue
			}
			owner, name, before := node, key, len(child)
			*out = append(*out, &mcpRowList{
				path:   childPath,
				all:    child,
				kept:   before,
				write:  func(rows []any) { owner[name] = rows },
				repair: mcpRowCountRepair(owner, ancestors, before),
			})
		case map[string]any:
			mcpCollectRowLists(child, childPath, depth+1, append(ancestors, node), out)
		}
	}
}

// mcpRowCountRepair binds matching row-count fields in the owner, its ancestors,
// and their pagination objects before trimming. Each probe updates those same
// fields; unrelated counts stay unchanged. Equal-length lists share allocations,
// so they can safely bind the same counter.
func mcpRowCountRepair(owner map[string]any, ancestors []map[string]any, before int) func(int) {
	var fields []map[string]any
	var keys []string
	collect := func(scope map[string]any) {
		if scope == nil {
			return
		}
		for key, value := range scope {
			if !mcpRowCountKeys[key] {
				continue
			}
			if n, ok := mcpJSONInt(value); ok && n == before {
				fields = append(fields, scope)
				keys = append(keys, key)
			}
		}
	}
	scope := func(node map[string]any) {
		collect(node)
		if pagination, ok := node["pagination"].(map[string]any); ok {
			collect(pagination)
		}
	}
	scope(owner)
	for i := len(ancestors) - 1; i >= 0; i-- {
		scope(ancestors[i])
	}
	if len(fields) == 0 {
		return nil
	}
	return func(keep int) {
		for i, scope := range fields {
			scope[keys[i]] = json.Number(strconv.Itoa(keep))
		}
	}
}

func mcpJSONInt(value any) (int, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	n, err := number.Int64()
	if err != nil {
		return 0, false
	}
	return int(n), true
}

// mcpTruncationMarker stamps the document with what was dropped. It is rebuilt
// on every round so the marker is inside every size measurement: the marker can
// never be the thing that pushes the document back over budget.
type mcpTruncationMarker struct {
	doc   *any
	lists []*mcpRowList
}

func (m *mcpTruncationMarker) truncated() bool {
	for _, list := range m.lists {
		if list.kept != list.total() {
			return true
		}
	}
	return false
}

func (m *mcpTruncationMarker) report() map[string]any {
	dropped := make([]any, 0, len(m.lists))
	for _, list := range m.lists {
		if list.kept == list.total() {
			continue
		}
		dropped = append(dropped, map[string]any{
			"path":     list.path,
			"returned": list.kept,
			"total":    list.total(),
		})
	}
	return map[string]any{
		"budget_bytes": mcpToolResponseMaxBytes,
		"dropped_from": dropped,
		"note":         "tail rows were dropped to fit the MCP response budget; lower limit (or drop details) to choose which rows you receive",
	}
}

// document returns the value to encode for the current allocation: the document
// itself when nothing was dropped, and otherwise the document carrying the
// marker.
//
// An OBJECT root takes the marker in its own envelope. An ARRAY root is wrapped
// in an object instead of having the marker stamped onto its last surviving row,
// because a marker inside a row is not a marker: it corrupts one data record,
// hides itself from a client reading the document root, and breaks a typed
// decoder. Wrapping also means an array trimmed to zero rows is still a
// well-formed, flagged answer, where before it had nowhere to put the marker and
// the whole call had to be refused.
func (m *mcpTruncationMarker) document() any {
	root, isObject := (*m.doc).(map[string]any)
	if !m.truncated() {
		if isObject {
			// A previous probe may have stamped this root; an untruncated
			// document must not carry a marker left over from one that was.
			delete(root, mcpResponseTruncatedKey)
			delete(root, mcpResponseTruncationKey)
		}
		return *m.doc
	}
	report := m.report()
	if isObject {
		root[mcpResponseTruncatedKey] = true
		root[mcpResponseTruncationKey] = report
		return root
	}
	return map[string]any{
		mcpResponseTrimArrayKey:  *m.doc,
		mcpResponseTruncatedKey:  true,
		mcpResponseTruncationKey: report,
	}
}

// mcpToolResponseMaxBytes is the active budget: the default unless
// ENTIRE_BRAIN_MCP_MAX_BYTES names a positive byte count. Resolved once, so a
// single server process cannot change budget between two calls and hand a
// client two different contracts after advertising one in its instructions.
var mcpToolResponseMaxBytes = resolveMCPToolResponseMaxBytes(os.Getenv(mcpToolResponseMaxBytesEnv))

// resolveMCPToolResponseMaxBytes keeps the parse testable without touching the
// process environment. An unset, unparseable or non-positive value takes the
// default rather than failing: a bad override must not take the server down,
// and silently running unbounded would be worse than ignoring it.
func resolveMCPToolResponseMaxBytes(raw string) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return mcpToolResponseDefaultMaxBytes
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return mcpToolResponseDefaultMaxBytes
	}
	// Never exceed the transport frame: a budget above it promises a result
	// the transport would refuse to carry.
	if n > maxMCPFrameBytes {
		return maxMCPFrameBytes
	}
	return n
}

// mcpResponseTrimFairRows() scales the per-list row guarantee with the active
// budget, holding the original calibration exactly at the budget it was tuned
// for (64 rows at 128 KiB) and degrading linearly below it.
//
// A floor keeps the guarantee meaningful: below a handful of rows per list the
// phase-1 reservation stops expressing "nobody gets starved" and just fails,
// which is the degenerate case this scaling exists to avoid.
func mcpResponseTrimFairRows() int {
	rows := mcpToolResponseMaxBytes * mcpResponseTrimFairRowsAt128K / mcpResponseTrimFairRowsBudget
	if rows < mcpResponseTrimFairRowsFloor {
		return mcpResponseTrimFairRowsFloor
	}
	if rows > mcpResponseTrimFairRowsAt128K {
		return mcpResponseTrimFairRowsAt128K
	}
	return rows
}
