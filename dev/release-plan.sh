#!/usr/bin/env bash
# Decide what the release workflow publishes for the commit GITHUB_SHA just pushed to main,
# and write version, tested and type to GITHUB_OUTPUT. Nothing is written when the version
# in the code is already published: the merge brought no new release.
#
# The commit must come from a merged pull request into main with exactly one label
# release:security, release:fix or release:feature; tested is that pull request's head,
# where the check workflow must have passed.
set -euo pipefail

fail() { echo "release-plan: $*" >&2; exit 1; }

root=$(cd "$(dirname "$0")/.." && pwd)
version=$(sed -n 's/^INFRA_VERSION = "\(.*\)"$/\1/p' "$root/service/src/aws_elevate/version.py")
[ -n "$version" ] || fail "cannot read INFRA_VERSION"

if gh release view "v$version" --repo "$GITHUB_REPOSITORY" >/dev/null 2>&1; then
	echo "v$version is already published: nothing to release"
	exit 0
fi

# One line: number, head sha, release labels (comma separated, maybe none).
pr=$(gh api "repos/$GITHUB_REPOSITORY/commits/$GITHUB_SHA/pulls" --jq '[.[] | select(.merged_at != null and .base.ref == "main")][0] // empty
	| "\(.number) \(.head.sha) \([.labels[].name | select(startswith("release:"))] | join(","))"')
[ -n "$pr" ] || fail "commit $GITHUB_SHA on main does not come from a merged pull request: nothing published"
read -r number tested types <<<"$pr"
check=$(gh api "repos/$GITHUB_REPOSITORY/commits/$tested/check-runs?check_name=check" \
	--jq '.check_runs[0].conclusion // empty')
[ "$check" = success ] || fail "make check did not pass on commit $tested (${check:-not run}): nothing published"
case "$types" in
release:security | release:fix | release:feature) type=${types#release:} ;;
"") fail "pull request #$number has no release label: add release:security, release:fix or release:feature and publish by hand" ;;
*) fail "pull request #$number has more than one release label or an unknown one: $types" ;;
esac

echo "release v$version ($type) from pull request #$number, tested commit $tested"
{
	echo "version=$version"
	echo "tested=$tested"
	echo "type=$type"
} >>"$GITHUB_OUTPUT"
