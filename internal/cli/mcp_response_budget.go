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

	// mcpResponseTrimFairRows is the row count every list keeps before any list
	// is allowed a larger share.
	//
	// A single shared width was the whole allocation, and that is what made
	// raising a limit return LESS: brain_context at limit=50 fits whole and
	// returns 200 relations beside 50 symbols, but at limit=200 the shared width
	// clamps all three sibling lists to 90 rows each, so the naturally widest
	// list loses 55% of its rows for asking for more. A shared width also spends
	// the budget badly — it buys the same number of expensive symbol rows as
	// cheap relation rows.
	//
	// 64 is the floor because it covers the short sections whole: a brief's
	// guidance (5 rows), likely_files (12), provider capabilities (4), a
	// context's neighbours (24 at the default limit). Those are never where the
	// bytes are, and trimming them buys nothing while costing the caller a whole
	// section. Lists longer than the floor are the ones that actually compete,
	// and above the floor they compete in proportion to how many rows they have,
	// so a list that is naturally four times wider keeps roughly four times the
	// rows instead of being cut to its narrowest sibling's width.
	//
	// When even the floor does not fit — a brief with thirteen lists, an impact
	// answer at the advertised ceiling — the allocation falls back to the shared
	// width, which is what protects those documents from starving a section.
	mcpResponseTrimFairRows = 64

	// mcpResponseTrimArrayKey is where the rows of an ARRAY-rooted document move
	// when such a document has to be truncated.
	//
	// The marker used to be stamped onto the array's last surviving row, which
	// put two foreign keys (response_truncated, response_truncation) inside a
	// data record: brain_patterns returned 208 of 2,389 rows and row [207] was a
	// Pattern carrying the server's bookkeeping. A client reading the document
	// root found no marker at all, a typed []Pattern decoder either dropped it
	// silently or failed strict decode, and one record was corrupted — which
	// defeats the machine-readable contract the server's instructions promise.
	//
	// A truncated array root is therefore wrapped in an object: the rows move
	// under this key and the marker sits beside them at the root, where a client
	// reading the document root finds it. The UNtruncated shape is left exactly
	// as the tool emits it, so the normal path of every array-rooted tool is
	// unchanged and the shape only moves in the case that was already broken —
	// where it now moves loudly (an object where an array was) instead of
	// silently corrupting a row.
	mcpResponseTrimArrayKey = "items"
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
	"and the answer says how much of it was dropped. Every list keeps its first %d rows where "+
	"the budget allows, and rows above that are shared in proportion to how many rows each list "+
	"has, so no list is starved and a wider list is not cut to a narrower one's width. Counts "+
	"beside a shortened list (pagination.count and friends) are rewritten to the rows actually "+
	"returned. A result whose JSON root is an array is wrapped in {%q: [...]} when it is "+
	"truncated, so the marker is always at the document root and never inside a data row.",
	mcpToolResponseMaxBytes, mcpResponseTruncatedKey, mcpResponseTruncationKey,
	mcpResponseTrimFairRows, mcpResponseTrimArrayKey)

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
// THE ALLOCATION. Every list used to be held to ONE shared row width, because
// filling greedily starves the sections that matter: brain_impact at the
// advertised ceiling has 66k relations beside 9k symbols, and giving the widest
// list its maximum first left the other with zero rows. A shared width fixes
// that, and breaks something else — it cuts a naturally wide list down to its
// narrowest sibling's size, so asking for MORE returns LESS. brain_context at
// limit=50 fits whole and returns 200 relations; at limit=200 the shared width
// clamped every sibling to 90 and the relations list lost 55% of its rows for
// the crime of a larger limit.
//
// Both properties are now held at once, in two phases:
//
//  1. Every list keeps its first mcpResponseTrimFairRows rows (or all of them,
//     when it is shorter). Nothing can be starved below that floor. Short
//     sections — a brief's guidance, a context's neighbours — are covered whole
//     and stop competing.
//  2. Above the floor, the remaining budget is shared IN PROPORTION to how many
//     rows each list actually has, bisected on the widest list's row count. A
//     list with four times the rows keeps roughly four times the rows.
//
// When the floor itself does not fit -- a brief carries thirteen lists, and 64
// rows of each is far past the budget -- the allocation degenerates to the old
// shared width, which is exactly the behaviour that protects those documents.
// So the wide-document case is never worse than it was, and the case that was
// broken is fixed.
//
// Measured over 12 tools at 7 limits each against a real 48k-symbol index, this
// cuts rows lost to non-monotonicity from 247 to 83, the worst single loss from
// 56% to 23%, and returns 238 more rows overall.
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
			if rows < mcpResponseTrimFairRows {
				rows = mcpResponseTrimFairRows
			}
			return min(rows, list.total())
		}
	}

	// Phase 1: can every list keep its fair share? share(mcpResponseTrimFairRows)
	// is exactly uniform(mcpResponseTrimFairRows), so this doubles as the low end
	// of the phase-2 search.
	encoded, fits, measured := probe(uniform(mcpResponseTrimFairRows))
	if !measured {
		return "", false
	}
	if !fits {
		return mcpTrimSharedWidth(&marker, probe, uniform, mcpResponseTrimFairRows, &probes)
	}
	best := encoded

	// Phase 2: bisect the widest list's row count. Document size is monotone in
	// it, so lo is the largest count known to fit and hi the smallest known not
	// to. widest is the document exactly as the tool produced it, which is how we
	// got here, so hi starts out known not to fit.
	lo, hi := mcpResponseTrimFairRows, widest
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

// mcpRowCountRepair binds the sibling fields that counted this list -- brain_code's
// pagination.count, for one -- so every probe rewrites them to the rows actually
// kept. The fields are identified once, against the untouched document, because
// after the first probe their value no longer matches the original row count and
// a field found by matching would silently stop being repaired.
//
// THE SEARCH GOES UP. It used to look only at the list's own object and that
// object's "pagination" child, which is why brain_code was repaired and
// brain_context was not: brain_context's rows live under "context" while its
// pagination sits beside it at the ROOT, one level up. So pagination.count kept
// saying 4559 (the total available, which is what len(symbols) was before the
// trim) beside 94 returned rows, and at limit=200 it said 200 (the requested
// limit) beside 90 — the field changed meaning the moment the trimmer fired,
// twice, in two different directions. Walking the ancestor chain finds it.
//
// Only a field whose value still equals the pre-trim row count is bound, so a
// field counting something else is left alone. Two lists with the same length
// can bind the same field; that is safe because the allocation is a function of
// a list's length, so same-length lists always keep the same number of rows.
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
