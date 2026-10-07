"""Summarise one run directory written by run.sh into summary.json fields.

Internal to the harness: run.sh calls it with the run directory it just created. The
argument must be an existing directory containing stream.jsonl; nothing outside it is read.
"""
import json, sys, os, re, collections
if len(sys.argv) != 2:
    sys.exit("usage: parse.py <run-dir>")
run = os.path.realpath(sys.argv[1])
if not os.path.isdir(run) or not os.path.isfile(os.path.join(run, "stream.jsonl")):
    sys.exit(f"{sys.argv[1]!r} is not a run directory (expected stream.jsonl inside it)")
tools = collections.Counter(); cmds = []; result = {}
# Leakage audit: the harness directory is stripped from the clone, but an agent may still probe
# for it. Probes are recorded; a tool result that carries rubric or README content marks the run
# contaminated so grade.py excludes it.
HARNESS_MARKERS = ("memory-ladder", "memory_ladder", "rubric")
CONTENT_MARKERS = ('"recovered"', ".rubric.json", "_comment", "## Run 1", "## Run 2", "Recovered the recorded")
probes = []; contaminated = False
for line in open(os.path.join(run, "stream.jsonl")):
    line = line.strip()
    if not line:
        continue
    try:
        ev = json.loads(line)
    except Exception:
        continue
    if ev.get("type") == "assistant":
        for b in ev.get("message", {}).get("content", []):
            if b.get("type") == "tool_use":
                tools[b["name"]] += 1
                if b["name"] == "Bash":
                    cmds.append(b["input"].get("command", "")[:300])
                probe_text = json.dumps(b.get("input", {}))
                if any(m in probe_text for m in HARNESS_MARKERS):
                    probes.append(b["name"] + ": " + probe_text[:200])
                elif b["name"] in ("Agent", "Task"):
                    cmds.append("AGENT:" + str(b["input"].get("prompt", ""))[:200])
    elif ev.get("type") == "user":
        for b in ev.get("message", {}).get("content", []):
            if isinstance(b, dict) and b.get("type") == "tool_result":
                body = b.get("content"); body = body if isinstance(body, str) else json.dumps(body)
                if any(m in body for m in CONTENT_MARKERS) and "not available in this environment" not in body:
                    contaminated = True
    elif ev.get("type") == "result":
        result = ev
u = result.get("usage", {})
failed_marker = os.path.exists(os.path.join(run, "failed.txt"))
completed = bool(result) and not result.get("is_error") and result.get("subtype") == "success" and bool(result.get("result")) and not failed_marker
entire_cmds = [c for c in cmds if re.search(r"\bentire\b", c)]
subs = collections.Counter(re.sub(r".*?\bentire\s+(\S+).*", r"\1", c, flags=re.S) for c in entire_cmds)
out = {
    "cond": os.path.basename(run),
    "is_error": result.get("is_error"), "subtype": result.get("subtype"),
    "completed": completed, "failed_marker": failed_marker,
    "harness_probes": probes, "contaminated": contaminated,
    "head": open(os.path.join(run, "head.txt")).read().strip() if os.path.exists(os.path.join(run, "head.txt")) else None,
    "duration_s": round((result.get("duration_ms") or 0) / 1000, 1),
    "cost_usd": round(result.get("total_cost_usd") or 0, 3),
    "num_turns": result.get("num_turns"),
    "tool_calls": sum(tools.values()), "tools": dict(tools),
    "bash_calls": len(cmds), "entire_calls": len(entire_cmds),
    "entire_subcommands": dict(subs),
    "tokens_in": (u.get("input_tokens") or 0) + (u.get("cache_read_input_tokens") or 0) + (u.get("cache_creation_input_tokens") or 0),
    "tokens_out": u.get("output_tokens"),
    "result": result.get("result"),
    "commands": cmds,
}
print(json.dumps(out, indent=1))
