#!/bin/sh
# One command that lets a human WATCH `entire-brain setup` work.
#
#   scripts/demo-setup.sh
#
# This is not scripts/trial-setup.sh. That one is the safety harness: it proves
# setup refuses a non-repository, survives being offline, is idempotent, and
# leaks nothing. It answers "is this safe". This one answers "what does it look
# like", and the two must not be merged: the trial deliberately runs setup
# against a tiny synthetic tree with no sessions and no semantic provider, which
# is the fastest way to check behaviour and the worst way to see it.
#
# The difference that matters is that this script never pipes, captures or tees
# the run. `setup` renders a single in-place line -- a spinner, a determinate
# bar that colour-ramps as it fills, a tick row of components landing one by one
# -- and every one of those is gated on the destination being a terminal
# (internal/tui/render.go, Caps.TTY / Caps.Color). Pipe it and you get the
# deliberate degraded fallback: whole lines, ASCII, no colour. Every previous
# demo of this feature was piped, which is why it always looked unbuilt.
#
# The three things this arranges that a bare `setup` in a scratch directory does
# not:
#
#   1. A repository with real substance. The brain's own checkout is cloned
#      (~1200 tracked files, the full commit history), so the semantic index has
#      enough to chew on that the spinner and the in-place repaints are visible
#      for a minute rather than 500ms.
#   2. A working entire-graph provider, installed INTO THE SANDBOX. Without it
#      the semantic component fails and the run ends on a red x. `entire plugin
#      install` honours the sandboxed HOME, so this registers a provider the
#      sandbox can see and the real machine cannot.
#   3. Captured sessions, and a stub agent to distill them with. The facts bar
#      is `distilled/total sessions`; with no sessions it is an empty grey track
#      that never moves. Sessions are seeded into the repo's checkpoint ref in
#      the same shape `entire` itself writes, then distilled in paced batches so
#      the bar is watched filling and colour-ramping red -> amber -> green
#      instead of jumping from nothing to done.
#
# NO TOKENS ARE SPENT. The distill agent is a stub shell script that reads the
# transcript on stdin and prints fixed fact lines. It never calls a model.
#
# Everything the binary can write is redirected into a scratch directory under
# the checkout -- HOME, all four XDG_*, all four ENTIRE_PLUGIN_*,
# ENTIRE_BRAIN_DAEMON_DIR -- and ENTIRE_BRAIN_DAEMON_NO_REGISTER=1 keeps
# launchctl/systemctl away from your real session. Teardown proves it by diffing
# the machine's service state against a snapshot taken before the run.
#
# Environment:
#   DEMO_DIR=<path>          put the sandbox somewhere else (default: .demo-setup/)
#   DEMO_KEEP=1              keep the sandbox after the run (leak checks still run)
#   DEMO_SESSIONS=<n>        how many sessions to seed (default: 30)
#   DEMO_BATCH=<n>           sessions distilled per visible pass (default: 5)
#   DEMO_PAUSE=<seconds>     pause between passes so the ramp is readable (default: 1)
#   ENTIRE_GRAPH_DIR=<path>  entire-graph checkout (default: ../entire-graph)
set -eu

repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
demo_dir=${DEMO_DIR:-"$repo_root/.demo-setup"}
keep=${DEMO_KEEP:-}
sessions=${DEMO_SESSIONS:-30}
batch=${DEMO_BATCH:-5}
pause=${DEMO_PAUSE:-1}
graph_root=${ENTIRE_GRAPH_DIR:-"$repo_root/../entire-graph"}

# Distinct from the real default (entire-brain-watch) so a developer's own
# watcher is never confused with this demo's.
daemon_name=entire-brain-demo

# The demo repo needs a remote that BOTH entire-brain and entire-graph reduce to
# the same key, or the semantic snapshot is rejected on a repo-key mismatch and
# the run ends on a red x. entire-graph maps a github URL to gh/<owner>/<name>
# (internal/sem/provider.go, githubRepoKey) and falls back to local/<basename>;
# entire-brain derives gh/<owner>/<name> the same way but falls back to
# local/<basename>-<hash>. The two fallbacks never agree, so a demo repo with no
# github remote can never build a semantic index. Nothing is fetched from it.
demo_remote=https://github.com/entireio/entire-brain-demo.git

say() { printf '\n\033[1m== %s\033[0m\n' "$1"; }
note() { printf '   %s\n' "$1"; }
die() { printf 'demo-setup: %s\n' "$1" >&2; exit 1; }

