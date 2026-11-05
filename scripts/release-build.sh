#!/bin/sh
set -eu

NAME=crosstalk

version() {
	sed -n 's/^[[:space:]]*Version = "\(.*\)"$/\1/p' internal/mcpserver/server.go
}

asset() {
	echo "$NAME-$1-macos-universal"
}

check_tag() {
	tag="$1"
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
}

sha_of() {
	h="$(awk 'NR == 1 { print $1 }' "$1")" || exit 1
	case "$h" in
	*[!0-9a-f]* | "") h="" ;;
	esac
	[ "${#h}" = 64 ] || {
		echo "no sha256 in $1" >&2
		exit 1
	}
	echo "$h"
}

case "${1:-}" in
check-tag)
	check_tag "${2:?usage: release-build.sh check-tag TAG}"
	;;
is-final)
	tag="${2:?usage: release-build.sh is-final TAG}"
	check_tag "$tag"
	if [ "$tag" = "v$(version)" ]; then echo true; else echo false; fi
	;;
formula)
	usage="usage: release-build.sh formula TAG DIST DIST_LINUX"
	tag="${2:?$usage}"
	dist="${3:?$usage}"
	distlinux="${4:?$usage}"
	case "$tag" in
	v*[!0-9.]* | v*..* | v*. | v.* | v) bad=1 ;;
	v*.*.*) bad= ;;
	*) bad=1 ;;
	esac
	[ -z "$bad" ] || {
		echo "formula needs a final tag vX.Y.Z, got $tag" >&2
		exit 1
	}
	mac="$NAME-$tag-macos-universal.zip"
	amd="$NAME-$tag-linux-amd64.tar.gz"
	arm="$NAME-$tag-linux-arm64.tar.gz"
	mac_sha="$(sha_of "$dist/$mac.sha256")"
	amd_sha="$(sha_of "$distlinux/$amd.sha256")"
	arm_sha="$(sha_of "$distlinux/$arm.sha256")"
	base="https://github.com/GoodOlClint/CrossTalk/releases/download/$tag"
	sed -e "s|@BASE@|$base|g" \
		-e "s|@MAC@|$mac|" -e "s|@MAC_SHA@|$mac_sha|" \
		-e "s|@AMD@|$amd|" -e "s|@AMD_SHA@|$amd_sha|" \
		-e "s|@ARM@|$arm|" -e "s|@ARM_SHA@|$arm_sha|" <<'EOF'
class Crosstalk < Formula
  desc "Message other running agent sessions (Claude Code, Codex) by name"
  homepage "https://getcrosstalk.dev"
  license "MIT"

  on_macos do
    on_arm do
      url "@BASE@/@MAC@"
      sha256 "@MAC_SHA@"
    end
    on_intel do
      url "@BASE@/@MAC@"
      sha256 "@MAC_SHA@"
    end
  end

  on_linux do
    on_intel do
      url "@BASE@/@AMD@"
      sha256 "@AMD_SHA@"
    end
    on_arm do
      url "@BASE@/@ARM@"
      sha256 "@ARM_SHA@"
    end
  end

  def install
    bin.install "crosstalk"
  end

  def caveats
    <<~EOS
      Register CrossTalk with Claude Code and Codex:
        crosstalk install
      Run it again after `brew upgrade crosstalk` so the plugin matches the binary.
      Before `brew uninstall crosstalk`, run:
        crosstalk uninstall
      Restart running Claude and Codex sessions after either.
    EOS
  end

  test do
    assert_equal version.to_s, shell_output("#{bin}/crosstalk version").strip
  end
end
EOF
	;;
check-identity)
	keychain="${2:?usage: release-build.sh check-identity KEYCHAIN}"
	matches="$(security find-identity -v -p codesigning "$keychain" 2>/dev/null | awk -F '"' 'NF > 2 && $2 == ENVIRON["APPLE_CODESIGN_IDENTITY"] { n++ } END { print n + 0 }')"
	[ "$matches" = 1 ] || {
		echo "expected exactly 1 valid codesigning identity matching APPLE_CODESIGN_IDENTITY in the temporary keychain, found $matches" >&2
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
	(cd "$out" && ditto -c -k --norsrc "$NAME" "$zip")
	[ "$(unzip -Z1 "$out/$zip")" = "$NAME" ] || {
		echo "zip must contain exactly $NAME" >&2
		exit 1
	}
	(cd "$out" && shasum -a 256 "$zip" >"$zip.sha256" && shasum -a 256 -c "$zip.sha256")
	;;
build-linux)
	out="${2:?usage: release-build.sh build-linux OUTDIR}"
	mkdir -p "$out"
	for arch in amd64 arm64; do
		CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -o "$out/$NAME-linux-$arch" ./cmd/crosstalk
	done
	;;
package-linux)
	out="${2:?usage: release-build.sh package-linux OUTDIR TAG}"
	tag="${3:?usage: release-build.sh package-linux OUTDIR TAG}"
	for arch in amd64 arm64; do
		tarball="$NAME-$tag-linux-$arch.tar.gz"
		stage="$(mktemp -d)"
		cp "$out/$NAME-linux-$arch" "$stage/$NAME"
		chmod 755 "$stage/$NAME"
		rm -f "$out/$tarball" "$out/$tarball.sha256"
		COPYFILE_DISABLE=1 tar -C "$stage" -czf "$out/$tarball" "$NAME"
		rm -rf "$stage"
		[ "$(tar -tzf "$out/$tarball")" = "$NAME" ] || {
			echo "tarball must contain exactly $NAME" >&2
			exit 1
		}
		(cd "$out" && shasum -a 256 "$tarball" >"$tarball.sha256" && shasum -a 256 -c "$tarball.sha256")
	done
	;;
*)
	echo "usage: release-build.sh check-tag TAG | is-final TAG | formula TAG DIST DIST_LINUX | check-identity KEYCHAIN | build OUTDIR | package OUTDIR TAG | build-linux OUTDIR | package-linux OUTDIR TAG" >&2
	exit 2
	;;
esac
