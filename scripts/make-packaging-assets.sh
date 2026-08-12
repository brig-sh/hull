#!/bin/bash
# Regenerates packaging/AppIcon.icns and packaging/dmg-background.tiff from
# the NOFire and urunc logo sources. Requires: imagemagick, iconutil, tiffutil.
# The generated assets are committed so regular builds don't need imagemagick.
set -euo pipefail
cd "$(dirname "$0")/.."

# urunc mark: the official CNCF artwork color icon
# (github.com/cncf/artwork/projects/urunc), vendored under packaging/sources.
NOFIRE_LOGO="${NOFIRE_LOGO:-packaging/sources/nofire-logo-black.png}"
URUNC_LOGO="${URUNC_LOGO:-packaging/sources/urunc-icon-color.png}"
OUT=packaging
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

# --- pieces ---------------------------------------------------------------
# NOFire flame mark = the leading squircle of the wordmark (left 256x256)
magick "$NOFIRE_LOGO" -crop 256x256+0+0 +repage "$TMP/nofire-mark.png"

# --- icns helper -----------------------------------------------------------
make_icns() { # $1 = 1024px source png, $2 = output icns
  local iconset dbl
  iconset="$TMP/$(basename "$2" .icns).iconset"
  mkdir -p "$iconset"
  for size in 16 32 128 256 512; do
    sips -z $size $size "$1" --out "$iconset/icon_${size}x${size}.png" >/dev/null
    dbl=$((size * 2))
    sips -z $dbl $dbl "$1" --out "$iconset/icon_${size}x${size}@2x.png" >/dev/null
  done
  iconutil -c icns "$iconset" -o "$2"
}

# --- application icon: the urunc CNCF mark on a macOS squircle -------------
magick \( -size 1024x1024 gradient:'#ffffff'-'#e7e7ee' \) \
  \( -size 1024x1024 xc:black -fill white -draw "roundrectangle 0,0,1023,1023,185,185" \) \
  -alpha off -compose CopyOpacity -composite "$TMP/bg.png"
magick "$TMP/bg.png" \
  \( "$URUNC_LOGO" -resize 700x700 \) -gravity center -composite \
  "$TMP/appicon_1024.png"
make_icns "$TMP/appicon_1024.png" "$OUT/AppIcon.icns"

# --- volume icon: the NOFire flame mark (global branding on the disk) ------
# The mark is already a black squircle with the flame; scale it onto a
# transparent canvas so Finder shows it as-is.
magick -size 1024x1024 xc:none \
  \( "$TMP/nofire-mark.png" -resize 880x880 \) -gravity center -composite \
  "$TMP/volicon_1024.png"
make_icns "$TMP/volicon_1024.png" "$OUT/VolumeIcon.icns"

# --- DMG background (660x420 window, HiDPI tiff) --------------------------
# icon slots at (180,280) app and (480,280) Applications; arrow between them
magick -size 1320x840 gradient:'#fbfbfd'-'#ebebf2' \
  \( "$NOFIRE_LOGO" -resize x110 \) -gravity north -geometry +0+90 -composite \
  -font /System/Library/Fonts/HelveticaNeue.ttc -pointsize 40 -fill '#3a3a42' -gravity north -annotate +0+290 'Drag urunc to the Applications folder' \
  -fill '#9a9aa4' -draw "path 'M 545,568 L 715,568 L 715,540 L 795,584 L 715,628 L 715,600 L 545,600 Z'" \
  "$TMP/bg@2x.png"
magick "$TMP/bg@2x.png" -resize 50% "$TMP/bg.png"
tiffutil -cathidpicheck "$TMP/bg.png" "$TMP/bg@2x.png" -out "$OUT/dmg-background.tiff" >/dev/null 2>&1

echo "wrote $OUT/AppIcon.icns, $OUT/VolumeIcon.icns and $OUT/dmg-background.tiff"