command -v git >/dev/null 2>&1 || die 'git is required'
command -v go >/dev/null 2>&1 || die 'a Go toolchain is required to build the branch binary'
command -v entire >/dev/null 2>&1 || die 'the parent `entire` CLI is required (it dispatches `entire graph` to the provider)'

case $sessions in *[!0-9]* | '') die 'DEMO_SESSIONS must be a whole number' ;; esac
case $batch in *[!0-9]* | '') die 'DEMO_BATCH must be a whole number' ;; esac
[ "$sessions" -gt 0 ] || die 'DEMO_SESSIONS must be greater than zero'
[ "$batch" -gt 0 ] || die 'DEMO_BATCH must be greater than zero'

if [ ! -t 1 ]; then
	printf 'demo-setup: stdout is not a terminal.\n' >&2
	printf '            Every in-place repaint, the spinner and all colour are gated on\n' >&2
	printf '            Caps.TTY/Caps.Color, so piping this run shows the degraded plain\n' >&2
	printf '            fallback -- which is exactly the thing this script exists to avoid.\n' >&2
	printf '            Run it straight in a terminal. To capture a transcript anyway, use\n' >&2
	printf '            a pty: script -q /dev/null scripts/demo-setup.sh\n' >&2
	exit 2
fi

rm -rf "$demo_dir"
mkdir -p "$demo_dir"
demo_dir=$(CDPATH='' cd -- "$demo_dir" && pwd)

sandbox_home="$demo_dir/home"
demo_repo="$demo_dir/repo"
stub_bin="$demo_dir/bin"
mkdir -p "$sandbox_home" "$stub_bin"

cleanup() {
	status=$?
	if [ -z "$keep" ]; then
		# A detached memory worker may still hold a file open for a moment; one
		# retry is enough and beats leaving the sandbox behind.
		rm -rf "$demo_dir" 2>/dev/null || { sleep 2; rm -rf "$demo_dir" 2>/dev/null || true; }
	fi
	exit $status
}
trap cleanup EXIT INT TERM

# ------------------------------------------------------------ before snapshot
#
# The leak check is a DIFF, not a grep. A developer may legitimately have a real
# entire-brain watcher installed; greping for it after the run would report that
# as a leak, and a check that cries wolf is a check nobody reads.

service_state() {
	if command -v launchctl >/dev/null 2>&1; then
		launchctl list 2>/dev/null | awk '{print $3}' | grep -i 'io\.entire\.' || true
	fi
	if command -v systemctl >/dev/null 2>&1; then
		systemctl --user list-unit-files --no-legend 2>/dev/null | awk '{print $1}' | grep -i 'entire' || true
	fi
	for dir in "$HOME/Library/LaunchAgents" "$HOME/.config/systemd/user"; do
		[ -d "$dir" ] || continue
		find "$dir" -maxdepth 1 -name '*entire*' -exec basename {} \; 2>/dev/null || true
	done
}
service_state | sort >"$demo_dir/services.before"

# ------------------------------------------------------- the sandboxed binary
#
# Both builds are niced and capped: indexing a real repository is the point of
# this demo, and an unniced parallel Go build on top of it is what turns "my
# laptop got warm" into a complaint.

say "1/6  build entire-brain and entire-graph"
brain_bin="$demo_dir/entire-brain"
graph_bin="$demo_dir/entire-graph"
(cd "$repo_root" && CGO_ENABLED="${CGO_ENABLED:-0}" GOMAXPROCS=4 nice -n 10 go build -p 2 -trimpath -o "$brain_bin" ./cmd/entire-brain)
note "$brain_bin"
[ -d "$graph_root" ] || die "entire-graph not found at $graph_root; set ENTIRE_GRAPH_DIR to its checkout"
(cd "$graph_root" && GOMAXPROCS=4 nice -n 10 go build -p 2 -trimpath -o "$graph_bin" ./cmd/entire-graph)
note "$graph_bin"

