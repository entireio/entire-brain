#!/bin/sh
# The loop that makes the brain worth having, run end to end for real.
#
#   scripts/demo-agent-session.sh
#
# scripts/demo-setup.sh answers "what does `entire-brain setup` LOOK like". It
# gets there by writing checkpoint refs into the demo repository itself with git
# plumbing, which proves setup can read a checkpoint ref and nothing else. The
# four components that have to cooperate before setup ever sees one --
#
#   entire enable -> an agent session runs -> the session-end hook fires
#                 -> a checkpoint ref is written -> entire-brain setup finds it
#                 -> distill -> the brain answers
#
# -- and the three seams between them are exactly what a synthesised ref skips.
# Every bug this feature has shipped lived on one of those seams: the 'U' bug
# was brain shelling out to the host CLI and choking on its stdout banner; the
# dirty-worktree refusal below is caused by the command the docs put immediately
# before setup. This script runs the whole chain against the real `entire` CLI
# and ASSERTS at each seam, so it fails loudly rather than printing a green
# summary over a broken link.
#
# ---------------------------------------------------------------- what is real
#
# Real, and running here: `entire enable`; the git hooks and agent hook settings
# it installs; the host CLI's own lifecycle hook verbs (`entire hooks
# claude-code session-start|user-prompt-submit|stop|session-end`); the session
# state machine behind them; the post-commit git hook that writes the persistent
# checkpoint; the checkpoint ref itself; `entire checkpoint list`;
# `entire-brain setup`, `distill`, `status`, `overview` and `brief`.
#
# Substituted, and the ONLY substitution: the model. A live agent would call an
# API and spend tokens, so this script writes the session transcript (a Claude
# Code JSONL file) itself and then hands it to the real hooks exactly as the
# agent host would. Nothing downstream can tell the difference -- the hook
# contract is a JSON object on stdin naming a transcript path -- but the
# sentences in that transcript were typed here, not generated. Zero tokens are
# spent by this script, at any step.
#
# The org's closest thing to a tokenless agent is `e2e/vogon` in the entireio/cli
# checkout: a deterministic binary that drives these same hook verbs. It is not
# used here because it needs a second checkout to build, and because `entire
# enable` refuses it by design (agent.IsTestOnly). Driving the hook verbs
# directly needs nothing but the `entire` binary a user already installed.
#
# ------------------------------------------------------------------- the state
#
# Everything the binaries can write goes into a scratch directory under the
# checkout: HOME, all four XDG_*, all four ENTIRE_PLUGIN_*,
# ENTIRE_BRAIN_DAEMON_DIR, and ENTIRE_CONFIG_DIR (the Entire CLI's own store).
# ENTIRE_BRAIN_DAEMON_NO_REGISTER=1 keeps launchctl/systemctl away from the real
# session. This matters more here than in demo-setup.sh, because `entire enable`
# WRITES: git hooks, .entire/settings.json and the agent's settings.json. All of
# it lands in the scratch repository and the sandbox home; teardown proves it by
# diffing the machine's service state against a snapshot taken before the run.
#
# Environment:
#   DEMO_DIR=<path>          put the sandbox somewhere else (default: .demo-agent-session/)
#   DEMO_KEEP=1              keep the sandbox after the run (leak checks still run)
#   ENTIRE_GRAPH_DIR=<path>  an entire-graph checkout, for the semantic component
#   ENTIRE_BRAIN_BIN=<path>  use this entire-brain binary instead of building one
set -eu

is_brain_checkout() { [ -n "${1:-}" ] && [ -d "$1/cmd/entire-brain" ] && [ -f "$1/go.mod" ]; }

