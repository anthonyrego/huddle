#!/bin/sh
# Builds build/Huddle.app around build/huddle and signs it, so the
# Microphone (and later Camera) permission belongs to the app and survives
# rebuilds.
#
# macOS keeps a permission against the app's designated requirement. An
# ad-hoc signature (the linker's default, and what this does without SIGN)
# makes that requirement the cdhash, which changes with every build, so
# every build would ask again; a certificate signature makes it
# "identifier + certificate leaf", which doesn't. SIGN is the certificate's
# SHA-1 (two certificates can share a name); keep it in local.mk.
#
# With a certificate the app gets the hardened runtime, which notarizing
# needs: the mic and camera then need entitlements (scripts/entitlements.plist),
# and every library the app loads must carry the same team's signature, so
# SDL ships in Contents/Frameworks, signed here, instead of being unpacked
# from the binary at run time.
#
# Copied from reel (github.com/anthonyrego/reel scripts/make-app.sh), plus
# the bundled SDL and the hardened runtime.
set -eu
bin=${1:-build/huddle}
app=${2:-build/Huddle.app}
sign=${SIGN:--}
bundle_id=${BUNDLE_ID:-com.anthonyrego.huddle}
version=$(git describe --always --dirty 2>/dev/null || echo dev)
here=$(dirname "$0")
icon=$here/../assets/icon/AppIcon.png

rm -rf "$app"
mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources" "$app/Contents/Frameworks"
cp "$bin" "$app/Contents/MacOS/huddle"

# The licenses: the app carries code adapted from LiveKit's SDK (Apache 2.0).
cp "$here/../LICENSE" "$here/../NOTICE" "$here/../LICENSES/Apache-2.0.txt" "$app/Contents/Resources/"

# SDL from the go-sdl3 module the binary was built against.
sdl_dir=$(go list -m -f '{{.Dir}}' github.com/Zyko0/go-sdl3)
arch=$(go env GOARCH)
gunzip -c "$sdl_dir/bin/binsdl/assets/sdl_${arch}.dylib.gz" >"$app/Contents/Frameworks/libSDL3.dylib"

icon_key=""
if [ -f "$icon" ]; then
	# AppIcon.icns from the 1024 px PNG.
	set_dir=$(mktemp -d)/AppIcon.iconset
	mkdir -p "$set_dir"
	for s in 16 32 128 256 512; do
		sips -z $s $s "$icon" --out "$set_dir/icon_${s}x${s}.png" >/dev/null
		sips -z $((s * 2)) $((s * 2)) "$icon" --out "$set_dir/icon_${s}x${s}@2x.png" >/dev/null
	done
	iconutil -c icns "$set_dir" -o "$app/Contents/Resources/AppIcon.icns"
	rm -rf "$(dirname "$set_dir")"
	icon_key="<key>CFBundleIconFile</key><string>AppIcon</string>"
fi

cat >"$app/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleName</key><string>Huddle</string>
	<key>CFBundleDisplayName</key><string>Huddle</string>
	<key>CFBundleIdentifier</key><string>$bundle_id</string>
	<key>CFBundleExecutable</key><string>huddle</string>
	$icon_key
	<key>CFBundlePackageType</key><string>APPL</string>
	<key>CFBundleShortVersionString</key><string>$version</string>
	<key>CFBundleVersion</key><string>$version</string>
	<key>LSMinimumSystemVersion</key><string>14.0</string>
	<key>LSApplicationCategoryType</key><string>public.app-category.productivity</string>
	<key>NSHighResolutionCapable</key><true/>
	<key>NSSupportsAutomaticGraphicsSwitching</key><true/>
	<key>NSMicrophoneUsageDescription</key><string>Huddle sends your voice to the people in your meeting.</string>
	<key>NSCameraUsageDescription</key><string>Huddle shows your camera to the people in your meeting.</string>
	<key>NSCameraUseContinuityCameraDeviceType</key><true/>
</dict>
</plist>
PLIST

if [ "$sign" = "-" ]; then
	# Ad hoc: runs here, but macOS asks for the mic again after each build.
	codesign --force --sign - "$app/Contents/Frameworks/libSDL3.dylib"
	codesign --force --sign - --identifier "$bundle_id" "$app"
else
	codesign --force --timestamp --options runtime --sign "$sign" "$app/Contents/Frameworks/libSDL3.dylib"
	codesign --force --timestamp --options runtime --entitlements "$here/entitlements.plist" \
		--sign "$sign" --identifier "$bundle_id" "$app"
fi
# Finder and the Dock cache icons by bundle; a newer mtime makes them look again.
touch "$app"
echo "$app"
