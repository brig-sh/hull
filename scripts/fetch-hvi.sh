#!/usr/bin/env bash
# Fetches the hvi release that hvi.pin names, verifies it and installs it.
#
# Usage: scripts/fetch-hvi.sh [out-dir]   (default: build)
#
# SHA256SUMS must carry a cosign bundle signed by hvi-vmm's tag-release
# workflow for the pinned tag. The archive must match both SHA256SUMS and the
# sha in hvi.pin, which is the trust root. The binary is used as shipped: it
# is signed with the Developer ID and notarized, and a new signature would
# drop the notarization ticket. Verified archives are cached in
# .cache/hvi/<version>/.
#
# HVI_BIN=/path/to/hvi skips the fetch and copies that binary instead, eg. a
# local cargo build. Nothing is verified then.
#
# COSIGN names the cosign binary (default: cosign). It must be v3 or later,
# since the bundle is in the cosign v3 format.
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
out=${1:-$root/build}

die() {
	echo "fetch-hvi: $*" >&2
	exit 1
}

mkdir -p "$out"

if [ -n "${HVI_BIN:-}" ]; then
	[ -f "$HVI_BIN" ] && [ -x "$HVI_BIN" ] || die "HVI_BIN=$HVI_BIN is not an executable file"
	if [ "$HVI_BIN" -ef "$out/hvi" ]; then
		echo "fetch-hvi: using $out/hvi as is (HVI_BIN), not the pinned release"
	else
		install -m 0755 "$HVI_BIN" "$out/hvi"
		echo "fetch-hvi: copied $HVI_BIN to $out/hvi (HVI_BIN), not the pinned release"
	fi
	exit 0
fi

# shellcheck source-path=SCRIPTDIR source=../hvi.pin
. "$root/hvi.pin"
: "${HVI_VERSION:?hvi.pin sets no HVI_VERSION}"
: "${HVI_COMMIT:?hvi.pin sets no HVI_COMMIT}"
: "${HVI_SHA256_AARCH64_APPLE_DARWIN:?hvi.pin sets no HVI_SHA256_AARCH64_APPLE_DARWIN}"
pin=$HVI_SHA256_AARCH64_APPLE_DARWIN

case "$pin" in
TBD*) die "hvi.pin has no sha for hvi $HVI_VERSION yet ($pin). Fill it in from the release's SHA256SUMS, or set HVI_BIN to a local build." ;;
esac
case "$pin" in
*[!0-9a-f]*) die "HVI_SHA256_AARCH64_APPLE_DARWIN=$pin is not a lowercase sha256" ;;
esac
[ "${#pin}" -eq 64 ] || die "HVI_SHA256_AARCH64_APPLE_DARWIN=$pin is not a sha256"

cosign=${COSIGN:-cosign}
command -v "$cosign" >/dev/null || die "$cosign not found; hvi's bundle needs cosign v3 or later"
cosign_version=$("$cosign" version 2>&1 | sed -n 's/^GitVersion:[[:space:]]*v\{0,1\}//p')
case "${cosign_version%%.*}" in
'' | *[!0-9]*) die "cannot read the version of $cosign" ;;
esac
[ "${cosign_version%%.*}" -ge 3 ] || die "$cosign is v$cosign_version; hvi's bundle needs cosign v3 or later"

target=aarch64-apple-darwin
tag=v$HVI_VERSION
archive=hvi-$HVI_VERSION-$target.tar.gz
base=https://github.com/brig-sh/hvi-vmm/releases/download/$tag
identity="^https://github\\.com/brig-sh/hvi-vmm/\\.github/workflows/tag-release\\.yml@refs/tags/$(printf '%s' "$tag" | sed 's/\./\\./g')\$"
issuer=https://token.actions.githubusercontent.com
cache=$root/.cache/hvi/$HVI_VERSION

sha256() {
	shasum -a 256 "$1" | cut -d' ' -f1
}

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# A cached archive is reused only when it still has the pinned sha. The
# signature is checked again either way.
if [ -f "$cache/$archive" ] && [ "$(sha256 "$cache/$archive")" = "$pin" ] &&
	[ -f "$cache/SHA256SUMS" ] && [ -f "$cache/SHA256SUMS.cosign.bundle" ]; then
	src=$cache
	echo "fetch-hvi: using the cached $archive"
else
	src=$tmp/download
	mkdir "$src"
	for f in "$archive" SHA256SUMS SHA256SUMS.cosign.bundle; do
		curl -fsSL --retry 3 -o "$src/$f" "$base/$f" || die "cannot download $base/$f"
	done
fi

"$cosign" verify-blob \
	--bundle "$src/SHA256SUMS.cosign.bundle" \
	--certificate-identity-regexp "$identity" \
	--certificate-oidc-issuer "$issuer" \
	"$src/SHA256SUMS" ||
	die "SHA256SUMS of $tag is not signed by brig-sh/hvi-vmm's tag-release workflow"

listed=$(awk -v f="$archive" '$2 == f || $2 == "*" f { print $1 }' "$src/SHA256SUMS")
[ -n "$listed" ] || die "SHA256SUMS lists no $archive"
got=$(sha256 "$src/$archive")
[ "$got" = "$listed" ] || die "$archive is $got, SHA256SUMS says $listed"
[ "$got" = "$pin" ] || die "$archive is $got, hvi.pin says $pin"

tar -C "$tmp" -xzf "$src/$archive" hvi manifest.json

rev=$(plutil -extract revision raw -o - "$tmp/manifest.json") || die "manifest.json has no revision"
case "$rev" in
"$HVI_COMMIT"*) ;;
*) die "manifest revision $rev is not $HVI_COMMIT" ;;
esac
signed=$(plutil -extract signed raw -o - "$tmp/manifest.json") || die "manifest.json has no signed field"
[ "$signed" = true ] || die "the manifest says $archive was not signed with the Developer ID"

# Captured first: grep -q would close the pipe early and codesign would die
# of SIGPIPE.
codesign --verify --strict "$tmp/hvi" || die "the signature of hvi does not verify"
signature=$(codesign -dvv "$tmp/hvi" 2>&1)
printf '%s\n' "$signature" | grep -q '^Authority=Developer ID Application' ||
	die "hvi is not signed with a Developer ID"
entitlements=$(codesign -d --entitlements - --xml "$tmp/hvi" 2>/dev/null)
printf '%s\n' "$entitlements" | grep -q 'com\.apple\.security\.hypervisor' ||
	die "hvi lacks the com.apple.security.hypervisor entitlement"

if [ "$src" != "$cache" ]; then
	rm -rf "$cache"
	mkdir -p "$(dirname "$cache")"
	mv "$src" "$cache"
fi

install -m 0755 "$tmp/hvi" "$out/hvi"
echo "fetch-hvi: $out/hvi is hvi $HVI_VERSION ($rev), verified"
