#!/usr/bin/env bash
#
# Generate the changelog / release notes from the Conventional-Commits
# history with git-cliff. Runs git-cliff in a container so no host install is
# needed (works on the Apple Silicon dev box and the self-hosted runner).
#
#   scripts/changelog.sh                 # rewrite CHANGELOG.md (all tags)
#   scripts/changelog.sh --tag vX.Y.Z    # rewrite, treating unreleased as vX.Y.Z
#   scripts/changelog.sh --notes vX.Y.Z  # print a release's notes to stdout
#
# CHANGELOG.md comes from cliff.toml and nothing in CI regenerates it, so it
# is committed by hand. --notes renders from cliff-notes.toml, the config the
# release workflow publishes, so it shows what a release will say.
# Without docker, `git-cliff --config cliff.toml -o CHANGELOG.md` is the same
# thing with a host install (brew install git-cliff).
set -euo pipefail

IMAGE="${GIT_CLIFF_IMAGE:-orhunp/git-cliff:latest}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"

cliff() {
  docker run --rm -e GITHUB_TOKEN -v "$ROOT:/app" -w /app --entrypoint git-cliff "$IMAGE" "$@"
}

case "${1:-}" in
  --notes)
    tag="${2:?usage: changelog.sh --notes <tag>}"
    # At a tag, --current renders that tag's notes; before tagging (a local
    # preview) fall back to folding unreleased into it. The contributor lists
    # use GITHUB_TOKEN when it is set and the anonymous API limit otherwise.
    cliff --config cliff-notes.toml --current 2>/dev/null \
      || cliff --config cliff-notes.toml --unreleased --tag "$tag"
    ;;
  --tag)
    tag="${2:?usage: changelog.sh --tag <tag>}"
    cliff --config cliff.toml --tag "$tag" --output CHANGELOG.md
    echo "CHANGELOG.md updated (unreleased folded into $tag)" >&2
    ;;
  "")
    cliff --config cliff.toml --output CHANGELOG.md
    echo "CHANGELOG.md updated" >&2
    ;;
  *)
    echo "usage: changelog.sh [--tag <tag> | --notes <tag>]" >&2
    exit 2
    ;;
esac