script_path=$0
case $script_path in
*/*) ;;
*) script_path=$(command -v -- "$script_path" 2>/dev/null || printf './%s' "$script_path") ;;
esac
hops=0
while [ -L "$script_path" ] && [ "$hops" -lt 40 ]; do
	link=$(readlink -- "$script_path")
	case $link in
	/*) script_path=$link ;;
	*) script_path=$(dirname -- "$script_path")/$link ;;
	esac
	hops=$((hops + 1))
done
script_dir=$(CDPATH='' cd -- "$(dirname -- "$script_path")" && pwd)
repo_root=$(CDPATH='' cd -- "$script_dir/.." && pwd)
if ! is_brain_checkout "$repo_root"; then
	top=$(git -C "$script_dir" rev-parse --show-toplevel 2>/dev/null || true)
	[ -n "$top" ] && is_brain_checkout "$top" && repo_root=$top
fi

demo_dir=${DEMO_DIR:-"$repo_root/.demo-agent-session"}
keep=${DEMO_KEEP:-}
daemon_name=entire-brain-agent-demo

# The scratch repo needs a github-shaped remote or entire-brain and entire-graph
# reduce it to different repo keys and the semantic snapshot is rejected. See
# scripts/demo-setup.sh for the derivation. Nothing is fetched from it, and the
# host CLI's inability to reach it is itself part of what this demo exercises.
demo_remote=https://github.com/entireio/entire-brain-agent-demo.git

say() { printf '\n\033[1m== %s\033[0m\n' "$1"; }
note() { printf '   %s\n' "$1"; }
die() { printf 'demo-agent-session: %s\n' "$1" >&2; exit 1; }

# fail is for a broken SEAM, not a broken environment: it means the chain this
# script exists to prove came apart, so it says which link and stops.
fail() {
	printf '\n\033[1;31mSEAM BROKEN: %s\033[0m\n' "$1" >&2
	exit 1
}

command -v git >/dev/null 2>&1 || die 'git is required'
is_brain_checkout "$repo_root" ||
	die "this script must live in an entire-brain checkout; resolved $repo_root from $0, which has no cmd/entire-brain"
# shellcheck disable=SC2016  # literal backticks, prose not expansion
command -v entire >/dev/null 2>&1 ||
	die 'the parent `entire` CLI is required -- it is the thing under test here, not a helper. Install it from https://github.com/entireio/cli, then re-run'

entire_bin=$(command -v entire)
brain_bin=${ENTIRE_BRAIN_BIN:-}
[ -n "$brain_bin" ] || command -v go >/dev/null 2>&1 ||
	die 'a Go toolchain is required to build the branch binary, or set ENTIRE_BRAIN_BIN to one you already have'

if [ ! -t 1 ]; then
	note 'stdout is not a terminal: setup renders its degraded plain fallback.'
	note 'That is fine here -- this script proves the loop, not the repaints.'
fi

rm -rf "$demo_dir"
mkdir -p "$demo_dir"
demo_dir=$(CDPATH='' cd -- "$demo_dir" && pwd)

sandbox_home="$demo_dir/home"
demo_repo="$demo_dir/repo"
stub_bin="$demo_dir/bin"
transcripts="$sandbox_home/transcripts"
mkdir -p "$sandbox_home" "$stub_bin" "$transcripts"

sandbox_processes() {
	ps -axo pid=,comm=,args= | awk -v root="$demo_dir" '$2 ~ /(^|\/)entire-brain$/ && index($0, root) { print $1 }'
}

cleanup() {
	status=$?
	trap - EXIT INT TERM
	# Stop only brain processes whose executable/arguments identify this sandbox.
	pids=$(sandbox_processes)
	if [ -n "$pids" ]; then
		for pid in $pids; do kill "$pid" 2>/dev/null || true; done
		attempts=0
		while [ "$attempts" -lt 5 ] && [ -n "$(sandbox_processes)" ]; do
			sleep 1
			attempts=$((attempts + 1))
		done
		pids=$(sandbox_processes)
		for pid in $pids; do kill -KILL "$pid" 2>/dev/null || true; done
	fi
	if [ -n "$(sandbox_processes)" ]; then
		printf 'sandbox process still running; preserving %s\n' "$demo_dir" >&2
		status=1
	elif [ -z "$keep" ]; then
		rm -rf "$demo_dir" || status=1
	fi
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# ------------------------------------------------------------ before snapshot

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

real_config_dir=${ENTIRE_CONFIG_DIR:-"${XDG_CONFIG_HOME:-$HOME/.config}/entire"}
real_config_state() { [ -d "$real_config_dir" ] && (cd "$real_config_dir" && find . | sort) || true; }
real_config_state >"$demo_dir/entire-config.before"

# -------------------------------------------------------------- the sandbox
#
# `env -i` is deliberate: an inherited variable is exactly how a "sandboxed" run
# ends up writing to a real store. ENTIRE_CONFIG_DIR is the one this script adds
# over demo-setup.sh's list -- `entire enable` writes login contexts there, and
# it is NOT derived from HOME on every platform.
#
# ENTIRE_TOKEN_STORE=file keeps the CLI off the login keychain. It never logs in
# here (nothing in this demo needs an account) but a keychain prompt in the
# middle of a demo is its own kind of failure.
#
# PAGER/GIT_PAGER=cat is that same rule applied to OUTPUT, and it is not
# optional. TERM is set below, so under a captured pty --
#   script -q run.log ./scripts/demo-agent-session.sh
# -- stdout IS a terminal, and `entire checkpoint list` pages anything taller
# than the window through $PAGER (cmd/entire/cli/explain.go: outputWithPager ->
# buildPagerCmd, which reads PAGER and falls back to `less`). `less` then sits
# on "Press RETURN to continue" and the demo hangs forever, mid-run, with no
# further output. `cat` is honoured by that same lookup, so setting it here
# covers every child and grandchild in one place -- every `entire` verb, every
# `entire-brain` verb, and the `entire` binary entire-brain itself shells out
# to -- rather than a --no-pager flag bolted onto whichever command happens to
# page today.
#
# GIT_TERMINAL_PROMPT=0 is the INPUT half of the same rule: a git child that
# wants a username or password must fail fast instead of blocking on the pty.
# The `entire` CLI already sets it on its own remote calls (internal/remote/
# git.go, disableTerminalPrompt); this extends the guarantee to every other git
# child the sandbox may start.

sandbox() {
	dir=$1
	shift
	(cd "$dir" && env -i \
		PATH="$stub_bin:$(dirname -- "$entire_bin"):${PATH:-/usr/bin:/bin:/usr/sbin:/sbin}" \
		TERM="${TERM:-xterm-256color}" \
		LANG="${LANG:-en_US.UTF-8}" \
		PAGER=cat \
		GIT_PAGER=cat \
		GIT_TERMINAL_PROMPT=0 \
		GOMAXPROCS=4 \
		HOME="$sandbox_home" \
		XDG_CONFIG_HOME="$sandbox_home/.config" \
		XDG_DATA_HOME="$sandbox_home/.local/share" \
		XDG_STATE_HOME="$sandbox_home/.local/state" \
		XDG_CACHE_HOME="$sandbox_home/.cache" \
		ENTIRE_CONFIG_DIR="$sandbox_home/.config/entire" \
		ENTIRE_TOKEN_STORE=file \
		ENTIRE_PLUGIN_CONFIG_DIR="$sandbox_home/plugin/config" \
		ENTIRE_PLUGIN_DATA_DIR="$sandbox_home/plugin/data" \
		ENTIRE_PLUGIN_STATE_DIR="$sandbox_home/plugin/state" \
		ENTIRE_PLUGIN_CACHE_DIR="$sandbox_home/plugin/cache" \
		ENTIRE_BRAIN_DAEMON_DIR="$sandbox_home/daemon" \
		ENTIRE_BRAIN_DAEMON_NO_REGISTER=1 \
		"$@")
}

brain() {
	dir=$1
	shift
	sandbox "$dir" nice -n 10 "$brain_bin" "$@"
}

# run_step prints the command, runs it, prints its exit code, and returns it.
# A demo whose steps have invisible exit codes is a demo that can be green while
# a step failed.
run_step() {
	label=$1
	shift
	printf '   $ %s\n' "$label"
	set +e
	"$@"
	step_status=$?
	set -e
	printf '   [exit %s]\n' "$step_status"
	return $step_status
}

# ----------------------------------------------------------------- 1. build

say "1/9  build entire-brain"
if [ -n "$brain_bin" ]; then
	[ -x "$brain_bin" ] || die "ENTIRE_BRAIN_BIN=$brain_bin is not executable"
	note "using $brain_bin (ENTIRE_BRAIN_BIN)"
else
	brain_bin="$demo_dir/entire-brain"
	(cd "$repo_root" && CGO_ENABLED="${CGO_ENABLED:-0}" GOMAXPROCS=4 nice -n 10 go build -p 2 -trimpath -o "$brain_bin" ./cmd/entire-brain)
	note "$brain_bin"
fi
note "entire: $entire_bin ($("$entire_bin" --version 2>/dev/null | head -n 1))"

# --------------------------------------------------------- 2. the repository

say "2/9  a scratch repository with real content and real history"

git init -q -b main "$demo_repo"
git -C "$demo_repo" config user.name 'Demo Runner'
git -C "$demo_repo" config user.email 'demo@example.invalid'
git -C "$demo_repo" config commit.gpgsign false
git -C "$demo_repo" remote add origin "$demo_remote"

mkdir -p "$demo_repo/greet"
cat >"$demo_repo/go.mod" <<'GOMOD'
module example.com/demo

go 1.22
GOMOD
cat >"$demo_repo/greet/greet.go" <<'SRC'
// Package greet formats greetings.
package greet

// Greet returns a greeting for name.
func Greet(name string) string {
	return "hello " + name
}
SRC
cat >"$demo_repo/main.go" <<'SRC'
package main

import (
	"fmt"

	"example.com/demo/greet"
)

func main() { fmt.Println(greet.Greet("world")) }
SRC
cat >"$demo_repo/README.md" <<'DOC'
# demo

A tiny module used to demonstrate the Entire brain over a real agent session.
DOC
git -C "$demo_repo" add -A
git -C "$demo_repo" commit -qm 'feat: a greeting package'
note "$(git -C "$demo_repo" rev-list --count HEAD) commit, $(git -C "$demo_repo" ls-files | wc -l | tr -d ' ') tracked files, remote $demo_remote"

# ------------------------------------------------------------ 3. entire enable

say "3/9  entire enable  (installs git hooks + the agent's session hooks)"
note 'non-interactive because --agent is passed; it needs no login and no network'
run_step "entire enable --agent claude-code" sandbox "$demo_repo" entire enable --agent claude-code ||
	fail "entire enable failed -- nothing downstream can work without it"

[ -f "$demo_repo/.entire/settings.json" ] ||
	fail "entire enable reported success but wrote no .entire/settings.json"
hook_settings="$demo_repo/.claude/settings.json"
[ -f "$hook_settings" ] ||
	fail "entire enable reported success but wrote no .claude/settings.json, so no session-end hook exists"
grep -q 'hooks claude-code session-end' "$hook_settings" ||
	fail "the settings entire enable wrote carry no session-end hook: $hook_settings"
note "session-end hook: $(grep -o 'entire hooks claude-code session-end' "$hook_settings" | head -n 1)"
note "checkpoint backend: $(sed -n 's/.*"type": *"\([a-z-]*\)".*/\1/p' "$demo_repo/.entire/settings.json" | head -n 1)"

