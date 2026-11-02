#!/usr/bin/env bash
# tui-census.sh — capture every command's human-mode output for an eyeball pass.
#
# Enumerates the full command tree (via cobra __complete), runs each command in
# a small scratch repo under a fake TTY so colors are captured, and writes one
# file per command. Run it twice to compare renderings side by side:
#
#   scripts/tui-census.sh -b /path/to/old/entire-brain before
#   scripts/tui-census.sh after
#   # then: for f in .context/tui-census/after/*.txt; do less -R "$f" ...; done
#
# The scratch repo and its brain live under the output root and are shared
# between runs, so before/after differ only by rendering. Everything is
# isolated via ENTIRE_PLUGIN_*_DIR; nothing touches the real plugin store,
# and ENTIRE_BRAIN_NO_EGRESS=1 guarantees no hosted traffic.
#
# Commands that are interactive, mutating, networked, or token-spending are
# not executed; their file records the skip reason plus their --help output.
# A command missing from the table below is flagged UNMAPPED in its file so
# new commands surface in the census instead of silently vanishing.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
OUT_ROOT="$ROOT/.context/tui-census"
BIN=""
LABEL="current"
TIMEOUT_SECS=90

while [ $# -gt 0 ]; do
  case "$1" in
    -b) BIN="$2"; shift 2 ;;
    -o) OUT_ROOT="$2"; shift 2 ;;
    -h|--help) sed -n '2,20p' "$0"; exit 0 ;;
    *) LABEL="$1"; shift ;;
  esac
done

CAPTURE_DIR="$OUT_ROOT/$LABEL"
SCRATCH="$OUT_ROOT/scratch"
PLUGIN_DIRS="$OUT_ROOT/plugin-dirs"
mkdir -p "$CAPTURE_DIR" "$PLUGIN_DIRS"/{config,data,state,cache}

if [ -z "$BIN" ]; then
  BIN="$OUT_ROOT/entire-brain-$LABEL"
  echo "building $BIN"
  (cd "$ROOT" && go build -o "$BIN" ./cmd/entire-brain)
fi

# Environment for every invocation: colors on, default theme, no egress,
# isolated plugin store.
census_env() {
  env -u NO_COLOR -u ENTIRE_BRAIN_THEME \
    TERM=xterm-256color \
    ENTIRE_BRAIN_NO_EGRESS=1 \
    ENTIRE_PLUGIN_CONFIG_DIR="$PLUGIN_DIRS/config" \
    ENTIRE_PLUGIN_DATA_DIR="$PLUGIN_DIRS/data" \
    ENTIRE_PLUGIN_STATE_DIR="$PLUGIN_DIRS/state" \
    ENTIRE_PLUGIN_CACHE_DIR="$PLUGIN_DIRS/cache" \
    "$@"
}

# Portable timeout (macOS has no coreutils timeout).
with_timeout() {
  perl -e 'alarm shift; exec @ARGV or die "exec: $!"' "$TIMEOUT_SECS" "$@"
}

# ---------------------------------------------------------------- scratch repo
if [ ! -f "$SCRATCH/.census-ready" ]; then
  echo "setting up scratch repo at $SCRATCH"
  rm -rf "$SCRATCH"
  mkdir -p "$SCRATCH"
  (
    cd "$SCRATCH"
    git init -q -b main
    git config user.email census@example.invalid
    git config user.name "TUI Census"
    cat > main.go <<'GO'
package main

import "fmt"

func greet(name string) string { return "hello, " + name }

func main() { fmt.Println(greet("census")) }
GO
    cat > README.md <<'MD'
# census-scratch

Tiny repo used by scripts/tui-census.sh to exercise entire-brain commands.
MD
    git add main.go README.md
    git commit -qm "scratch: seed files"
  )
  (cd "$SCRATCH" && census_env "$BIN" refresh --agent none) || echo "warning: scratch refresh failed; captures will show degraded output"
  (cd "$SCRATCH" && census_env "$BIN" remember "The census scratch repo greets by name; greet() is the only API." --path project.notes.census --json >/dev/null) || true
  touch "$SCRATCH/.census-ready"
fi

