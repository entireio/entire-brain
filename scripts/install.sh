#!/bin/sh
# One command that installs the Entire code-intelligence system from source.
#
#   scripts/install.sh
#
# What it does to the machine, and nothing else:
#
#   1. builds the entire-graph semantic provider and registers it with the
#      Entire CLI (`entire plugin install`),
#   2. builds entire-brain and registers it the same way,
#   3. writes the default entire-brain plugin configuration
#      (`entire brain config init`),
#   4. runs `entire brain doctor`.
#
# It does NOT require entire-graph to be arranged as a sibling of this checkout,
# and it does not care what this checkout is called. The provider is resolved by
# three routes, and the route actually taken is printed:
#
#   1. $ENTIRE_GRAPH_DIR, when set. An explicit override is never second-guessed:
#      if it does not hold an entire-graph checkout the run stops there rather
#      than quietly installing something else.
#   2. A checkout already on this machine -- siblings of this checkout (and of
#      the main worktree, when this is a linked one), the route-3 cache, and the
#      usual source directories under $HOME. A candidate counts only when it
#      really is entire-graph (cmd/entire-graph and scripts/install-local.sh are
#      both present), so a directory that merely has the name is skipped.
#   3. Otherwise a shallow clone of the public entire-graph repository into a
#      cache directory this script owns, built from there.
#
# Prerequisites are checked before anything is built, and every missing one is
# reported in one pass with the fix for it.
#
# Environment:
#   ENTIRE_GRAPH_DIR=<path>    use this entire-graph checkout; skip discovery
#   ENTIRE_GRAPH_REPO=<url>    clone route 3 from somewhere else
#   ENTIRE_GRAPH_CACHE=<path>  where route 3 clones to
#                              (default $XDG_CACHE_HOME/entire-brain/entire-graph)
#   ENTIRE_INSTALL_OFFLINE=1   never reach the network; fail rather than clone
set -eu

brain_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
graph_repo=${ENTIRE_GRAPH_REPO:-https://github.com/entireio/entire-graph.git}
offline=${ENTIRE_INSTALL_OFFLINE:-}
graph_cache=${ENTIRE_GRAPH_CACHE:-"${XDG_CACHE_HOME:-${HOME:-$brain_root}/.cache}/entire-brain/entire-graph"}

nl='
'

say() { printf '==> %s\n' "$1"; }
note() { printf '    %s\n' "$1"; }
die() { printf 'install: %s\n' "$1" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

# ------------------------------------------------------------------ preflight
#
# Every prerequisite is collected before the first build starts. Dying on the
# first missing one sends a user round the loop once per tool, and the two
# expensive ones -- no C compiler, Go too old -- otherwise surface minutes into
# a build rather than in the first second.

problems=''
add_problem() { problems="${problems}  - ${1}${nl}${nl}"; }

case $(uname -s 2>/dev/null || echo unknown) in
Darwin)
	cc_fix='xcode-select --install'
	git_fix='xcode-select --install, or: brew install git'
	;;
Linux)
	cc_fix='install your distribution C toolchain, e.g. apt install build-essential'
	git_fix='e.g. apt install git'
	;;
*)
	cc_fix='install a C compiler (cc/clang/gcc) and put it on PATH'
	git_fix='install git and put it on PATH'
	;;
esac

if ! have git; then
	add_problem "git is not on PATH
      fix: $git_fix"
fi

if ! have entire; then
	# shellcheck disable=SC2016  # literal backticks, prose not expansion
	add_problem 'the Entire CLI is not on PATH as `entire`
      It is the host that dispatches `entire brain` and `entire graph`, and it
      captures the sessions a brain learns from. A plugin cannot be registered
      without it.
      fix: install the Entire CLI, then re-run this script'
fi

# The required Go version is read from go.mod rather than pinned here, so this
# check cannot drift away from what the build actually needs.
go_required=$(sed -n 's/^go \([0-9][0-9.]*\).*/\1/p' "$brain_root/go.mod" 2>/dev/null | head -1)
[ -n "$go_required" ] || go_required=1.27

if ! have go; then
	add_problem "Go $go_required or newer is not on PATH
      fix: install it from https://go.dev/dl/, or with a version manager:
             mise use -g go@$go_required
             asdf install golang $go_required"