# `entire enable` writes those two directories and does NOT commit them, so the
# very next `entire-brain setup` finds a dirty worktree and refuses to seed or
# index it -- naming --worktree, a flag setup does not accept. That is a real
# first-run trap and the reason `setup` now carries a remedy for it; the honest
# fix for a user is the one applied here, because both files are project
# configuration that belongs in the repository anyway.
say "4/9  commit what enable wrote"
note 'setup refuses to index an uncommitted tree; enable leaves one behind'
# Through sandbox(), not bare `sh -c`. From here on this repository has the git
# hooks `entire enable` installed, so EVERY commit below runs `entire` -- and an
# unsandboxed commit would run it against the developer's real HOME and
# ENTIRE_CONFIG_DIR, and would stop on the prepare-commit-msg hook's
# "Link this commit to session context? [Y]es / [n]o / [a]lways" question. That
# question is asked on /dev/tty, not stdin, so redirecting input cannot answer
# it and the demo blocks forever on a human's terminal. sandbox() sets
# GIT_TERMINAL_PROMPT=0, which the CLI treats as "the caller cannot answer
# prompts" (cmd/entire/cli/interactive/interactive.go, isAgentSubprocessEnv) and
# auto-links instead of asking -- the same default the prompt itself carries.
run_step "stage and commit Entire configuration" \
	sandbox "$demo_repo" sh -c "git add .entire .claude && git commit -qm 'chore: enable Entire session capture'" ||
	fail "could not commit the files entire enable wrote"

