#!/bin/sh
# One command that shows what `entire-brain setup` does to a brand new
# repository, WITHOUT touching anything you own.
#
#   scripts/trial-setup.sh
#
# It is safe to run on a real machine. Every piece of state the binary can write
# is redirected into a scratch directory under the checkout:
#
#   HOME, XDG_{CONFIG,DATA,STATE,CACHE}_HOME     the brain's own stores
#   ENTIRE_PLUGIN_{CONFIG,DATA,STATE,CACHE}_DIR  the plugin dirs the host sets
#   ENTIRE_BRAIN_DAEMON_DIR                      where the unit FILE is written
#   ENTIRE_BRAIN_DAEMON_NO_REGISTER              launchctl/systemctl are NEVER called
#
# The last two are both required, and they are not the same guarantee.
# ENTIRE_BRAIN_DAEMON_DIR moves the plist / systemd unit; `launchctl load -w`
# acts on your live session whatever directory the file came from, so without
# ENTIRE_BRAIN_DAEMON_NO_REGISTER a "sandboxed" run still registers a real,
# restart-forever agent on your machine.
#
# What it does, in order:
#   1. builds entire-brain from this checkout
#   2. creates a scratch git repository with real content and real history: a
#      synthesised Go + Markdown tree, ~130 files over 13 commits. Synthesised
#      rather than cloned so the run stays offline, deterministic and fast.
#   3. runs `setup` and prints everything it printed
#   4. runs `setup` again, to show a re-run is idempotent
#   5. runs `status --verbose` and `doctor`
#   6. exercises the failure modes: a directory that is not a git repository, an
#      offline run, and two concurrent setups against the same repository
#   7. tears the sandbox down and PROVES nothing leaked, by diffing the machine's
#      launchd/systemd state against a snapshot taken before the run
#
# No agent tokens are spent: every setup here passes --no-backfill, and the
# watcher is never registered.
#
# Environment:
#   TRIAL_DIR=<path>   put the sandbox somewhere else (default: .trial-setup/)
#   TRIAL_KEEP=1       keep the sandbox after the run (the leak checks still run)
set -eu

repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
trial_dir=${TRIAL_DIR:-"$repo_root/.trial-setup"}
keep=${TRIAL_KEEP:-}

# The only label a job could ever be registered under in this run. Distinct from
# the real default (entire-brain-watch) so a developer's own watcher is never
# confused with this trial's.
daemon_name=entire-brain-trial

say() { printf '\n\033[1m== %s\033[0m\n' "$1"; }
note() { printf '   %s\n' "$1"; }
die() { printf 'trial-setup: %s\n' "$1" >&2; exit 1; }

command -v git >/dev/null 2>&1 || die 'git is required'
command -v go >/dev/null 2>&1 || die 'a Go toolchain is required to build the branch binary'

# Only a directory created by this invocation may be cleaned up below.
if [ -e "$trial_dir" ] || [ -L "$trial_dir" ]; then
	die "sandbox path already exists; refusing to overwrite it: $trial_dir"
fi
mkdir -- "$trial_dir" || die "could not create fresh sandbox: $trial_dir"
trial_dir=$(CDPATH='' cd -- "$trial_dir" && pwd)

sandbox_home="$trial_dir/home"
scratch_repo="$trial_dir/scratch-repo"
plain_dir="$trial_dir/not-a-repo"
mkdir -p "$sandbox_home" "$scratch_repo" "$plain_dir"

