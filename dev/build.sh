#!/usr/bin/env bash
# Build the files of one release in $OUT/v$VERSION: the Lambda package, the
# two templates, one client executable per system and architecture, and checksums.txt.
# Template, Lambda and client share one version: the build stops when the code says
# otherwise, so a release never ships a template that points at another version's code.
#
# Usage: dev/build.sh <version> <out-dir>
set -euo pipefail

fail() { echo "build: $*" >&2; exit 1; }

[ $# -eq 2 ] || fail "usage: dev/build.sh <version> <out-dir>"
version=$1
out=$2
[[ $version =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] ||
	fail "version $version is not MAJOR.MINOR.PATCH"

root=$(cd "$(dirname "$0")/.." && pwd)

check() { # name, value found in the code
	[ "$2" = "$version" ] || fail "$1 is ${2:-missing}, not $version: update it before the release"
}
check "INFRA_VERSION in service/src/aws_elevate/version.py" \
	"$(sed -n 's/^INFRA_VERSION = "\(.*\)"$/\1/p' "$root/service/src/aws_elevate/version.py")"
check "the CodeKey default in infra/elevate.yaml" \
	"$(awk '/^  CodeKey:/ { f = 1 } f && /Default:/ { print $2; exit }' "$root/infra/elevate.yaml" |
		sed -n 's|^v\(.*\)/lambda\.zip$|\1|p')"
check "Client in client/internal/version/version.go" \
	"$(sed -n 's/^var Client = "\(.*\)"$/\1/p' "$root/client/internal/version/version.go")"

dir="$out/v$version"
rm -rf "$dir"
mkdir -p "$dir"

(cd "$root/service" && uv run --quiet python -c "import sys, pathlib; sys.path.insert(0, 'tests/e2e'); \
	import harness; pathlib.Path(sys.argv[1]).write_bytes(harness.package())" "$dir/lambda.zip")
cp "$root/infra/elevate.yaml" "$root/infra/elevate-guardrails.yaml" "$dir/"

# -trimpath and -buildvcs=false keep paths and git state out of the executable: the same
# source and Go version give the same bytes.
for os in linux darwin; do
	for arch in amd64 arm64; do
		(cd "$root/client" && CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -buildvcs=false \
			-ldflags "-s -w -X github.com/sdiaco/aws-elevate/client/internal/version.Client=$version" \
			-o "$dir/aws-elevate_${os}_${arch}" ./cmd/aws-elevate)
	done
done

(cd "$dir" && sha256sum aws-elevate_* lambda.zip elevate.yaml elevate-guardrails.yaml >checksums.txt)
echo "$dir"