# ------------------------------------------------------- 5. the agent session

say "5/9  an agent session, driven through the host CLI's own hook verbs"
note 'the model is substituted (this script writes the transcript); every hook,'
note 'the session state machine and the checkpoint writer are the real ones.'

session_id=7f3a1c20-0000-4000-8000-0000000000a1
transcript="$transcripts/$session_id.jsonl"

hook() {
	verb=$1
	payload=$2
	printf '%s' "$payload" | sandbox "$demo_repo" entire hooks claude-code "$verb"
}

cat >"$transcript" <<JSONL
{"type":"user","message":{"role":"user","content":"Greet(\"\") returns \"hello \" with a dangling space. Make an empty name greet \"there\" instead."},"timestamp":"2026-09-05T10:00:00.000Z"}
{"type":"assistant","message":{"role":"assistant","model":"demo-substituted-model","content":[{"type":"text","text":"I'll guard the empty name inside Greet rather than at the call sites."}]},"timestamp":"2026-09-05T10:00:05.000Z"}
JSONL

run_step "entire hooks claude-code session-start" \
	hook session-start "{\"session_id\":\"$session_id\",\"transcript_path\":\"$transcript\",\"model\":\"demo-substituted-model\",\"cwd\":\"$demo_repo\"}" ||
	fail "the session-start hook failed"

