#!/usr/bin/env bash
# Open, or update, the tap pull request for one stable hull cask.
#
# The release workflow runs this for a stable tag, after it renders
# Casks/hull.rb. The cask goes to the tap as a pull request on the branch
# hull-<version>, so somebody reads it before people install it.
#
# A re-run for the same tag reuses that branch and its open pull request. It
# keeps the commits on the branch, and writes the rendered cask over
# Casks/hull.rb: the file is generated, so a fix to it belongs in the
# renderer. It adds a commit only when the rendered cask differs from the
# branch's.
#
# Usage: open-tap-pr.sh <owner/tap-repo> <version> <cask.rb>
# GH_TOKEN must be able to push a branch to the tap and open a pull request.
set -euo pipefail

repo=${1:?usage: open-tap-pr.sh <owner/tap-repo> <version> <cask.rb>}
version=${2:?usage: open-tap-pr.sh <owner/tap-repo> <version> <cask.rb>}
cask=${3:?usage: open-tap-pr.sh <owner/tap-repo> <version> <cask.rb>}
: "${GH_TOKEN:?GH_TOKEN is not set}"
case "$cask" in /*) ;; *) cask="$PWD/$cask" ;; esac
branch="hull-$version"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
# Not shallow: counting the branch's commits ahead of main, and pushing a
# fast-forward over a merged branch, both need the history. The tap is small.
git clone --quiet \
	"https://x-access-token:${GH_TOKEN}@github.com/${repo}.git" "$work/tap"
cd "$work/tap"

if cmp -s "$cask" Casks/hull.rb; then
	echo "main in $repo already has this cask"
	exit 0
fi

# A branch with no commits ahead of main was merged already, and has nothing
# to open a pull request from. It starts again from main: the push is then a
# fast-forward, and the pull request has the new commit to show.
ahead=0
if git show-ref --verify --quiet "refs/remotes/origin/$branch"; then
	ahead=$(git rev-list --count "origin/main..origin/$branch")
fi
if [ "$ahead" != 0 ]; then
	git checkout --quiet -B "$branch" "origin/$branch"
else
	git checkout --quiet -B "$branch" origin/main
fi

cp "$cask" Casks/hull.rb
git add Casks/hull.rb
if git diff --cached --quiet; then
	echo "$branch already has this cask"
else
	# -s: the tap's DCO check runs on every pull request.
	git -c user.name=brig-sh-bot -c user.email=bot@brig.sh \
		commit --quiet -s -m "chore(cask): hull $version"
	git push --quiet origin "$branch"
	echo "pushed $branch"
fi

pr=$(gh pr list -R "$repo" --head "$branch" --state open --json url --jq '.[0].url // empty')
if [ -n "$pr" ]; then
	echo "the pull request is open already: $pr"
	exit 0
fi
gh pr create -R "$repo" --base main --head "$branch" \
	--title "chore(cask): hull $version" \
	--body "Casks/hull.rb for hull v$version, rendered by the release workflow in brig-sh/hull with scripts/render-cask.py.

Merge it after the release is published. Until then the release is a draft, its archive cannot be downloaded anonymously, and the checksum check here fails. Re-run that check once the release is out."
