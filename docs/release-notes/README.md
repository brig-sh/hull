# Release notes

Release notes are generated automatically from the Conventional-Commits
history with [git-cliff](https://git-cliff.org) -- see `cliff.toml` and
`scripts/changelog.sh`. Every release gets:

1. an entry in the top-level `CHANGELOG.md` (regenerated and committed by the
   release workflow), and
2. a GitHub release whose body is that same generated section.

## Curated highlights

The generated section is just the list of merged PRs, which is fine for most
releases. For a release with a big user-facing change, we can lead with a
short, hand-written highlights section.

Drop a file named after the tag here, e.g. `docs/release-notes/v0.1.0.md`.
The release workflow prepends it to the generated changelog for that tag. Keep
it to a few short paragraphs -- the what and the why -- and let the generated
list carry the details.

## Previewing locally

```bash
scripts/changelog.sh                 # rewrite CHANGELOG.md
scripts/changelog.sh --notes v0.1.0  # print the notes for one tag
```

Both run git-cliff in a container, so no host install is needed.