run_step "entire hooks claude-code user-prompt-submit" \
	hook user-prompt-submit "{\"session_id\":\"$session_id\",\"transcript_path\":\"$transcript\",\"prompt\":\"Make an empty name greet \\\"there\\\".\"}" ||
	fail "the user-prompt-submit hook failed"

# The edit an agent would have made, and the transcript entries recording it in
# Claude Code's own shape -- this is what the exporter later reads back.
cat >"$demo_repo/greet/greet.go" <<'SRC'
// Package greet formats greetings.
package greet

// Greet returns a greeting for name. An empty name greets "there" rather than
// producing a dangling "hello ", so no caller has to check first.
func Greet(name string) string {
	if name == "" {
		return "hello there"
	}
	return "hello " + name
}
SRC
cat >>"$transcript" <<JSONL
{"type":"assistant","message":{"role":"assistant","model":"demo-substituted-model","content":[{"type":"tool_use","id":"tu_1","name":"Edit","input":{"file_path":"$demo_repo/greet/greet.go","old_string":"return \"hello \" + name","new_string":"if name == \"\" {\n\t\treturn \"hello there\"\n\t}\n\treturn \"hello \" + name"}}]},"timestamp":"2026-09-05T10:00:10.000Z"}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":"The file has been updated."}]},"timestamp":"2026-09-05T10:00:11.000Z"}
{"type":"assistant","message":{"role":"assistant","model":"demo-substituted-model","content":[{"type":"text","text":"Done. The empty-name guard lives inside Greet rather than at each call site, so no caller has to repeat the check."}]},"timestamp":"2026-09-05T10:00:12.000Z"}
JSONL

run_step "entire hooks claude-code stop" \
	hook stop "{\"session_id\":\"$session_id\",\"transcript_path\":\"$transcript\",\"model\":\"demo-substituted-model\"}" ||
	fail "the stop hook failed"

# The PERSISTENT checkpoint is written by the post-commit git hook `entire
# enable` installed, not by session-end: `entire checkpoint` has no verb that
# creates one (it is list/explain/tokens/search only). Committing is therefore
# part of the loop, not a tidy-up after it.
say "6/9  commit the session's work  (the post-commit hook writes the checkpoint)"
run_step "stage and commit the session work" \
	sandbox "$demo_repo" sh -c "git add -A && git commit -qm 'fix(greet): greet \"there\" for an empty name'" ||
	fail "the commit failed, so no post-commit hook ran"

run_step "entire hooks claude-code session-end" \
	hook session-end "{\"session_id\":\"$session_id\",\"transcript_path\":\"$transcript\",\"model\":\"demo-substituted-model\",\"reason\":\"clear\"}" ||
	fail "the session-end hook failed"

