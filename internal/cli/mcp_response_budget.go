package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// The MCP tool surface used to advertise `"maximum": 10000` on every limit and
// depth argument so a schema-validating client could "plan valid calls", and
// then refuse the call it had just been told was legal. On a real repository
// (~48k symbols, ~155k relations) a call at that advertised ceiling hard-failed
// for ten of the seventeen limit-taking tools: brain_impact built 33 MB,
// brain_brief 7 MB after burning 34 seconds, brain_query_graph 8 MB — each one
// refused at the frame boundary with an error that named the tool and the limit
// but never the limit that *would* have worked, so recovery was a binary search.
//
// The refusal itself was not the defect. The contract was: a ceiling nothing
// could satisfy. This file makes the two agree by making the ceiling reachable
// — a call at the advertised maximum now returns a bounded answer that says, in
// the document, exactly which rows were dropped and how many there were.
//
// The idiom is the retrieval surface's (boundedRetrievalJSONPayload): drop
// whole tail rows, re-measure the exact bytes the peer will receive, and keep
// the response_truncated marker present during every size check so the marker
// can never be what pushes the document over budget.
const (
	// mcpToolResponseMaxBytes is the budget for a complete MCP tool result.
	//
	// Deliberately far below maxMCPFrameBytes. The frame limit is a transport
	// fact; it is not a sane amount of context to hand an agent. brain_dead_code
	// at the advertised limit returned 3,401,455 bytes — roughly 850k tokens —
	// as a *success*, unflagged. This is the same 128 KiB the multi-concept
	// retrieval surface already holds itself to (conversationConceptResponseMaxBytes),
	// so the two agent-facing surfaces answer to one number; every tool's
	// default-limit response measured well under it.
	mcpToolResponseMaxBytes = 128 * 1024

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
)

// mcpResponseBudgetNote is appended to every size-bounding integer argument's
// description, so the half of the contract that is not expressible as a JSON
// Schema bound is still on the schema a client plans against.
const mcpResponseBudgetNote = " (over-budget results drop tail rows and set response_truncated)"

// mcpServerInstructions states the surface-wide half of the contract once, in
// the place MCP defines for it, rather than paying for it in every tool's
// schema. The per-argument note above is the short form a client reads while
// planning a call at the ceiling; this is the full one.
var mcpServerInstructions = fmt.Sprintf("Tool results are bounded: a response over %d bytes "+
	"drops whole tail rows rather than failing, sets %q: true, and reports each shortened list "+
	"as {path, returned, total} under %q. A limit or depth argument therefore caps the work "+
	"requested, not the bytes returned; a call at the schema maximum always returns an answer, "+
	"and the answer says how much of it was dropped.",
	mcpToolResponseMaxBytes, mcpResponseTruncatedKey, mcpResponseTruncationKey)

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

