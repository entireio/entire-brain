#!/bin/sh
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$repo_root"

version=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || printf 'dev')}
out_dir=${OUT_DIR:-dist/release-$version}
targets=${ENTIRE_RELEASE_TARGETS:-"darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64"}
build_tags=${BUILD_TAGS:-}

mkdir -p "$out_dir"
rm -f "$out_dir"/SHA256SUMS

build_target() {
	goos=${1%/*}
	goarch=${1#*/}
	if [ "$goos" = "$goarch" ] || [ -z "$goos" ] || [ -z "$goarch" ]; then
		printf 'invalid target %s; expected GOOS/GOARCH\n' "$1" >&2
		return 1
	fi

	bin="entire-brain"
	case "$goos" in
		windows) bin="$bin.exe" ;;
	esac

	work="$out_dir/entire-brain-$version-$goos-$goarch"
	archive="$out_dir/entire-brain-$version-$goos-$goarch.tar.gz"
	rm -rf "$work"
	mkdir -p "$work"

	printf 'building %s/%s\n' "$goos" "$goarch" >&2
	if [ -n "$build_tags" ]; then
		GOOS="$goos" GOARCH="$goarch" CGO_ENABLED="${CGO_ENABLED:-1}" \
			go build -trimpath -tags "$build_tags" \
			-ldflags "-s -w -X main.version=$version" \
			-o "$work/$bin" ./cmd/entire-brain
	else
		GOOS="$goos" GOARCH="$goarch" CGO_ENABLED="${CGO_ENABLED:-0}" \
			go build -trimpath -ldflags "-s -w -X main.version=$version" \
			-o "$work/$bin" ./cmd/entire-brain
	fi

	cp README.md LICENSE entire-plugin.yml "$work/"
	tar -C "$out_dir" -czf "$archive" "entire-brain-$version-$goos-$goarch"
	rm -rf "$work"
	shasum -a 256 "$archive" >> "$out_dir/SHA256SUMS"
}

for target in $targets; do
	build_target "$target"
done

printf 'wrote %s\n' "$out_dir" >&2