else
	go_have=$(go env GOVERSION 2>/dev/null || echo '')
	go_have=${go_have#go}
	case $go_have in
	[0-9]*.[0-9]*)
		have_major=${go_have%%.*}
		have_minor=${go_have#*.}
		have_minor=${have_minor%%.*}
		want_major=${go_required%%.*}
		want_minor=${go_required#*.}
		want_minor=${want_minor%%.*}
		too_old=''
		if [ "$have_major" -lt "$want_major" ]; then
			too_old=yes
		elif [ "$have_major" -eq "$want_major" ] && [ "$have_minor" -lt "$want_minor" ]; then
			too_old=yes
		fi
		if [ -n "$too_old" ]; then
			# GOTOOLCHAIN defaults to "auto", which fetches the toolchain named
			# in go.mod on demand. That makes an older `go` on PATH harmless --
			# but only while it has not been switched off.
			case ${GOTOOLCHAIN:-auto} in
			auto | *+auto | path+*)
				note "Go $go_have is older than the required $go_required; GOTOOLCHAIN=${GOTOOLCHAIN:-auto} will fetch $go_required during the build"
				;;
			*)
				add_problem "Go $go_have is on PATH, Go $go_required or newer is required, and GOTOOLCHAIN=$GOTOOLCHAIN forbids fetching it
      fix: install Go $go_required, or unset GOTOOLCHAIN so Go fetches it"
				;;
			esac
		fi
		;;
	esac

fi

# entire-graph binds tree-sitter through cgo, so a C compiler is not optional
# for it, even when Go itself is missing. Ask Go for its configured compiler
# when possible, but keep this prerequisite independent so one preflight pass
# reports both missing tools.
go_cc=cc
if have go; then
	go_cc=$(go env CC 2>/dev/null || echo cc)
	[ -n "$go_cc" ] || go_cc=cc
fi
if ! have "$go_cc" && ! have cc && ! have clang && ! have gcc; then
	add_problem "no C compiler found (compiler checked: $go_cc)
      entire-graph uses tree-sitter native parser bindings, so its build needs
      cgo. entire-brain itself is pure Go and does not.
      fix: $cc_fix"
fi

if [ -n "$problems" ]; then
	{
		printf 'install: cannot start -- these prerequisites are missing:\n\n'
		printf '%s' "$problems"
		printf 'Nothing was built and nothing on this machine was changed.\n'
	} >&2
	exit 1
fi

# -------------------------------------------------------- resolve entire-graph
#
# A candidate has to prove it is entire-graph. Trusting the directory name is
# how "clone them side by side, the names matter" became a documented
# prerequisite in the first place.

is_graph_checkout() {
	[ -n "${1:-}" ] || return 1
	[ -d "$1" ] || return 1
	[ -d "$1/cmd/entire-graph" ] || return 1
	[ -f "$1/scripts/install-local.sh" ] || return 1
	return 0
}

abspath() { (CDPATH='' cd -- "$1" 2>/dev/null && pwd) || printf '%s' "$1"; }

graph_root=''
graph_route=''

# Route 1: the explicit override, honoured exactly.
if [ -n "${ENTIRE_GRAPH_DIR:-}" ]; then
	if is_graph_checkout "$ENTIRE_GRAPH_DIR"; then
		graph_root=$(abspath "$ENTIRE_GRAPH_DIR")
		graph_route='ENTIRE_GRAPH_DIR override'
	else
		die "ENTIRE_GRAPH_DIR=$ENTIRE_GRAPH_DIR is not an entire-graph checkout.
       Expected both of:
         $ENTIRE_GRAPH_DIR/cmd/entire-graph
         $ENTIRE_GRAPH_DIR/scripts/install-local.sh
       Point it at a real checkout, or unset it and let this script find or
       clone one."
	fi
fi

