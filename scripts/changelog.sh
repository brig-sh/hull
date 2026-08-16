#!/usr/bin/env bash
#
# Generate the changelog / release notes from the Conventional-Commits
# history with git-cliff. Runs git-cliff in a container so no host install is
# needed (works on the Apple Silicon dev box and the self-hosted runner).
#
#   scripts/changelog.sh                 # rewrite CHANGELOG.md (all tags)
#   scripts/changelog.sh --tag vX.Y.Z    # rewrite, treating unreleased as vX.Y.Z
#   scripts/changelog.sh --notes vX.Y.Z  # print notes for a single tag to stdout
#
# The release workflow uses --tag (to fold the release being cut into
# CHANGELOG.md) and --notes (to build the GitHub release body).
set -euo pipefail

IMAGE="${GIT_CLIFF_IMAGE:-orhunp/git-cliff:latest}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"

cliff() {
  docker run --rm -v "$ROOT:/app" -w /app --entrypoint git-cliff "$IMAGE" "$@"
}

case "${1:-}" in
  --notes)
    tag="${2:?usage: changelog.sh --notes <tag>}"
    # Just the section for this tag, stripped of the file header. At release
    # time HEAD is the tag commit, so --current returns that tag's section;
    # before tagging (local preview) fall back to folding unreleased into it.
    cliff --config cliff.toml --current --strip header 2>/dev/null \
      || cliff --config cliff.toml --unreleased --tag "$tag" --strip header
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