# ------------------------------------------------------------- command census
# run:<args>  — execute with these args (empty string means no args)
# skip:<why>  — do not execute; record the reason and --help instead
cmd_plan() {
  case "$1" in
    version|capabilities|doctor|status|stats|overview|agent-guide|verify|path|repo-identity|docs|entities|inspect) echo "run:" ;;
    brief)       echo "run:'add a verbose flag to greet'" ;;
    query)       echo "run:greet" ;;
    recall)      echo "run:'census decisions'" ;;
    get)         echo "run:census-missing-id" ;;
    multi-get)   echo "run:census-missing-id census-missing-id-2" ;;
    show)        echo "run:census-missing-id" ;;
    "docs extract")        echo "run:README.md" ;;
    "entities history")    echo "run:greet" ;;
    "entities show")       echo "run:greet" ;;
    "config show"|"config path") echo "run:" ;;
    memories|"memories files"|"memories global"|"memories map"|"memories outline"|"memories status"|"memories tree"|"memories vitality"|"memories proposals list") echo "run:" ;;
    "memory status"|"memory jobs") echo "run:" ;;
    "patterns"|"patterns status"|"patterns skills") echo "run:" ;;
    "privacy"|"privacy list"|"privacy verify") echo "run:" ;;
    "webhook status"|"webhook events") echo "run:" ;;
    "workspace list") echo "run:" ;;
    "inspect code")     echo "run:greet" ;;
    "inspect context")  echo "run:greet" ;;
    "inspect impact")   echo "run:greet" ;;
    "inspect query-graph") echo "run:greet" ;;
    "inspect search-graph") echo "run:greet" ;;
    "inspect snippet")  echo "run:main.go:1-5" ;;
    "inspect blame")    echo "run:main.go" ;;
    "inspect changes")  echo "run:main.go" ;;
    "inspect tests")    echo "run:greet" ;;
    "inspect dead-code"|"inspect boundaries"|"inspect regressions") echo "run:" ;;

    add)        echo "skip:clones a remote repository" ;;
    setup)      echo "skip:registers daemon/machine state" ;;
    dash)       echo "skip:full-screen interactive TUI" ;;
    mcp)        echo "skip:long-running server" ;;
    viz|"inspect graph-ui") echo "skip:starts a local web server" ;;
    watch|start|stop)       echo "skip:machine-wide background work" ;;
    connect|disconnect|publish) echo "skip:hosted egress" ;;
    distill|"refresh seed"|"patterns refresh"|"patterns verify"|"patterns skills form") echo "skip:may invoke an agent / heavy synthesis" ;;
    reset|gc|repair)        echo "skip:mutates or rebuilds the scratch brain mid-census" ;;
    refresh|"refresh delta"|"refresh history"|"refresh index"|"refresh sessions") echo "skip:already exercised during scratch setup" ;;
    remember)   echo "skip:writes a memory per census run (seeded once during scratch setup)" ;;
    config|"config init") echo "skip:writes plugin configuration" ;;
    "memories import"|"memories sync"|"memories promote"|"memories retract"|"memories review"|"memories gc") echo "skip:mutating or interactive" ;;
    "memories proposals"|"memories proposals apply"|"memories proposals reject"|"memories proposals repair"|"memories proposals show") echo "skip:mutating or needs a proposal id" ;;
    memory|"memory abstract"|"memory cancel"|"memory configure"|"memory configure abstracts"|"memory migrate"|"memory notify"|"memory rebuild"|"memory reconcile"|"memory repair"|"memory retry") echo "skip:mutating or background-job control" ;;
    "privacy exclude"|"privacy include"|"privacy purge"|"privacy retention") echo "skip:mutates retention state" ;;
    "entities backfill"|"entities migrate") echo "skip:mutating migration" ;;
    bundle|"bundle export"|"bundle import") echo "skip:reads/writes bundle files" ;;
    webhook|"webhook test") echo "skip:sends a network request" ;;
    workspace|"workspace add"|"workspace create"|"workspace get"|"workspace inspect"|"workspace inspect context"|"workspace inspect graph"|"workspace inspect impact"|"workspace inspect regressions"|"workspace patterns"|"workspace patterns refresh"|"workspace patterns skills"|"workspace patterns skills form"|"workspace patterns status"|"workspace patterns verify"|"workspace query"|"workspace refresh"|"workspace remove"|"workspace review"|"workspace watch") echo "skip:needs a configured multi-repo workspace" ;;
    "inspect ingest-traces"|"inspect trace-path") echo "skip:needs trace input files" ;;
    *) echo "unmapped:" ;;
  esac
}

list_commands() {
  (cd "$ROOT" && go run ./tools/devcheck commands "$BIN")
}

# Capture one command under a fake TTY so color/caps paths are exercised.
# `script` needs a real command; the wrapper also pins cwd and env.
capture() {
  local outfile="$1"; shift
  (
    cd "$SCRATCH"
    # script(1) forwards its stdin to the pty; starve it or it will eat the
    # caller's command list.
    census_env script -q "$outfile.raw" "$ROOT/scripts/tui-census-run.sh" "$TIMEOUT_SECS" "$@" >/dev/null 2>&1 < /dev/null || true
  )
  # script(1) records CRLF plus the terminal-capability queries termenv sends
  # (OSC 11 background probe, CSI 6n cursor probe) and the EOT from the starved
  # stdin. Strip those; keep the SGR color sequences, they are the census.
  LC_ALL=C sed -e $'s/\\^D\x08\x08//g' -e $'s/\x1b\\]11;?\x1b\\\\//g' -e $'s/\x1b\\]11;?\x07//g' -e $'s/\x1b\\[6n//g' "$outfile.raw" | tr -d $'\r\x04' >> "$outfile"
  rm -f "$outfile.raw"
}

COMMANDS_FILE="$CAPTURE_DIR/commands.txt"
list_commands | sort > "$COMMANDS_FILE"
echo "census: $(wc -l < "$COMMANDS_FILE" | tr -d ' ') commands -> $CAPTURE_DIR"

while IFS= read -r cmd; do
  slug="$(echo "$cmd" | tr ' ' '-')"
  outfile="$CAPTURE_DIR/$slug.txt"
  plan="$(cmd_plan "$cmd")"
  kind="${plan%%:*}"
  args="${plan#*:}"
  case "$kind" in
    run)
      printf '$ entire brain %s %s\n\n' "$cmd" "$args" > "$outfile"
      # The table quotes multi-word arguments; eval re-splits them faithfully.
      eval "set -- $args"
      # shellcheck disable=SC2086
      capture "$outfile" "$BIN" $cmd "$@"
      ;;
    skip)
      printf 'skipped: %s\n\n$ entire brain %s --help\n\n' "$args" "$cmd" > "$outfile"
      # shellcheck disable=SC2086
      capture "$outfile" "$BIN" $cmd --help
      ;;
    unmapped)
      printf 'UNMAPPED: add %q to cmd_plan in scripts/tui-census.sh\n\n$ entire brain %s --help\n\n' "$cmd" "$cmd" > "$outfile"
      # shellcheck disable=SC2086
      capture "$outfile" "$BIN" $cmd --help
      ;;
  esac
  echo "  $slug"
done < "$COMMANDS_FILE"

echo "done: page through with  for f in $CAPTURE_DIR/*.txt; do less -R \"\$f\"; done"