# Route 2: something already on this machine.
if [ -z "$graph_root" ]; then
	# When this checkout is a linked worktree, the sibling that matters sits
	# next to the main checkout, not next to the worktree.
	main_worktree_parent=''
	common_dir=$(git -C "$brain_root" rev-parse --git-common-dir 2>/dev/null || echo '')
	if [ -n "$common_dir" ]; then
		case $common_dir in
		/*) ;;
		*) common_dir="$brain_root/$common_dir" ;;
		esac
		main_worktree_parent=$(abspath "$common_dir/../..")
	fi

	candidates="$brain_root/../entire-graph"
	if [ -n "$main_worktree_parent" ]; then
		candidates="$candidates${nl}$main_worktree_parent/entire-graph"
	fi
	candidates="$candidates${nl}$graph_cache"
	if [ -n "${HOME:-}" ]; then
		for d in devenv src code dev projects Projects workspace Developer \
			Documents/Coding go/src/github.com/entireio; do
			candidates="$candidates${nl}$HOME/$d/entire-graph"
		done
		candidates="$candidates${nl}$HOME/entire-graph"
	fi

	graph_cache_abs=$(abspath "$graph_cache")
	seen=''
	# read -r, not word splitting: a source directory may contain spaces.
	while IFS= read -r candidate; do
		[ -n "$candidate" ] || continue
		case "$seen" in *"|$candidate|"*) continue ;; esac
		seen="$seen|$candidate|"
		if is_graph_checkout "$candidate"; then
			graph_root=$(abspath "$candidate")
			if [ "$graph_root" = "$graph_cache_abs" ]; then
				graph_route='cache from a previous run'
			else
				graph_route='checkout discovered on this machine'
			fi
			break
		fi
	done <<EOF
$candidates
EOF
fi

# Route 3: fetch it.
if [ -z "$graph_root" ]; then
	if [ -n "$offline" ]; then
		die "no entire-graph checkout found, and ENTIRE_INSTALL_OFFLINE=$offline forbids
       cloning one. All three ways to supply it:
       1. point at a checkout you already have:
            ENTIRE_GRAPH_DIR=/path/to/entire-graph scripts/install.sh
       2. put one where this script looks -- next to this checkout is simplest:
            git -C '$brain_root/..' clone $graph_repo
       3. unset ENTIRE_INSTALL_OFFLINE and let this script shallow-clone
          $graph_repo into $graph_cache"
	fi
	say 'No entire-graph checkout found; cloning one'
	note "from $graph_repo"
	note "into $graph_cache"
	note 'shallow clone, normally 5-30 seconds; the build after it is the slow part'
	rm -rf "$graph_cache"
	mkdir -p "$(dirname -- "$graph_cache")"
	if ! git clone --depth 1 --quiet "$graph_repo" "$graph_cache"; then
		rm -rf "$graph_cache"
		die "could not clone entire-graph. All three ways to supply it:
       1. point at a checkout you already have:
            ENTIRE_GRAPH_DIR=/path/to/entire-graph scripts/install.sh
       2. clone it yourself, anywhere this script looks -- next to this
          checkout is simplest:
            git -C '$brain_root/..' clone $graph_repo
       3. restore network access to $graph_repo and re-run. It is a public
          repository, so no credentials are needed; a proxy, a firewall or DNS
          is the usual cause."
	fi
	if ! is_graph_checkout "$graph_cache"; then
		die "cloned $graph_repo into $graph_cache, but it is not an entire-graph
       checkout (no cmd/entire-graph). Check ENTIRE_GRAPH_REPO."
	fi
	graph_root=$(abspath "$graph_cache")
	graph_route="shallow clone of $graph_repo"
elif [ "$graph_route" = 'cache from a previous run' ] && [ -z "$offline" ]; then
	# Keep the cache this script owns current, but never let a network hiccup
	# fail an install that could have proceeded with what is already on disk.
	if git -C "$graph_root" fetch --depth 1 --quiet origin HEAD 2>/dev/null &&
		git -C "$graph_root" reset --hard --quiet FETCH_HEAD 2>/dev/null; then
		graph_route='cache from a previous run (refreshed)'
	else
		graph_route='cache from a previous run (not refreshed; network unavailable)'
	fi
fi

# -------------------------------------------------------------------- install

say 'entire-graph semantic provider'
note "route:  $graph_route"
note "source: $graph_root"
(cd "$graph_root" && sh scripts/install-local.sh)

say 'entire-brain (store and MCP tools)'
note "source: $brain_root"
(cd "$brain_root" && sh scripts/install-local.sh)

say 'Default plugin configuration'
if ! entire brain config init; then
	# shellcheck disable=SC2016  # literal backticks, prose not expansion
	printf 'note: `entire brain config init` returned nonzero (config may already exist); continuing\n' >&2
fi

say 'Verifying the plugin environment'
entire brain doctor

cat <<DONE

Entire is installed end to end:
  - entire-graph provider built and registered
      route: $graph_route
      from:  $graph_root
  - entire-brain plugin built and registered
      from:  $brain_root
  - default entire-brain plugin config written

That is the machine. No repository has a brain yet.

Next -- onboard a repository, once per repository:

    cd /path/to/your/repo
    entire brain setup

  setup builds the deterministic core (seconds, no tokens), then starts a
  detached fact backfill and installs ONE machine-wide watcher service, both of
  which SPEND TOKENS. Skip either with --no-backfill / --no-daemon;
  --no-backfill --no-daemon spends nothing at all. Remove the watcher with
  \`entire brain setup --uninstall-daemon\`.

Then:
  - what was built:   entire brain status
  - what this is:     entire brain overview
  - context a task:   entire brain brief "<task>"
  - MCP (stdio):      entire brain mcp
  - re-run checks:    entire brain doctor
DONE
