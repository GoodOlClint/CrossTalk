#!/bin/sh
set -eu

NAME=crosstalk

version() {
	sed -n 's/^[[:space:]]*Version = "\(.*\)"$/\1/p' internal/mcpserver/server.go
}

asset() {
	echo "$NAME-$1-macos-universal"
}

case "${1:-}" in
check-tag)
	tag="${2:?usage: release-build.sh check-tag TAG}"
	want="v$(version)"
	[ -n "$want" ] && [ "$tag" = "$want" ] || {
		echo "tag $tag does not match mcpserver.Version (want $want)" >&2
		exit 1
	}
	;;
build)
	out="${2:?usage: release-build.sh build OUTDIR}"
	mkdir -p "$out"
	for arch in arm64 amd64; do
		CGO_ENABLED=0 GOOS=darwin GOARCH=$arch go build -trimpath -o "$out/$NAME-$arch" ./cmd/crosstalk
	done
	lipo -create -output "$out/$NAME" "$out/$NAME-arm64" "$out/$NAME-amd64"
	rm "$out/$NAME-arm64" "$out/$NAME-amd64"
	archs="$(lipo -archs "$out/$NAME")"
	echo "archs: $archs"
	[ "$archs" = "x86_64 arm64" ] || [ "$archs" = "arm64 x86_64" ] || {
		echo "unexpected archs: $archs" >&2
		exit 1
	}
	;;
package)
	out="${2:?usage: release-build.sh package OUTDIR TAG}"
	tag="${3:?usage: release-build.sh package OUTDIR TAG}"
	got="$("$out/$NAME" version)"
	[ "$got" = "$(version)" ] || {
		echo "binary reports $got, want $(version)" >&2
		exit 1
	}
	zip="$(asset "$tag").zip"
	rm -f "$out/$zip" "$out/$zip.sha256"
	ditto -c -k --keepParent "$out/$NAME" "$out/$zip"
	(cd "$out" && shasum -a 256 "$zip" >"$zip.sha256" && shasum -a 256 -c "$zip.sha256")
	;;
*)
	echo "usage: release-build.sh check-tag TAG | build OUTDIR | package OUTDIR TAG" >&2
	exit 2
	;;
esac
