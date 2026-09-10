# Code signing and entitlements

Read this if you build hull from source. If you installed a release from the
Homebrew cask, the executables are already signed correctly and you need
nothing on this page.

For the release process itself, see [releasing.md](releasing.md).

## Why signing is involved at all

hull's CLI needs no entitlement. The two runner processes do, because they are
the processes that talk to a hypervisor framework:

| Executable | Entitlement | Why |
|---|---|---|
| `hull` | none | it starts a runner; it never calls a framework |
| `vz-runner` | `com.apple.security.virtualization` | it uses Virtualization.framework |
| `hvi` | `com.apple.security.hypervisor` | it calls Hypervisor.framework directly |
| `qemu` | `com.apple.vm.networking`, and only for `--net shared` | vmnet |

Those are two different entitlements for two different frameworks. Signing one
runner does nothing for the other.

`vz-runner` ships two entitlements plists.
`Entitlements-novmnet.plist` grants only `com.apple.security.virtualization`,
and it is the one the build and the release both use. `Entitlements.plist` also
grants `com.apple.vm.networking`, which the current runner source never
exercises: there is no bridged-network path in it, and Apple NAT needs nothing
beyond the virtualization entitlement.

`hvi`'s entitlements file carries `com.apple.vm.networking` commented out, with
a note saying why: its virtio-net uses a user-space stack, a gvisor-tap relay
or a Linux tap, and never touches vmnet.

## The supported path: sign with an Apple identity

Use an *Apple Development* or *Developer ID Application* certificate. The
entitlements are then honored with **SIP enabled** and no boot-argument
changes.

```bash
security find-identity -v -p codesigning          # find yours
make sign CODESIGN_IDENTITY="Apple Development: Your Name (TEAMID)"
make codesign_verify                              # authority chain + entitlements
```

An *Apple Development* certificate signs for your own machines and is the one
to use for local work; a *Developer ID Application* certificate is what
distribution needs and requires a paid Apple Developer Program membership.
Check Apple's current developer-program terms for what your account can issue.
Prefer whichever of the two you already have.

`make macos` builds all three and signs them. `make sign` signs what is already
built and strips the quarantine attribute.

### Keychain prerequisites

`codesign` failing with `unable to build chain to self-signed root` or
`errSecInternalComponent` usually means the Apple WWDR intermediate
certificate, the Apple Root CA, or both are missing from your keychain. Install
them once from Apple's certificate authority page.

`errSecInternalComponent` part way through a build has a second cause.
`security list-keychains -d user -s` is a per-user setting, so a concurrent
build that sets its own search list evicts yours mid-run. Pin the keychain when
other jobs may share the login:

```bash
make sign CODESIGN_IDENTITY="..." CODESIGN_KEYCHAIN="$HOME/Library/Keychains/login.keychain-db"
```

## Ad-hoc signing

The build's default identity is `-`, an ad-hoc signature. An ad-hoc signature
**does** embed the entitlement. Whether macOS **honors** it is a separate
question, and the answer is decided by SIP:

| Signature | SIP enabled | SIP disabled |
|---|---|---|
| ad-hoc (`-`) | entitlement **refused** | entitlement honored |
| Apple Development or Developer ID | entitlement honored | entitlement honored |

This is not inferred. CI tests both rows on real hardware, with two paired
jobs that each assert the machine's actual state before trusting the result:

- `boot-sip` runs on a `sip-enabled` runner, asserts `csrutil status` reports
  enabled, and boots real VMs on `vz` and `hvi` from a **Developer ID** build.
- `boot-nosip` runs on a `sip-disabled` runner, asserts `csrutil status`
  reports disabled, and boots the same two backends from a plain ad-hoc
  `make macos` build with no keychain and no certificate. Its own comment
  states the rule: the entitlements are "honored without a real signature"
  with SIP off, "which is exactly the posture this job exists to exercise".

Two corrections follow, and earlier versions of hull's own documentation got
both slightly wrong:

- **It is a SIP claim, not an AMFI claim.** The Makefile describes ad-hoc as
  needing AMFI disabled, and `hvi`'s tooling says ad-hoc is simply enough. What
  the project actually tests is SIP, in both directions. `hvi`'s claim holds
  only with SIP off.
- **Turning SIP off does not remove the need for the entitlement.** SIP off
  relaxes *who may hold* the entitlement, not whether the framework looks for
  one. The SIP-disabled job still verifies that `vz-runner` carries
  `com.apple.security.virtualization`, because a Makefile regression that
  dropped it would otherwise pass there.

What to do with that:

1. **Use an Apple identity.** It works with SIP enabled and removes the
   question.
2. **Do not disable SIP to make ad-hoc work.** That turns off a protection for
   the whole machine, permanently, to work around a signing choice. hull's own
   troubleshooting page says the same: for an entitlement error, re-sign or
   check sibling placement first, and do not reach for SIP or AMFI changes.

Ad-hoc still has a legitimate place. It is the default because it is what lint
and build jobs need, and it is what a SIP-disabled test machine deliberately
exercises. It is not the posture to develop in on your own Mac.

Note that `codesign --verify` checks the **seal**, not the signer, and the
entitlement is embedded either way. So neither `--verify` nor
`codesign -d --entitlements` notices that a signature is ad-hoc. That is why
`make release_tarball` asserts the Authority string separately, and refuses to
package unless `CODESIGN_IDENTITY` starts with `Developer ID Application:` or
`ALLOW_ADHOC_TARBALL=1` is set.

## Does copying a binary break its signature?

This is worth getting right, because the two cases behave differently and
conflating them leads to unnecessary re-signing.

### A standalone signed executable survives a copy

A Mach-O's signature is embedded in the file. Copy it and the signature and its
entitlements come along.

