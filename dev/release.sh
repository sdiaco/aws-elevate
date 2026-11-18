#!/usr/bin/env bash
# Publish one release built by dev/build.sh (design §12.1), in two steps with a real test
# between them:
#
#   upload  copies templates and Lambda package to v<version>/ in the release bucket of
#           every region in contract/release-buckets.json
#   github  creates the GitHub release v<version> with the executables and checksums.txt
#
# Both steps run only from a clean main whose tree passed the automatic end-to-end tests
# (build/e2e/result) and whose build matches its checksums. A published version is never
# overwritten: a fix is a new version.
#
# Usage: dev/release.sh upload <version> <aws-profile>
#        dev/release.sh github <version> <security|fix|feature>
set -euo pipefail

fail() { echo "release: $*" >&2; exit 1; }

root=$(cd "$(dirname "$0")/.." && pwd)
step=${1:-}
version=${2:-}
[ $# -eq 3 ] && { [ "$step" = upload ] || [ "$step" = github ]; } ||
	fail "usage: dev/release.sh upload <version> <aws-profile> | github <version> <security|fix|feature>"

dir="$root/build/release/v$version"
cd "$root"

[ "$(git rev-parse --abbrev-ref HEAD)" = main ] || fail "releases are published from main only"
[ -z "$(git status --porcelain)" ] || fail "the working tree has changes: commit or remove them first"
[ -f "$dir/checksums.txt" ] || fail "no build for $version: run make build VERSION=$version"
(cd "$dir" && sha256sum --quiet -c checksums.txt) || fail "the build of $version does not match its checksums.txt"

# The result names the git tree, not the commit: the merge into main makes a new commit
# with the same tree as the develop commit that was tested.
tree=$(git rev-parse 'HEAD^{tree}')
[ "$(cat build/e2e/result 2>/dev/null)" = "$tree passed" ] ||
	fail "no passing end-to-end run for this tree: run make e2e-auto first"

upload() {
	local profile=$1 region bucket key
	local regions
	regions=$(python3 -c 'import json, sys; [print(r, b) for r, b in json.load(open(sys.argv[1])).items()]' \
		contract/release-buckets.json)
	# Check every target before writing any, so a version is published everywhere or nowhere.
	while read -r region bucket; do
		for key in elevate.yaml elevate-guardrails.yaml lambda.zip; do
			if aws s3api head-object --profile "$profile" --region "$region" --bucket "$bucket" \
				--key "v$version/$key" >/dev/null 2>&1; then
				fail "s3://$bucket/v$version/$key already exists: published versions are never overwritten"
			fi
		done
	done <<<"$regions"
	while read -r region bucket; do
		for key in elevate.yaml elevate-guardrails.yaml lambda.zip; do
			aws s3api put-object --profile "$profile" --region "$region" --bucket "$bucket" \
				--key "v$version/$key" --body "$dir/$key" --if-none-match '*' >/dev/null ||
				fail "upload of s3://$bucket/v$version/$key failed: check the bucket and publish $version by hand or as a new version"
			echo "s3://$bucket/v$version/$key"
		done
	done <<<"$regions"
}

github() {
	local type=$1
	case "$type" in security | fix | feature) ;; *) fail "type must be security, fix or feature" ;; esac
	local min_infra min_client notes
	min_infra=$(sed -n 's/^var MinInfra = "\(.*\)"$/\1/p' client/internal/version/version.go)
	min_client=$(sed -n 's/^MIN_CLIENT = "\(.*\)"$/\1/p' service/src/aws_elevate/version.py)
	[ -n "$min_infra" ] && [ -n "$min_client" ] || fail "cannot read MinInfra or MIN_CLIENT from the code"
	# The client reads these header lines (client/internal/release/release.go).
	notes=$(printf 'type: %s\nminInfra: %s\nminClient: %s\n' "$type" "$min_infra" "$min_client")
	gh release create "v$version" --target "$(git rev-parse HEAD)" --title "v$version" --notes "$notes" \
		"$dir"/aws-elevate_* "$dir/checksums.txt"
}

"$step" "$3"
