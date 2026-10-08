"""Summarise one run directory written by run.sh into summary.json fields.

Internal to the harness: run.sh calls it with the run directory it just created. The
argument must be an existing directory containing stream.jsonl; nothing outside it is read.
"""
import json, sys, os, re, collections, subprocess
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
# Escape audit: the clone is the agent's whole world. Any tool input that names a path outside
# it (absolute path elsewhere, or a parent traversal) is recorded; one that reaches the source
# repository this harness lives in, or names harness content, marks the run contaminated.
clone = os.path.realpath(os.path.join(run, "repo"))
# The output root holds every run's clone, transcript and answer. A path under it that is not
# this run's clone is another run's data, which may contain the answer: contamination.
out_root = os.path.realpath(os.environ["LADDER_OUT_ROOT"]) if os.environ.get("LADDER_OUT_ROOT") else os.path.realpath(os.path.dirname(os.path.dirname(run)))
def _go_env(name):
    v = os.environ.get(name)
    if not v:
        try:
            v = subprocess.run(["go", "env", name], capture_output=True, text=True, timeout=20).stdout.strip()
        except Exception:
            v = ""
    return os.path.realpath(v) if v else None
# Allowed outside the clone: tool and system trees, and the Go caches the toolchain reports.
# No blanket temp-directory allowance: the output root lives there too.
SYSTEM_PREFIXES = tuple(p + os.sep for p in ["/usr", "/bin", "/opt", "/dev", "/etc"] + [x for x in (_go_env("GOCACHE"), _go_env("GOMODCACHE")) if x])
src = os.path.realpath(os.environ["LADDER_SRC"]) if os.environ.get("LADDER_SRC") else None
escapes = []
def audit_paths(tool, inp):
    global contaminated
    text = json.dumps(inp)
    for m in re.findall(r"(?:(?<=[\s\"'=:(])|^)(/[^\s\"'`;|&)>]+|(?:\.\./)+[^\s\"'`;|&)>]*)", text):
        cand = os.path.realpath(m if m.startswith("/") else os.path.join(clone, m))
        if cand.startswith(clone + os.sep) or cand == clone:
            continue
        if cand.startswith(SYSTEM_PREFIXES):
            continue  # tool binaries, devices, Go caches
        escapes.append(f"{tool}: {m[:160]}")
        if src and (cand == src or cand.startswith(src + os.sep)):
            contaminated = True  # reached the source repository and its un-stripped harness
        if cand.startswith(out_root + os.sep) and not (cand == clone or cand.startswith(clone + os.sep)):
            contaminated = True  # reached another run's clone, transcript or answer
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
                elif b["name"] in ("Agent", "Task"):
                    cmds.append("AGENT:" + str(b["input"].get("prompt", ""))[:200])
                probe_text = json.dumps(b.get("input", {}))
                if any(m in probe_text for m in HARNESS_MARKERS):
                    probes.append(b["name"] + ": " + probe_text[:200])
                audit_paths(b["name"], b.get("input", {}))
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
    "harness_probes": probes, "escape_paths": escapes, "contaminated": contaminated,
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