# sandbox runs a command with EVERY state path redirected into the sandbox.
# `env -i` is deliberate: an inherited variable is exactly how a "sandboxed" run
# ends up writing to a real store. The stub agent's directory goes FIRST on PATH
# so nothing can reach a real agent CLI.
sandbox() {
	dir=$1
	shift
	(cd "$dir" && env -i \
		PATH="$stub_bin:$PATH" \
		TERM="${TERM:-xterm-256color}" \
		LANG="${LANG:-en_US.UTF-8}" \
		GOMAXPROCS=4 \
		HOME="$sandbox_home" \
		XDG_CONFIG_HOME="$sandbox_home/.config" \
		XDG_DATA_HOME="$sandbox_home/.local/share" \
		XDG_STATE_HOME="$sandbox_home/.local/state" \
		XDG_CACHE_HOME="$sandbox_home/.cache" \
		ENTIRE_PLUGIN_CONFIG_DIR="$sandbox_home/plugin/config" \
		ENTIRE_PLUGIN_DATA_DIR="$sandbox_home/plugin/data" \
		ENTIRE_PLUGIN_STATE_DIR="$sandbox_home/plugin/state" \
		ENTIRE_PLUGIN_CACHE_DIR="$sandbox_home/plugin/cache" \
		ENTIRE_BRAIN_DAEMON_DIR="$sandbox_home/daemon" \
		ENTIRE_BRAIN_DAEMON_NO_REGISTER=1 \
		STUB_AGENT_COUNTER="$demo_dir/stub-agent.count" \
		"$@")
}

# brain runs the branch binary under that sandbox. Nothing here redirects its
# output: `setup` renders in place on the terminal, and a pipe would silently
# turn every repaint into a separate plain line.
brain() {
	dir=$1
	shift
	sandbox "$dir" nice -n 10 "$brain_bin" "$@"
}

# ------------------------------------------ the semantic provider, sandboxed
#
# `setup` runs the provider as `entire graph ...` -- the binary name is not
# configurable from setup (internal/cli/watch.go pins graphBinary to "entire").
# So the provider has to be registered where the sandbox's `entire` will look
# for it, which `entire plugin install` does correctly: it resolves the plugin
# root from the (sandboxed) HOME, so this writes a symlink under the sandbox and
# leaves the developer's real plugin registry untouched.

