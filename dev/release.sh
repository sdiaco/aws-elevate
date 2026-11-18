#!/usr/bin/env bash
# Publish one release built by dev/build.sh, in two steps:
#
#   upload  copies templates and Lambda package to v<version>/ in the release bucket of
#           every region in contract/release-buckets.json
#   github  creates the GitHub release v<version> with the executables and checksums.txt
#
# Both steps run only from a clean main, with a build that matches its checksums, and
# when <tested-commit> has the same git tree as HEAD and a successful "e2e" commit status
# (posted by make e2e-auto or make e2e-status after the end-to-end tests on real AWS).
# A published version is never overwritten: a fix is a new version.
#
# Usage: dev/release.sh upload <version> <tested-commit> [aws-profile]
#        dev/release.sh github <version> <tested-commit> <security|fix|feature>
# AWS credentials come from the profile, or from the environment when it is omitted.
set -euo pipefail

fail() { echo "release: $*" >&2; exit 1; }

root=$(cd "$(dirname "$0")/.." && pwd)
step=${1:-}
version=${2:-}
tested=${3:-}
case "$step:$#" in
upload:3 | upload:4 | github:4) ;;
*) fail "usage: dev/release.sh upload <version> <tested-commit> [aws-profile] | github <version> <tested-commit> <security|fix|feature>" ;;
esac

[ "$step" = upload ] || case "$4" in security | fix | feature) ;; *) fail "type must be security, fix or feature" ;; esac

dir="$root/build/release/v$version"
cd "$root"

# In GitHub Actions the checkout may be detached; GITHUB_REF names the branch.
[ "$(git branch --show-current)" = main ] || [ "${GITHUB_REF:-}" = refs/heads/main ] ||
	fail "releases are published from main only"
[ -z "$(git status --porcelain)" ] || fail "the working tree has changes: commit or remove them first"
[ -f "$dir/checksums.txt" ] || fail "no build for $version: run make build VERSION=$version"
(cd "$dir" && sha256sum --quiet -c checksums.txt) || fail "the build of $version does not match its checksums.txt"

# The tested commit is the head of the release pull request; merging it into main makes a
# new commit with the same tree. Its tree comes from GitHub: the pull request branch is
# deleted on merge, so a fresh clone may not have the commit.
repo=${GITHUB_REPOSITORY:-$(gh repo view --json nameWithOwner -q .nameWithOwner)}
[ "$(gh api "repos/$repo/commits/$tested" --jq .commit.tree.sha 2>/dev/null)" = "$(git rev-parse 'HEAD^{tree}')" ] ||
	fail "commit $tested does not have the tree of HEAD: the end-to-end run did not test this code"
state=$(gh api "repos/$repo/commits/$tested/status" --jq '.statuses[] | select(.context == "e2e") | .state')
[ "$state" = success ] ||
	fail "commit $tested has no successful e2e status (${state:-none}): run make e2e-auto, or the manual checks and make e2e-status"

upload() {
	local profile=() region bucket key regions
	[ -n "${1:-}" ] && profile=(--profile "$1")
	regions=$(python3 -c 'import json, sys; [print(r, b) for r, b in json.load(open(sys.argv[1])).items()]' \
		contract/release-buckets.json)
	# Check every target before writing any, so a version is published everywhere or nowhere.
	while read -r region bucket; do
		for key in elevate.yaml elevate-guardrails.yaml lambda.zip; do
			if aws s3api head-object "${profile[@]}" --region "$region" --bucket "$bucket" \
				--key "v$version/$key" >/dev/null 2>&1; then
				fail "s3://$bucket/v$version/$key already exists: published versions are never overwritten"
			fi
		done
	done <<<"$regions"
	while read -r region bucket; do
		for key in elevate.yaml elevate-guardrails.yaml lambda.zip; do
			aws s3api put-object "${profile[@]}" --region "$region" --bucket "$bucket" \
				--key "v$version/$key" --body "$dir/$key" --if-none-match '*' >/dev/null ||
				fail "upload of s3://$bucket/v$version/$key failed: check the bucket and publish $version by hand or as a new version"
			echo "s3://$bucket/v$version/$key"
		done
	done <<<"$regions"
}

github() {
	local type=$1
	local min_infra min_client notes
	min_infra=$(sed -n 's/^var MinInfra = "\(.*\)"$/\1/p' client/internal/version/version.go)
	min_client=$(sed -n 's/^MIN_CLIENT = "\(.*\)"$/\1/p' service/src/aws_elevate/version.py)
	[ -n "$min_infra" ] && [ -n "$min_client" ] || fail "cannot read MinInfra or MIN_CLIENT from the code"
	# The client reads these header lines (client/internal/release/release.go).
	notes=$(printf 'type: %s\nminInfra: %s\nminClient: %s\n' "$type" "$min_infra" "$min_client")
	gh release create "v$version" --target "$(git rev-parse HEAD)" --title "v$version" --notes "$notes" \
		"$dir"/aws-elevate_* "$dir/checksums.txt"
}

"$step" "${4:-}"
