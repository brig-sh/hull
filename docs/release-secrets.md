# What the release workflows need

`sign-notarize.yml` signs hull and vz-runner with a Developer ID certificate,
notarizes the result with Apple and staples the ticket. None of that works
until this repository has the credentials, and they did not come across with
the code: GitHub secrets are write-only, so they cannot be copied from
`NOFireAI/urunc-macos` -- they have to be set again from the original source.

## Secrets

| secret | what it is | where it comes from |
| --- | --- | --- |
| `MACOS_CERT_P12` | Developer ID Application certificate and private key, as base64 of the `.p12` | Apple Developer portal, exported from Keychain Access |
| `MACOS_CERT_PASSWORD` | the password set when exporting that `.p12` | whoever exported it |
| `NOTARY_KEY_P8` | App Store Connect API key, as base64 of the `.p8` | App Store Connect, Users and Access, Integrations |
| `NOTARY_KEY_ID` | the key ID for that `.p8` | shown next to the key |
| `NOTARY_ISSUER_ID` | the issuer ID of the App Store Connect account | shown above the key list |
| `NOFIRE_BOT_PRIVATE_KEY` | private key of the GitHub App that creates releases | the App's settings page |

## Variables

| variable | what it is |
| --- | --- |
| `NOFIRE_BOT_APP_ID` | the GitHub App's numeric ID |
| `TELEMETRY_ENDPOINT` | where release builds report telemetry |

## Setting them

```bash
base64 -i DeveloperID.p12 | gh secret set MACOS_CERT_P12 --repo brig-sh/hull
gh secret set MACOS_CERT_PASSWORD --repo brig-sh/hull
base64 -i AuthKey_XXXXXXXX.p8 | gh secret set NOTARY_KEY_P8 --repo brig-sh/hull
gh secret set NOTARY_KEY_ID --repo brig-sh/hull
gh secret set NOTARY_ISSUER_ID --repo brig-sh/hull
gh secret set NOFIRE_BOT_PRIVATE_KEY --repo brig-sh/hull < app-private-key.pem

gh variable set NOFIRE_BOT_APP_ID --repo brig-sh/hull
gh variable set TELEMETRY_ENDPOINT --repo brig-sh/hull
```

Setting them at the organisation level instead is worth considering, since
brig will want the same certificate to notarize its own binaries.

## Two things that are not secrets

The signing job runs on `[self-hosted, macOS, ARM64]`. That runner has to be
registered with this repository, or shared with it through an organisation
runner group. A hosted macOS runner would work too, at the cost of importing
the certificate on a machine you do not control.

The GitHub App behind `NOFIRE_BOT_*` has to be installed on the `brig-sh`
organisation, not only on the old one, or the token it mints cannot create a
release here.

## Checking

```bash
gh secret list --repo brig-sh/hull
gh variable list --repo brig-sh/hull
```

Six secrets and two variables, and a tag build that reaches "notarize and
staple" without failing, is the whole test.