checkpoint_refs=$(git -C "$demo_repo" for-each-ref --format='%(refname)' 'refs/entire/checkpoints/' 'refs/heads/entire/checkpoints/')
[ -n "$checkpoint_refs" ] ||
	fail "the session ended and NO checkpoint ref was written -- this is the seam a synthesised ref hides"
printf '%s\n' "$checkpoint_refs" | sed -e 's/^/   ref  /'

# The two warning lines this step prints are TRUE, and they are left in
# deliberately. `entire enable` writes no checkpoint_remote, so the CLI falls
# back to `origin` for remote checkpoint discovery (cmd/entire/cli/
# git_operations.go, listCheckpointRefsOnRemote) -- and this demo's origin is
# the deliberately unreachable github.com URL above, which exists only so that
# entire-brain and entire-graph derive the same repo key from `git remote
# get-url origin`. Redirecting the warning away would hide the identical
# message in a real run, where an unreachable checkpoint remote is exactly the
# thing a user needs told; writing a checkpoint_remote into .entire/settings.json
# to make the CLI skip discovery would misrepresent what `entire enable` wrote,
# which is the artifact this script asserts on two steps above. So it is
# narrated instead of silenced.
printf '\n'
note 'the two warnings below are expected and correct: origin is deliberately'
note 'unreachable here, so remote checkpoint discovery fails and the list falls'
note 'back to local refs -- which is all this demo ever wrote. A real repo with'
note 'a reachable remote gets its remote checkpoints merged in at this point.'
run_step "entire checkpoint list" sandbox "$demo_repo" entire checkpoint list ||
	fail "entire cannot read back the checkpoint it just wrote"

# -------------------------------------------------- 7. the semantic provider
#
# Optional here, unlike in demo-setup.sh: this demo is about the session seam,
# and requiring a second checkout to see it would be the same mistake. When the
# provider is absent setup reports the semantic component as failed and every
# other component still builds.

if [ -n "${ENTIRE_GRAPH_DIR:-}" ]; then
	graph_root=$(CDPATH='' cd -- "$ENTIRE_GRAPH_DIR" 2>/dev/null && pwd) ||
		die "ENTIRE_GRAPH_DIR=$ENTIRE_GRAPH_DIR does not exist"
else
	graph_root=
	for candidate in "$repo_root/../entire-graph" "$repo_root/../../entire-graph" "${HOME:-}/devenv/entire-graph"; do
		[ -n "$candidate" ] || continue
		resolved=$(CDPATH='' cd -- "$candidate" 2>/dev/null && pwd) || continue
		[ -d "$resolved/cmd/entire-graph" ] || continue
		graph_root=$resolved
		break
	done
fi

say "7/9  the semantic provider"
if [ -n "$graph_root" ] && command -v go >/dev/null 2>&1; then
	note "building entire-graph from $graph_root"
	(cd "$graph_root" && GOMAXPROCS=4 nice -n 10 go build -p 2 -trimpath -o "$demo_dir/entire-graph" ./cmd/entire-graph)
	run_step "entire plugin install entire-graph --force" \
		sandbox "$demo_repo" entire plugin install "$demo_dir/entire-graph" --force ||
		fail "the provider would not register inside the sandbox"
else
	note 'no entire-graph checkout found (set ENTIRE_GRAPH_DIR to use one).'
	note 'setup will report the semantic component as failed; everything else is'
	note 'unaffected, and the session loop this script exists to prove does not'
	note 'depend on it.'
fi

# ------------------------------------------------------------ 8. the brain

say "8/9  entire-brain setup  (over a checkpoint the host CLI actually wrote)"
run_step "entire-brain setup --no-backfill ." \
	brain "$demo_repo" setup --daemon-name "$daemon_name" --no-backfill . ||
	fail "setup exited non-zero: no component built"

# The assertion the whole script is for. `brief` and `status` can look healthy
# off the seed baseline alone, so the session count is checked at the source.
session_count=$(brain "$demo_repo" status --json 2>/dev/null | tr ',' '\n' | sed -n 's/.*"sessions": *\([0-9][0-9]*\).*/\1/p' | head -n 1)
case ${session_count:-0} in
'' | 0) fail "setup built a brain with ZERO sessions -- the checkpoint written above never reached it" ;;
esac
note "setup found $session_count real captured session(s)"