sandbox_processes() {
	ps -axo pid=,comm=,args= | awk -v root="$trial_dir" '$2 ~ /(^|\/)entire-brain$/ && index($0, root) { print $1 }'
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
		printf 'sandbox process still running; preserving %s\n' "$trial_dir" >&2
		status=1
	elif [ -z "$keep" ]; then
		rm -rf "$trial_dir" || status=1
	fi
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

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
service_state | sort >"$trial_dir/services.before"

# ------------------------------------------------------- the sandboxed binary

say "1/7  build entire-brain from this checkout"
brain_bin="$trial_dir/entire-brain"
(cd "$repo_root" && CGO_ENABLED="${CGO_ENABLED:-0}" go build -trimpath -o "$brain_bin" ./cmd/entire-brain)
note "$brain_bin"

# brain runs the binary with EVERY state path redirected into the sandbox.
# `env -i` is deliberate: an inherited variable is exactly how a "sandboxed" run
# ends up writing to a real store. Extra NAME=VALUE assignments may be passed
# before the subcommand via TRIAL_EXTRA_ENV.
brain() {
	dir=$1
	shift
	# shellcheck disable=SC2086 # TRIAL_EXTRA_ENV is a deliberate word list
	(cd "$dir" && env -i \
		PATH="$PATH" \
		TERM="${TERM:-dumb}" \
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
		${TRIAL_EXTRA_ENV:-} \
		"$brain_bin" "$@")
}

# ------------------------------------------------------ the scratch repository

say "2/7  build a scratch repository with real content and history"
(
	cd "$scratch_repo"
	git init -q -b main .
	git config user.name 'Trial Runner'
	git config user.email 'trial@example.invalid'
	git config commit.gpgsign false
	mkdir -p cmd/app internal/store internal/api docs
	cat >go.mod <<-'EOF'
		module example.invalid/trial

		go 1.26
	EOF
	cat >README.md <<-'EOF'
		# trial

		A synthetic repository built by scripts/trial-setup.sh. Nothing here is real.
	EOF
	git add -A
	git commit -qm 'chore: scaffold the module'

	commit=1
	while [ "$commit" -le 12 ]; do
		file=1
		while [ "$file" -le 10 ]; do
			pkg_dir=internal/store
			if [ $((file % 3)) -eq 0 ]; then pkg_dir=internal/api; fi
			if [ $((file % 5)) -eq 0 ]; then pkg_dir=cmd/app; fi
			pkg=$(basename "$pkg_dir")
			cat >"$pkg_dir/gen_${commit}_${file}.go" <<-EOF
				package $pkg

				// Handler${commit}x${file} is generated trial content.
				type Handler${commit}x${file} struct {
					Name string
					Size int
				}

				// Process${commit}x${file} does nothing of consequence.
				func Process${commit}x${file}(h *Handler${commit}x${file}) int {
					if h == nil {
						return 0
					}
					return h.Size + ${commit}
				}
			EOF
			file=$((file + 1))
		done
		printf '# Note %s\n\nGenerated for commit %s.\n' "$commit" "$commit" >"docs/note-${commit}.md"
		git add -A
		git commit -qm "feat: add generated batch ${commit}"
		commit=$((commit + 1))
	done
)
note "$(git -C "$scratch_repo" rev-list --count HEAD) commits, $(git -C "$scratch_repo" ls-files | wc -l | tr -d ' ') tracked files"

# ------------------------------------------------------------------- the runs

say "3/7  entire-brain setup  (first run; --no-backfill, so no token spend)"
set +e
brain "$scratch_repo" setup --daemon-name "$daemon_name" --no-backfill .
first_status=$?
set -e
note "exit=$first_status"

say "4/7  entire-brain setup  (second run -- a re-run must be idempotent)"
set +e
brain "$scratch_repo" setup --daemon-name "$daemon_name" --no-backfill .
second_status=$?
set -e
note "exit=$second_status"

say "5/7  entire-brain status --verbose, then doctor"
brain "$scratch_repo" status --verbose || true
printf '\n'
brain "$scratch_repo" doctor || true

say "6/7  failure modes"

note '--- a directory that is not a git repository (must refuse, non-zero) ---'
echo 'hello' >"$plain_dir/notes.txt"
# GIT_CEILING_DIRECTORIES stops git's upward search at the sandbox, so this is a
# genuine non-repository even when the sandbox lives inside a checkout.
set +e
TRIAL_EXTRA_ENV="GIT_CEILING_DIRECTORIES=$trial_dir" \
	brain "$plain_dir" setup --daemon-name "$daemon_name" --no-backfill .
plain_status=$?
set -e
note "exit=$plain_status (expected non-zero)"
if find "$sandbox_home/plugin/data/repos/local" -maxdepth 1 -name '*not-a-repo*' 2>/dev/null | grep -q .; then
	note 'LEAK: the non-repository still got a brain store'
	plain_left_state=1
else
	note 'no brain store was created for the non-repository'
	plain_left_state=0
fi

note '--- offline: every outbound connection refused ---'
set +e
TRIAL_EXTRA_ENV='http_proxy=http://127.0.0.1:1 https_proxy=http://127.0.0.1:1 HTTP_PROXY=http://127.0.0.1:1 HTTPS_PROXY=http://127.0.0.1:1' \
	brain "$scratch_repo" setup --daemon-name "$daemon_name" --no-backfill . >"$trial_dir/offline.log" 2>&1
offline_status=$?
set -e
tail -n 14 "$trial_dir/offline.log" || true
note "exit=$offline_status"

note '--- two concurrent setups against the same repository ---'
set +e
brain "$scratch_repo" setup --daemon-name "$daemon_name" --no-backfill . >"$trial_dir/concurrent-a.log" 2>&1 &
pid_a=$!
brain "$scratch_repo" setup --daemon-name "$daemon_name" --no-backfill . >"$trial_dir/concurrent-b.log" 2>&1 &
pid_b=$!
wait $pid_a
concurrent_a=$?
wait $pid_b
concurrent_b=$?
set -e
note "exit A=$concurrent_a  B=$concurrent_b"
grep -h -i 'lock\|already\|failed' "$trial_dir/concurrent-a.log" "$trial_dir/concurrent-b.log" 2>/dev/null | head -n 8 || true

# ------------------------------------------------------------- the leak checks

say "7/7  teardown and leak checks"

leaked=0
service_state | sort >"$trial_dir/services.after"
if ! diff -u "$trial_dir/services.before" "$trial_dir/services.after" >"$trial_dir/services.diff" 2>&1; then
	printf '   LEAK  the machine gained or lost a service:\n'
	cat "$trial_dir/services.diff"
	leaked=1
else
	printf '   ok    launchd/systemd state is byte-identical to before the run\n'
fi

if command -v launchctl >/dev/null 2>&1; then
	found=$(launchctl list 2>/dev/null | grep -i "$daemon_name" || true)
	if [ -n "$found" ]; then
		printf '   LEAK  a trial job is registered with launchd:\n%s\n' "$found"
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
		printf '   LEAK  a trial unit is registered with systemd:\n%s\n' "$found"
		leaked=1
	else
		printf '   ok    systemctl --user has no %s unit\n' "$daemon_name"
	fi
else
	printf '   skip  systemctl (not present)\n'
fi

# `setup` detaches a short-lived `memory worker --once` child on purpose, so a
# process seen the instant setup returns is expected, not a leak. Give it a
# bounded grace period; anything still alive after that IS a leak.
stray=""
waited=0
while [ "$waited" -lt 30 ]; do
	stray=$(pgrep -f -- "$brain_bin" 2>/dev/null || true)
	[ -n "$stray" ] || break
	sleep 1
	waited=$((waited + 1))
done
if [ -n "$stray" ]; then
	printf '   LEAK  a trial process outlived its grace period (%ss): %s\n' "$waited" "$stray"
	ps -o pid,ppid,command -p "$(echo "$stray" | tr '\n' ',' | sed 's/,$//')" 2>/dev/null || true
	leaked=1
elif [ "$waited" -gt 0 ]; then
	printf '   ok    the detached memory worker exited on its own after %ss\n' "$waited"
else
	printf '   ok    no trial process survived\n'
fi

# The unit file SHOULD exist, INSIDE the sandbox. Seeing it there is how you
# know the redirect worked rather than the install having silently done nothing.
if [ -d "$sandbox_home/daemon" ] && [ -n "$(ls -A "$sandbox_home/daemon" 2>/dev/null)" ]; then
	printf '   ok    the unit was written inside the sandbox: %s\n' "$(ls "$sandbox_home/daemon")"
else
	printf '   note  no unit file was written (this platform has no supported service manager)\n'
fi

if [ -n "$keep" ]; then
	printf '\n   sandbox kept at %s\n' "$trial_dir"
fi

printf '\n'
[ "$leaked" -eq 0 ] || die 'the trial leaked state outside its sandbox (see LEAK lines above)'
[ "$plain_left_state" -eq 0 ] || die 'the non-repository run left brain state behind'
if [ "$first_status" -ne 0 ] || [ "$second_status" -ne 0 ]; then
	die "setup did not succeed on a clean repository (first=$first_status second=$second_status)"
fi
[ "$concurrent_a" -eq 0 ] && [ "$concurrent_b" -eq 0 ] || die "concurrent setup failed (A=$concurrent_a B=$concurrent_b)"
[ "$offline_status" -eq 0 ] || die "setup failed with no network reachable (exit $offline_status)"
[ "$plain_status" -ne 0 ] || die 'setup accepted a directory that is not a git repository'
printf 'trial-setup: OK -- setup succeeded twice and offline, refused a non-repository, and left nothing behind.\n'
