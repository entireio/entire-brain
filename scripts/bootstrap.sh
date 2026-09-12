#!/bin/sh
# From nothing to an installed brain, in one command.
#
#   git clone https://github.com/entireio/entire-brain.git
#   entire-brain/scripts/bootstrap.sh
#
# Run from inside a checkout it uses that checkout. Run from anywhere else it
# clones entire-brain first (into ./entire-brain, or $ENTIRE_BRAIN_DIR). Either
# way it then hands off to scripts/install.sh, which checks the prerequisites,
# finds or clones entire-graph, builds and registers both plugins, writes the
# default plugin configuration and runs `entire brain doctor`.
#
# Why there is no `curl ... | sh` one-liner
# -----------------------------------------
# Not caution for its own sake -- it could not work, and would buy nothing if it
# could:
#
#   - entireio/entire-brain is a PRIVATE repository. raw.githubusercontent.com
#     serves 404 for it without a token, so a piped installer would fail for
#     exactly the people it is meant to help, and the fix ("export a GitHub
#     token, then pipe an unread script into your shell") is worse than the
#     disease.
#   - There is nothing to save. Installing means building from source, so the
#     source has to be cloned regardless. A pipe would replace one `git clone`
#     with one `curl`, not remove a step.
#   - This installer builds two binaries, registers them as Entire CLI plugins
#     and writes plugin configuration. Cloning first means the exact script that
#     does that is on disk, at a reviewable commit, before any of it runs. A
#     pipe also executes a truncated download as though it were the whole file.
#
# So the one-liner is a clone and a script, and this file is the script.
#
# Environment:
#   ENTIRE_BRAIN_DIR=<path>   clone into this directory (default ./entire-brain)
#   ENTIRE_BRAIN_REPO=<url>   clone from somewhere else
# Anything scripts/install.sh honours (ENTIRE_GRAPH_DIR, ENTIRE_GRAPH_REPO,
# ENTIRE_GRAPH_CACHE, ENTIRE_INSTALL_OFFLINE) is passed straight through.
set -eu

brain_repo=${ENTIRE_BRAIN_REPO:-https://github.com/entireio/entire-brain.git}
brain_slug=entireio/entire-brain

say() { printf '==> %s\n' "$1"; }
note() { printf '    %s\n' "$1"; }
die() { printf 'bootstrap: %s\n' "$1" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

is_brain_checkout() {
	[ -n "${1:-}" ] || return 1
	[ -d "$1/cmd/entire-brain" ] || return 1
	[ -f "$1/scripts/install.sh" ] || return 1
	return 0
}

abspath() { (CDPATH='' cd -- "$1" 2>/dev/null && pwd) || printf '%s' "$1"; }

# Already in a checkout? Use it, whatever the directory is called.
here=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
if is_brain_checkout "$here"; then
	brain_root=$here
	say "Using the entire-brain checkout this script came from"
	note "$brain_root"
else
	have git || die "git is not on PATH, and entire-brain has to be cloned before
           anything can be installed. Install git and re-run."

	brain_root=${ENTIRE_BRAIN_DIR:-"$PWD/entire-brain"}
	if is_brain_checkout "$brain_root"; then
		brain_root=$(abspath "$brain_root")
		say 'Using the entire-brain checkout already here'
		note "$brain_root"
	else
		if [ -e "$brain_root" ]; then
			die "$brain_root already exists and is not an entire-brain
           checkout. Move it aside, or set ENTIRE_BRAIN_DIR to a free path."
		fi

		say 'Cloning entire-brain'
		note "from $brain_repo"
		note "into $brain_root"
		cloned=''
		if git clone --quiet "$brain_repo" "$brain_root"; then
			cloned=yes
		elif have gh && gh repo clone "$brain_slug" "$brain_root" -- --quiet 2>/dev/null; then
			# gh carries the user's GitHub credentials; git alone may not.
			note 'plain git clone failed; cloned with the GitHub CLI instead'
			cloned=yes
		fi
		if [ -z "$cloned" ]; then
			rm -rf "$brain_root"
			die "could not clone $brain_repo.
           $brain_slug is a PRIVATE repository, so this needs GitHub
           credentials that can read it. Any one of these fixes it:
             gh auth login                 (then re-run this script)
             ENTIRE_BRAIN_REPO=git@github.com:$brain_slug.git $0
             a git credential helper holding a token with repo read access
           If you can already clone it by hand, do that and run
           entire-brain/scripts/install.sh directly."
		fi
		brain_root=$(abspath "$brain_root")
		is_brain_checkout "$brain_root" ||
			die "cloned $brain_repo into $brain_root, but it is not an entire-brain
           checkout (no cmd/entire-brain). Check ENTIRE_BRAIN_REPO."
	fi
fi

say 'Handing off to scripts/install.sh'
exec sh "$brain_root/scripts/install.sh"