say "9/9  distill and query"
note 'the distill agent is a stub shell script: it reads the transcript on stdin'
note 'and prints fixed fact lines. It never calls a model and spends no tokens.'

cat >"$stub_bin/entire-brain-demo-agent" <<'STUB'
#!/bin/sh
# Demo stub agent. NEVER calls a model, NEVER spends a token.
set -eu

for arg in "$@"; do
	case $arg in
	--version) printf 'entire-brain demo stub agent 1.0.0\n'; exit 0 ;;
	esac
done

input=$(cat 2>/dev/null || true)

# The reconciler reuses the same agent with a different input shape.
case $input in
CANDIDATES*)
	printf '%s\n' "$input" | awk '/^CANDIDATES$/ {seen=1; next} seen && /^[0-9]+/ {printf "%s new - 1.0\n", $1}'
	exit 0
	;;
esac

# One fact, drawn from what the session actually decided. The path must sit
# under a shipped taxonomy category or the fact is dropped.
printf 'architecture\tarchitecture.boundaries.rationale\tThe empty-name guard lives inside Greet rather than at each call site, so no caller has to repeat the check.\n'
STUB
chmod +x "$stub_bin/entire-brain-demo-agent"

run_step "entire-brain distill --agent command ." \
	brain "$demo_repo" distill --agent command --agent-command "$stub_bin/entire-brain-demo-agent" . ||
	fail "distillation of a real session failed"

printf '\n'
run_step "entire-brain status" brain "$demo_repo" status || fail "status failed"
printf '\n'
run_step "entire-brain overview" brain "$demo_repo" overview || fail "overview failed"

printf '\n'
brief_task='change how Greet handles an empty name'
brief_out=$(brain "$demo_repo" brief "$brief_task" 2>&1) || fail "brief failed"
printf '   $ entire-brain brief "%s"\n' "$brief_task"
printf '%s\n' "$brief_out" | sed -e 's/^/   /'

# A brief that answers off the seed baseline alone would prove nothing, so both
# session-derived lines are required: the exported transcript, and the fact
# distilled from it.
printf '%s' "$brief_out" | grep -q "$session_id" ||
	fail "brief drew nothing from the session -- it never reached the history layer"
printf '%s' "$brief_out" | grep -q 'architecture.boundaries.rationale' ||
	fail "brief drew no distilled fact from the session"
note 'both lines above came from the session captured in step 5.'

# ------------------------------------------------------------- the leak checks

say "teardown and leak checks"

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

# `entire enable` is the reason this check exists: it is the one step here that
# writes outside the repository, and if ENTIRE_CONFIG_DIR were not redirected it
# would write login contexts into the developer's real store.
real_config_state >"$demo_dir/entire-config.after"
if diff -u "$demo_dir/entire-config.before" "$demo_dir/entire-config.after" >"$demo_dir/entire-config.diff" 2>&1; then
	printf '   ok    %s is unchanged (entire enable wrote only inside the sandbox)\n' "$real_config_dir"
else
	printf '   LEAK  the real Entire config store changed:\n'
	cat "$demo_dir/entire-config.diff"
	leaked=1
fi

if [ -f "$sandbox_home/.config/entire/contexts.json.lock" ] || [ -d "$sandbox_home/.config/entire" ]; then
	printf '   ok    the Entire CLI wrote its state inside the sandbox\n'
else
	printf '   LEAK  the Entire CLI wrote no state inside the sandbox -- where did it go?\n'
	leaked=1
fi

if [ -n "$keep" ]; then
	printf '\n   sandbox kept at %s\n' "$demo_dir"
fi

printf '\n'
[ "$leaked" -eq 0 ] || die 'the demo leaked state outside its sandbox (see LEAK lines above)'
printf 'demo-agent-session: OK -- enable, a session, its hooks, a real checkpoint,\n'
printf 'setup, distill and brief all ran end to end. Zero tokens were spent; the\n'
printf 'only substituted component was the model that would have written the\n'
printf 'transcript.\n'
