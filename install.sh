#!/bin/sh
# Install aws-elevate: download the executable of a release for this system, check its
# SHA-256 against the release's checksums.txt and copy it to $PREFIX.
#
#   curl -fsSL https://raw.githubusercontent.com/sdiaco/aws-elevate/main/install.sh | sh
#
# VERSION=<x.y.z> installs that version instead of the latest; PREFIX=<folder> installs
# there instead of ~/.local/bin.
set -eu

releases="${AWS_ELEVATE_RELEASES:-https://github.com/sdiaco/aws-elevate/releases}"
prefix="${PREFIX:-$HOME/.local/bin}"

fail() {
	echo "aws-elevate: $*" >&2
	exit 1
}

case "$(uname -s)" in
Linux) os=linux ;;
Darwin) os=darwin ;;
*) fail "no executable for $(uname -s): only Linux and macOS are supported" ;;
esac
case "$(uname -m)" in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) fail "no executable for $(uname -m): only amd64 and arm64 are supported" ;;
esac
asset="aws-elevate_${os}_${arch}"

version="${VERSION:-}"
if [ -z "$version" ]; then
	# GitHub redirects releases/latest to releases/tag/v<version>.
	latest=$(curl -fsSLo /dev/null -w '%{url_effective}' "$releases/latest") || fail "cannot read the latest release"
	version="${latest##*/v}"
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
base="$releases/download/v$version"
curl -fsSLo "$tmp/$asset" "$base/$asset" || fail "cannot download $base/$asset"
curl -fsSLo "$tmp/checksums.txt" "$base/checksums.txt" || fail "cannot download $base/checksums.txt"

want=$(awk -v f="$asset" '$2 == f || $2 == "*" f { print $1 }' "$tmp/checksums.txt")
if command -v sha256sum >/dev/null 2>&1; then
	got=$(sha256sum "$tmp/$asset" | awk '{ print $1 }')
else
	got=$(shasum -a 256 "$tmp/$asset" | awk '{ print $1 }')
fi
if [ -z "$want" ] || [ "$want" != "$got" ]; then
	fail "checksum mismatch for $asset $version: nothing installed"
fi

mkdir -p "$prefix"
cp "$tmp/$asset" "$prefix/.aws-elevate.tmp"
chmod 0755 "$prefix/.aws-elevate.tmp"
mv -f "$prefix/.aws-elevate.tmp" "$prefix/aws-elevate"
echo "Installed aws-elevate $version in $prefix/aws-elevate."
case ":$PATH:" in
*":$prefix:"*) ;;
*) echo "Add $prefix to your PATH to run it." ;;
esac
echo "Next: aws-elevate setup"