say "2/6  register entire-graph inside the sandbox"
sandbox "$repo_root" entire plugin install "$graph_bin" --force
note "provider: $(sandbox "$repo_root" entire graph doctor --json | sed -e 's/.*"provider":"\([^"]*\)".*/\1/')"

# ------------------------------------------------------ the demo repository

say "3/6  build the demo repository (real files, real history, seeded sessions)"

# --single-branch keeps the clone to one branch, which drops the checkout's own
# entire/checkpoints/v1 ref. That is deliberate: the real ref carries hundreds
# of sessions and ~800MB of exported transcript, which makes the sandbox heavy
# and the facts bar too coarse to watch. Sessions are seeded below instead, at a
# count chosen so each pass moves the bar a visible amount.
git clone --local --single-branch -q "$repo_root" "$demo_repo"
git -C "$demo_repo" remote set-url origin "$demo_remote"
git -C "$demo_repo" config user.name 'Demo Runner'
git -C "$demo_repo" config user.email 'demo@example.invalid'
git -C "$demo_repo" config commit.gpgsign false
note "$(git -C "$demo_repo" rev-list --count HEAD) commits, $(git -C "$demo_repo" ls-files | wc -l | tr -d ' ') tracked files"

# The mined benchmark corpora are generated, minified JSON. The semantic
# provider correctly declines to parse them, but every skipped file is then
# reported as a blind spot, which degrades semantic_completeness and ends an
# otherwise all-green run on `x freshness degraded`. The checkout's own
# .brainignore already excludes the sibling generated directories
# (benchmarks/agent-brain/{bin,cache,results}/) and simply does not list these
# two; excluding them here is the same judgement, applied to the demo clone
# only. Committed rather than left in the worktree so the tree stays clean --
# a dirty worktree is itself a freshness warning.
{
	printf 'benchmarks/agent-brain/mined-c0701/\n'
	printf 'benchmarks/agent-brain/mined-candidates/\n'
} >>"$demo_repo/.brainignore"
git -C "$demo_repo" add .brainignore
git -C "$demo_repo" commit -qm 'chore: exclude generated benchmark corpora from the brain index'

# Sessions live in the repo's checkpoint ref, not in a home directory: the
# exporter reads refs/heads/entire/checkpoints/v1 and walks
# <id[0:2]>/<id[2:]>/metadata.json plus <...>/<n>/{metadata.json,full.jsonl}
# (internal/cli/export.go). Writing that tree directly with plumbing is what
# `entire` does, and it keeps the demo offline and instant.
seed_index="$demo_repo/.git/demo-seed-index"
rm -f "$seed_index"
demo_branch=$(git -C "$demo_repo" symbolic-ref --short HEAD)
i=1
while [ "$i" -le "$sessions" ]; do
	id=$(printf 'de%010x' "$i")
	sid=$(printf '%08x-0000-4000-8000-%012x' "$i" "$i")
	dir="$(printf '%s' "$id" | cut -c1-2)/$(printf '%s' "$id" | cut -c3-)"
	stamp=$(printf '2026-08-%02dT%02d:00:00Z' $(((i % 28) + 1)) $((i % 24)))

	root=$(printf '{"checkpoint_id":"%s","branch":"%s","sessions":[{"metadata":"%s/0/metadata.json","transcript":"%s/0/full.jsonl"}]}\n' \
		"$id" "$demo_branch" "$dir" "$dir" | git -C "$demo_repo" hash-object -w --stdin)
	meta=$(printf '{"checkpoint_id":"%s","session_id":"%s","created_at":"%s","branch":"%s","agent":"Claude Code","model":"demo-stub"}\n' \
		"$id" "$sid" "$stamp" "$demo_branch" | git -C "$demo_repo" hash-object -w --stdin)
	transcript=$({
		printf '{"type":"user","message":{"role":"user","content":"Session %s. Keep the scratch sandbox inside the checkout - a shared /tmp is contended and can hand back another agent%ss file."}}\n' "$i" "'"
		printf '{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Understood, scratch stays in the worktree."}]}}\n'
		printf '{"type":"user","message":{"role":"user","content":"From now on run the focused package tests while iterating and the full suite exactly once at the end."}}\n'
		printf '{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Recorded as a standing rule for this repository."}]}}\n'
	} | git -C "$demo_repo" hash-object -w --stdin)

	GIT_INDEX_FILE="$seed_index" git -C "$demo_repo" update-index --add --cacheinfo 100644 "$root" "$dir/metadata.json"
	GIT_INDEX_FILE="$seed_index" git -C "$demo_repo" update-index --add --cacheinfo 100644 "$meta" "$dir/0/metadata.json"
	GIT_INDEX_FILE="$seed_index" git -C "$demo_repo" update-index --add --cacheinfo 100644 "$transcript" "$dir/0/full.jsonl"
	i=$((i + 1))
done
seed_tree=$(GIT_INDEX_FILE="$seed_index" git -C "$demo_repo" write-tree)
seed_commit=$(git -C "$demo_repo" commit-tree "$seed_tree" -m "chore: seed $sessions demo sessions")
git -C "$demo_repo" update-ref refs/heads/entire/checkpoints/v1 "$seed_commit"
rm -f "$seed_index"
note "$sessions captured sessions seeded into refs/heads/entire/checkpoints/v1"

# ------------------------------------------------------------- the stub agent
#
# Distillation is agent-required, and the point of this demo is to watch the
# facts bar move without paying for it. This stub satisfies both contracts the
# distiller can use: `--version` for agent detection, and raw
# kind<TAB>path<TAB>fact lines on stdout for `--agent command` (which passes the
# argv through verbatim and does not JSON-decode the result). The taxonomy roots
# are the real ones -- a line whose path is outside the taxonomy is dropped.

cat >"$stub_bin/entire-brain-demo-agent" <<'STUB'
#!/bin/sh
# Demo stub agent. Reads a transcript chunk on stdin, prints fact lines. It
# NEVER calls a model and NEVER spends a token.
set -eu

for arg in "$@"; do
	case $arg in
	--version) printf 'entire-brain demo stub agent 1.0.0\n'; exit 0 ;;
	esac
done

input=$(cat 2>/dev/null || true)

# The reconciler reuses the same agent with a different input shape: a list of
# candidate facts under a literal CANDIDATES header, expecting
# "<candidate#> <decision> <existing#-or-dash> <confidence>" back. Answering it
# with fact lines would degrade every candidate with a warning.
case $input in
CANDIDATES*)
	printf '%s\n' "$input" | awk '/^CANDIDATES$/ {seen=1; next} seen && /^[0-9]+/ {printf "%s new - 1.0\n", $1}'
	exit 0
	;;
esac

counter=${STUB_AGENT_COUNTER:-}
n=0
if [ -n "$counter" ]; then
	n=$(cat "$counter" 2>/dev/null || printf '0')
	case $n in *[!0-9]* | '') n=0 ;; esac
	n=$((n + 1))
	printf '%s\n' "$n" >"$counter" 2>/dev/null || true
