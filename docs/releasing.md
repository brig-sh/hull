# Releasing hull

This page is for maintainers. Cutting a release needs repository secrets and an
Apple Developer account. Nothing here is required to use or build hull.

For installing a release, see [install.md](install.md). For signing a local
build, see [signing.md](signing.md).

## The two workflows

| Workflow | Trigger | What it signs with |
|---|---|---|
| `.github/workflows/ci.yml` | every push and pull request | ad-hoc signatures for lint and build; the end-to-end job signs with the Developer ID so entitlements hold under SIP |
| `.github/workflows/release.yml` | a `v*` tag, or `workflow_dispatch` with an existing tag | Developer ID Application, with notarization and stapling |

## Cutting a release

1. Bump `VERSION`. The tag must match it.
2. Commit, tag and push:

   ```bash
   git tag "v$(cat VERSION)"
   git push origin "v$(cat VERSION)"
   ```

3. Wait for `release.yml`. It signs and notarizes `vz-runner` and `hvi`, then
   runs goreleaser, which builds and notarizes `hull`, packs all three into
   `hull-<version>-arm64.tar.gz`, writes `checksums.txt` with a keyless cosign
   signature, generates an SBOM for the archive, and drafts the GitHub release
   with notes from the pull request titles.
4. A separate `dmg` job builds `hull.dmg`, notarizes and staples it, signs it
   with cosign, and attaches `hull.dmg`, `hull.dmg.sig` and `hull.dmg.pem` to
   the release.
5. Publish the draft release.
6. Update the tap. For a stable tag, goreleaser opens a pull request against
   `brig-sh/homebrew-brig` with a new `Casks/hull.rb`; review and merge it. For
   a release candidate the cask is not published, because `skip_upload` is set
   to `auto`. If the candidate should be installable, edit `version` and
   `sha256` in the tap's `Casks/hull.rb` by hand.
7. Regenerate the changelog. `release.yml` does not touch `CHANGELOG.md`:

   ```bash
   scripts/changelog.sh          # or: git-cliff --config cliff.toml -o CHANGELOG.md
   ```

   Commit the result.

## The `dmg` job needs a graphical session

`create-dmg` lays out the installer window by driving Finder, so the job checks
for an Aqua session and stops with a named reason when there is none. A
headless runner cannot build the DMG. If that job fails while the rest of the
release succeeds, the archive and its signature are still published and the
release is usable; only the DMG is missing.

## Repository secrets

| Secret | Content | How to produce it |
|---|---|---|
| `MACOS_CERT_P12` | base64 of the Developer ID Application `.p12`, certificate and private key | `base64 -i DeveloperID.p12`, exported from Keychain Access under My Certificates |
| `MACOS_CERT_PASSWORD` | the password protecting that `.p12` | chosen at export time |
| `NOTARY_KEY_P8` | App Store Connect API private key, the plain `.p8` contents | App Store Connect, Users and Access, Integrations, App Store Connect API |
| `NOTARY_KEY_ID` | that key's ID | shown next to the key |
| `NOTARY_ISSUER_ID` | the issuer UUID | shown on the same page |
| `NOFIRE_BOT_PRIVATE_KEY` | private key of the GitHub App that mints the tap token | the App's settings page |
| `HOMEBREW_TAP_GITHUB_TOKEN` | fallback token for the tap, used when the App token is unavailable | a fine-grained PAT with contents and pull-requests write on `homebrew-brig` |

CI reads two more secrets: `HULL_ASSETS_TOKEN`, which pulls the boot bundle on
the end-to-end runner, and `CODECOV_TOKEN`, for coverage uploads on pushes to
`main`.

`TELEMETRY_ENDPOINT` is a repository **variable**, not a secret. Left empty,
the telemetry client stays inert, which is the default for local and fork
builds. See [telemetry.md](telemetry.md).

Set a secret with:

```bash
gh secret set MACOS_CERT_P12 --repo brig-sh/hull < cert.p12.b64
```

## `release_tarball` refuses an ad-hoc build

`make release_tarball` checks the signing identity before it packages anything
and refuses unless `CODESIGN_IDENTITY` starts with `Developer ID Application:`,
or `ALLOW_ADHOC_TARBALL=1` is set to override it. The guard exists because
`codesign --verify` checks the seal rather than the signer, so an ad-hoc
signature passes every other check while macOS would refuse `vz-runner`'s
entitlement under SIP on the installing machine. See
[signing.md](signing.md#ad-hoc-signing).

## Why goreleaser does not build everything

`vz-runner` is Swift. goreleaser cannot build it, and importing a prebuilt
binary is a goreleaser Pro feature. The release workflow therefore builds,
signs and notarizes `vz-runner` and `hvi` before goreleaser runs, and they
travel in the archive as extra files.

The same restriction applies to the DMG. Notarizing an app bundle inside a DMG
is Pro-only, which is why a separate job handles it rather than goreleaser.

## Ephemeral keychains

Both signing jobs create a keychain under `$RUNNER_TEMP` and delete it in an
`always()` step. Pin the keychain explicitly when signing, with
`CODESIGN_KEYCHAIN`. The Makefile documents why: `security list-keychains -d
user -s` is a per-user setting, so a concurrent build that sets its own search
list evicts this one, and `codesign` then fails with
`errSecInternalComponent` part way through a run.
