Retrieval (query takes --json/--format json|cli/--limit/-n/--branch,
get/multi-get take --json/--format json|cli/--branch):
  entire brain query "<query>" --json       # BM25 lexical over memories + history + docs — the only ranking (semantic retrieval was removed)
  entire brain query --keyword "<query>" --json      # match keywords and identifiers only
  entire brain query --source history "<query>" --json  # explicit transcript-history escalation when memories miss
  entire brain get <id> --json              # fetch one item by id (fact:… | history:… | doc:…)
  entire brain multi-get <id>... --json     # fetch several by id

Small top-level surface:
  entire brain status [repo] --json         # sources, memories+verification, retrieval identity, freshness/blind spots, live state
  entire brain status --fail-on release     # CI gate: nonzero when freshness is not ok or blind spots exist (also: unsafe, degraded, blind-spots)
  entire brain overview [repo] --json
  entire brain brief "<task>" --json
  entire brain show <id> --json
  entire brain agent-guide
  entire brain path [repo]

Durable memories (curated, provenance-anchored repo knowledge):
  entire brain distill --dry-run --json
  entire brain recall "<query>" [--scope local|cross-cutting] [--expand] --json
  entire brain remember "<memory>" [--path category.sub.type] --json
  entire brain verify [<memory-id | query>] --json
  entire brain memories tree [--path <prefix>] [--depth N]
  entire brain memories retract <memory-id> --json
  entire brain inspect blame <memory-id> --json # source anchors a memory was derived from

Specialist tools (symbol graph + regression analysis — what the verbs can't do):
  entire brain inspect code "<query>" --json        # find a symbol in the graph
  entire brain inspect search-graph "<query>" --json
  entire brain inspect query-graph "type:CALLS <query>" --json
  entire brain inspect graph-ui semantic-graph.html
  entire brain inspect snippet <symbol-or-id> --json
  entire brain inspect trace-path <from-symbol> <to-symbol> --json
  entire brain inspect dead-code --json
  entire brain inspect ingest-traces <json-or-ndjson-file> --json
  entire brain inspect context <symbol-or-id> --json
  entire brain inspect impact <symbol-or-file> --json
  entire brain inspect changes --json
  entire brain inspect tests "<query>" --json
  entire brain inspect boundaries --kind route|tool|workflow --json
  entire brain inspect regressions "<query>" --location-only [--include-deletions] --json