fi

# Most chunks yield nothing. That is the documented default in the distill
# prompt, and a stub that emitted a fact every single time would misrepresent
# how the real thing behaves.
case $((n % 4)) in
0) printf 'convention\tworkflow.testing.scope\tFocused package tests run while iterating; the full suite runs exactly once before the branch is pushed.\n' ;;
1) printf 'gotcha\tproject.indexing.cache\tThe semantic index cache is keyed by tree, so a repo whose key changed after its first index is served a stale snapshot until the cache is cleared.\n' ;;
2) ;;
3) printf 'preference\tpreferences.workflow.scratch\tScratch files stay inside the worktree rather than /tmp, which is shared and contended between agents.\n' ;;
esac
STUB
chmod +x "$stub_bin/entire-brain-demo-agent"
note "stub agent: $stub_bin/entire-brain-demo-agent (no model calls, no tokens)"

# ------------------------------------------------------------------- the run
#
# Straight to the terminal. No pipe, no tee, no command substitution: this is
# the whole reason the script exists.

say "4/6  entire-brain setup  (watch the tick row, the spinner and the bar)"
brain "$demo_repo" setup --daemon-name "$daemon_name" --no-backfill .

# ------------------------------------------------------------- the facts bar
#
# The backfill `setup` starts is detached on purpose -- it runs for hours on a
# real corpus and nobody watches it. That makes it the wrong thing to
# demonstrate, so the same work is driven here in the foreground, in paced
# batches, with the bar re-rendered between them. --max-sessions caps how many
# not-yet-distilled sessions a pass takes, which is exactly the pacing knob.

say "5/6  distill the seeded sessions in paced batches (stub agent, zero tokens)"
note "$sessions sessions, $batch per pass -- the facts bar fills and ramps red -> amber -> green"
printf '\n'
done_count=0
while [ "$done_count" -lt "$sessions" ]; do
	brain "$demo_repo" distill \
		--agent command \
		--agent-command "$stub_bin/entire-brain-demo-agent" \
		--max-sessions "$batch" \
		--newest-first \
		.
	done_count=$((done_count + batch))
	brain "$demo_repo" status
	[ "$pause" = "0" ] || sleep "$pause"
done

# ------------------------------------------------------------- the leak checks

say "6/6  teardown and leak checks"

leaked=0
service_state | sort >"$demo_dir/services.after"
if ! diff -u "$demo_dir/services.before" "$demo_dir/services.after" >"$demo_dir/services.diff" 2>&1; then
	printf '   LEAK  the machine gained or lost a service:\n'
	cat "$demo_dir/services.diff"
	leaked=1
else
	printf '   ok    launchd/systemd state is byte-identical to before the run\n'
fi

if command -v launchctl >/dev/null 2>&1; then
	found=$(launchctl list 2>/dev/null | grep -i "$daemon_name" || true)
	if [ -n "$found" ]; then
		printf '   LEAK  a demo job is registered with launchd:\n%s\n' "$found"
		leaked=1
	else
		printf '   ok    launchctl list has no %s job\n' "$daemon_name"
	fi
else
	printf '   skip  launchctl (not present)\n'
fi

if command -v systemctl >/dev/null 2>&1; then
	found=$(systemctl --user list-units --all --no-legend 2>/dev/null | grep -i "$daemon_name" || true)
	if [ -n "$found" ]; then
		printf '   LEAK  a demo unit is registered with systemd:\n%s\n' "$found"
		leaked=1
	else
		printf '   ok    systemctl --user has no %s unit\n' "$daemon_name"
	fi
else
	printf '   skip  systemctl (not present)\n'
fi

# The provider must have been registered INSIDE the sandbox. Seeing it there is
# how you know the redirect worked rather than the install having silently gone
# to the developer's real plugin root.
if [ -e "$sandbox_home/.local/share/entire/plugins/bin/entire-graph" ]; then
	printf '   ok    entire-graph was registered inside the sandbox\n'
else
	printf '   LEAK  entire-graph was not registered inside the sandbox\n'
	leaked=1
fi

if [ -n "$keep" ]; then
	printf '\n   sandbox kept at %s\n' "$demo_dir"
fi

printf '\n'
[ "$leaked" -eq 0 ] || die 'the demo leaked state outside its sandbox (see LEAK lines above)'
printf 'demo-setup: OK -- setup built every component, the facts bar filled %s/%s, and nothing leaked.\n' "$sessions" "$sessions"