// mcpTrimToolResponseRows drops whole tail rows until the exact frame fits.
//
// Row removal is the only edit: no row is rewritten, no field is summarized. A
// caller therefore reads real rows or none, never an abridged one. ok is false
// when the text is not a JSON document, when it holds no row list, or when even
// an empty one does not fit -- the caller refuses instead.
//
// Every list is held to ONE shared row cap rather than the widest list being
// filled first. Filling greedily starves the sections that matter: brain_impact
// at the advertised ceiling has 84,022 relations beside 10,000 symbols, and
// giving the relations list its maximum first left it with zero rows, because
// not one relation fit beside ten thousand symbols. A shared cap gives every
// section the same number of rows, so an impact answer carries both its
// relations and its symbols, and a brief carries all of its sections.
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
	if !marker.supported() {
		return "", false
	}
	widest := 0
	for _, list := range lists {
		if list.total() > widest {
			widest = list.total()
		}
	}

	probes := 0
	// probe applies a row cap and measures the result, with the marker applied.
	// The marker is therefore inside every measurement and can never be the
	// thing that pushes a document back over budget.
	probe := func(rowCap int) (encoded string, fits, measured bool) {
		for _, list := range lists {
			list.keep(min(rowCap, list.total()))
		}
		marker.apply()
		data, err := jsonOutputBytes(doc)
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

	// Document size is monotone in the cap, so bisect it: lo is the largest cap
	// known to fit, hi the smallest known not to. A cap of `widest` is the
	// document exactly as the tool produced it, which is how we got here, so hi
	// starts out known not to fit.
	lo, hi := 0, widest
	best, haveBest := "", false
	// Seed the search from how far over budget the document is, so a 33 MB
	// answer starts near its cap instead of halving down to it.
	// int64 throughout: widest * budget overflows a 32-bit int on a wide result.
	if seed := int(int64(widest) * int64(mcpToolResponseMaxBytes) / int64(size)); seed > 0 && seed < widest {
		encoded, fits, measured := probe(seed)
		if !measured {
			return "", false
		}
		if fits {
			lo, best, haveBest = seed, encoded, true
		} else {
			hi = seed
		}
	}
	for hi-lo > 1 && probes < mcpResponseTrimMaxProbes {
		mid := lo + (hi-lo)/2
		encoded, fits, measured := probe(mid)
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
	if _, isArray := doc.([]any); isArray {
		// An array root carries the marker on its last row. Emptied, it has
		// nowhere to carry it, and an unflagged empty array is exactly the
		// silent fragment this change exists to prevent.
		return "", false
	}
	// Not one row fits beside the envelope. An empty, flagged list is still an
	// answer; an envelope that does not fit on its own is not.
	encoded, fits, measured := probe(0)
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
			out = append(out, &mcpRowList{path: "", all: typed, kept: len(typed), write: func(rows []any) { *doc = rows }})
		}
	case map[string]any:
		mcpCollectRowLists(typed, "", 1, &out)
	}
	return out
}

func mcpCollectRowLists(node map[string]any, path string, depth int, out *[]*mcpRowList) {
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
				repair: mcpRowCountRepair(owner, before),
			})
		case map[string]any:
			mcpCollectRowLists(child, childPath, depth+1, out)
		}
	}
}

// mcpRowCountRepair binds the sibling fields that counted this list -- brain_code's
// pagination.count, for one -- so every probe rewrites them to the rows actually
// kept. The fields are identified once, against the untouched document, because
// after the first probe their value no longer matches the original row count and
// a field found by matching would silently stop being repaired.
func mcpRowCountRepair(owner map[string]any, before int) func(int) {
	var fields []map[string]any
	var keys []string
	collect := func(scope map[string]any) {
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
	collect(owner)
	if pagination, ok := owner["pagination"].(map[string]any); ok {
		collect(pagination)
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

// mcpTruncationMarker stamps the document with what was dropped. It is
// re-applied on every round so the marker is inside every size measurement:
// the marker can never be the thing that pushes the document back over budget.
type mcpTruncationMarker struct {
	doc    *any
	lists  []*mcpRowList
	marked map[string]any
}

// supported reports whether this document has somewhere honest to put the
// marker. An object root takes it in the envelope; an array root takes it on
// its last remaining row, the same place the retrieval surface puts its own row
// marker. A root that is neither is refused rather than silently shortened.
func (m *mcpTruncationMarker) supported() bool {
	switch root := (*m.doc).(type) {
	case map[string]any:
		return true
	case []any:
		if len(root) == 0 {
			return false
		}
		_, ok := root[len(root)-1].(map[string]any)
		return ok
	default:
		return false
	}
}

func (m *mcpTruncationMarker) report() map[string]any {
	dropped := make([]any, 0, len(m.lists))
	for _, list := range m.lists {
		if list.kept == list.total() {
			continue
		}
		path := list.path
		if path == "" {
			path = "."
		}
		dropped = append(dropped, map[string]any{
			"path":     path,
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

func (m *mcpTruncationMarker) apply() {
	if m.marked != nil {
		delete(m.marked, mcpResponseTruncatedKey)
		delete(m.marked, mcpResponseTruncationKey)
		m.marked = nil
	}
	target, ok := (*m.doc).(map[string]any)
	if !ok {
		root, isArray := (*m.doc).([]any)
		if !isArray || len(root) == 0 {
			return
		}
		target, ok = root[len(root)-1].(map[string]any)
		if !ok {
			return
		}
		m.marked = target
	}
	target[mcpResponseTruncatedKey] = true
	target[mcpResponseTruncationKey] = m.report()
}
