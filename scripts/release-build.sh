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
	ver="$(version)"
	[ -n "$ver" ] || {
		echo "cannot read mcpserver.Version" >&2
		exit 1
	}
	case "$tag" in
	"v$ver") ;;
	"v$ver"-rc.*[!0-9]* | "v$ver"-rc. | "v$ver"-rc.0[0-9]*)
		echo "tag $tag is not v$ver or v$ver-rc.N" >&2
		exit 1
		;;
	"v$ver"-rc.*) ;;
	*)
		echo "tag $tag is not v$ver or v$ver-rc.N" >&2
		exit 1
		;;
	esac
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
	(cd "$out" && ditto -c -k --norsrc "$NAME" "$zip")
	[ "$(unzip -Z1 "$out/$zip")" = "$NAME" ] || {
		echo "zip must contain exactly $NAME" >&2
		exit 1
	}
	(cd "$out" && shasum -a 256 "$zip" >"$zip.sha256" && shasum -a 256 -c "$zip.sha256")
	;;
*)
	echo "usage: release-build.sh check-tag TAG | build OUTDIR | package OUTDIR TAG" >&2
	exit 2
	;;
esac