Verified on macOS 26.5, Apple M3, SIP enabled, against this repository's own
ad-hoc signed `dist/vz-runner` and `dist/hvi`:

```console
$ codesign -dvvv dist/vz-runner
CodeDirectory v=20500 size=787 flags=0x10002(adhoc,runtime) location=embedded
Signature=adhoc

$ cp dist/vz-runner /tmp/vzr-copy
$ codesign --verify --verbose=2 /tmp/vzr-copy
/tmp/vzr-copy: valid on disk
/tmp/vzr-copy: satisfies its Designated Requirement
```

The entitlement survives too: `codesign -d --entitlements -` reports
`com.apple.security.virtualization` on both the original and the copy, and
`com.apple.security.hypervisor` on both copies of `hvi`.

So a plain `cp` of a standalone runner does **not** strip entitlements.

`install -m0755` and a tar round trip behave the same way. Measured against
`dist/vz-runner` on this repository: the original and all three copies report
the identical `CDHash=1bdec45b8abaaee6bfe9be9f6928963f3aaed438`, all three keep
`com.apple.security.virtualization`, and all three satisfy
`codesign --verify --strict`.

One consequence: the Makefile's stated reason for re-signing inside the
`install` target, that `install` "copies rather than preserving the
signature's file identity", does not hold. The re-sign is harmless and costs
nothing, but `install(1)` preserved the CDHash and the entitlement exactly.

### Only a bundle's *main* executable breaks when extracted

This is the real hazard, and it is narrower than "copying breaks signatures".

A bundle's **main** executable, the one named by `CFBundleExecutable`, is
sealed to the bundle's `Info.plist` and resource directory. Extracted as a lone
file its signature is invalid, reported as
`invalid Info.plist (plist or signature have been modified)`, and AMFI kills it
at exec with `Killed: 9` on any Mac that enforces code signing.

A **nested, non-main** executable in the same bundle is not sealed that way. It
stays valid with its entitlement intact when extracted.

Both halves were measured on a throwaway bundle built from this repository's
own binaries, signing the nested binary first and the bundle last, the order
the Makefile uses:

```console
$ codesign --verify --strict extracted/hull-main       # CFBundleExecutable
extracted/hull-main: invalid Info.plist (plist or signature have been modified)

$ codesign --verify --strict extracted/vz-nested       # nested binary
extracted/vz-nested: valid on disk
extracted/vz-nested: satisfies its Designated Requirement
```

The nested copy still reported `com.apple.security.virtualization`.

For `hull.app` that means `hull` is the one that genuinely needs re-signing
after extraction. `vz-runner` and `hvi` ride inside `Contents/MacOS` as nested
binaries and survive extraction on their own.

`make release_tarball` re-signs the staged binaries as **standalone** code
anyway and gates on `codesign --verify --strict` for each. For `hull` that is
required. For the two runners it is belt and braces.

### What actually blocks a downloaded binary

A `com.apple.quarantine` extended attribute, set when the file was downloaded,
AirDropped or file-shared. That is not a signature problem:

```bash
xattr -dr com.apple.quarantine ./hull ./vz-runner ./hvi
```

`make sign` does this for you. Locally built binaries are never quarantined,
and a local `cp` does not add the attribute.

## Signing QEMU for vmnet

`--net shared` on the `qemu` backend goes through `vmnet`, which requires the
caller to be root or to hold `com.apple.vm.networking`.

That entitlement is **managed**. Apple must grant it to your team, and it needs
a provisioning profile. A plain Apple Development certificate cannot sign for
it. hull never signs QEMU, and CI runs the QEMU tests against a stock Homebrew
build through the gateway.

Use `--gateway-sock` instead. It needs no entitlement and no root, and works on
every backend. See
[troubleshooting.md](troubleshooting.md#cannot-create-vmnet-interface-general-failure)
for the other options.

## Distribution: Developer ID, notarization, stapling

Local development signing and distribution signing are different jobs. Everything
above is about making a build run on your own machine. Shipping it to someone
else needs three more things:

1. **A Developer ID Application certificate**, not an Apple Development one.
   Apple Development signs for your own machines; Developer ID signs for
   others'.
2. **Notarization.** Submit the artifact to Apple, which scans it and issues a
   ticket.
3. **Stapling.** Attach the ticket to the artifact so Gatekeeper can check it
   with no network.

```bash
make dmg CODESIGN_IDENTITY="Developer ID Application: Your Name (TEAMID)"
```

That target builds the installer image, notarizes it through a `notarytool`
keychain profile, and staples the ticket. Sign with the hardened runtime and a
timestamp, using the same entitlements plists as a local build.

Note what notarization does and does not assert. A stapled ticket says Apple
scanned the file. It does not say who published it. That is what the release's
cosign signature is for, and a careful installer checks both. See
[install.md](install.md#verify-a-release).

### Getting a Developer ID certificate

A Developer ID Application certificate needs a paid Apple Developer Program
membership, and for an organization, verification of the organization itself.
Create the certificate in the developer portal or through Xcode, then export it
with its private key from Keychain Access under My Certificates.

The release workflow's secrets and how to produce each one are in
[releasing.md](releasing.md#repository-secrets).

## Verifying what you signed

```bash
make codesign_verify
```

That prints the authority chain and entitlements of all three binaries. To check
one by hand:

```bash
codesign -dvvv --entitlements - ./vz-runner
codesign --verify --strict --verbose=2 ./vz-runner
```

The release workflow does the same and then asserts that
`com.apple.security.virtualization` is present on the shipped `vz-runner`, so a
build that silently lost the entitlement fails rather than shipping.

Note that `hvi` must be re-signed after every rebuild. Rebuilding produces a new
Mach-O, and a new Mach-O carries no signature until you apply one. `make macos`
handles this by signing after it builds.
