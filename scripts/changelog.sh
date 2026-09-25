#!/usr/bin/env bash
#
# Generate the changelog / release notes from the Conventional-Commits
# history with git-cliff. Runs git-cliff in a container so no host install is
# needed (works on the Apple Silicon dev box and the self-hosted runner).
#
#   scripts/changelog.sh                 # rewrite CHANGELOG.md (all tags)
#   scripts/changelog.sh --tag vX.Y.Z    # rewrite, treating unreleased as vX.Y.Z
#   scripts/changelog.sh --notes vX.Y.Z  # print a release's notes to stdout
#                                        # (at a tag: that tag's, whatever X.Y.Z)
#
# CHANGELOG.md comes from cliff.toml and nothing in CI regenerates it, so it
# is committed by hand. --notes renders from cliff-notes.toml, the config the
# release workflow publishes, so it shows what a release will say.
# Without docker, `git-cliff --config cliff.toml -o CHANGELOG.md` is the same
# thing with a host install (brew install git-cliff).
set -euo pipefail

# Pinned by digest: the container gets the tree, and for --notes a token, so
# it is not left to whatever the tag points at today. The version is the one
# release.yml's action installs, so a preview renders what the release will.
IMAGE="${GIT_CLIFF_IMAGE:-orhunp/git-cliff:2.14.1@sha256:8c1fe7f1351c9fc6288e503f33e448fb108fc73bfb1a93011332eec6b1daf2c7}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"

# Only --notes hands the container GITHUB_TOKEN: the CHANGELOG.md render has
# no [remote.github] and needs none.
cliff() {
  docker run --rm -v "$ROOT:/app" -w /app --entrypoint git-cliff "$IMAGE" "$@"
}
cliff_notes() {
  docker run --rm -e GITHUB_TOKEN -v "$ROOT:/app" -w /app --entrypoint git-cliff "$IMAGE" "$@"
}

case "${1:-}" in
  --notes)
    tag="${2:?usage: changelog.sh --notes <tag>}"
    # git-cliff falls back to its built-in template when the config is
    # missing, and exits 0; from a checkout older than the file that would
    # print the wrong notes without a word.
    [ -f "$ROOT/cliff-notes.toml" ] || { echo "cliff-notes.toml not found in $ROOT" >&2; exit 1; }
    # At a tag, --current renders that tag's notes, whatever <tag> says; the
    # argument is for a preview before tagging, where unreleased is folded
    # into it. Which of the two is decided up front rather than by trying
    # --current and hiding its stderr, which would turn any failure into the
    # other render. The contributor lists use GITHUB_TOKEN when it is set
    # and the anonymous API limit otherwise.
    if git -C "$ROOT" describe --exact-match --tags --match 'v[0-9]*' >/dev/null 2>&1; then
      notes=$(cliff_notes --config cliff-notes.toml --current)
    else
      notes=$(cliff_notes --config cliff-notes.toml --unreleased --tag "$tag")
    fi
    # A range of nothing but mechanics renders as the compare link alone,
    # which is not a release's notes; release.yml refuses the same.
    if ! printf '%s\n' "$notes" | grep -q '^## '; then
      echo "no sections rendered for $tag:" >&2
      printf '%s\n' "$notes" >&2
      exit 1
    fi
    printf '%s\n' "$notes"
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
