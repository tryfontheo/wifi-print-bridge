#!/bin/bash
set -euo pipefail

root=$(cd -- "$(/usr/bin/dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
go_command=${GO:-go}
identity=${CODESIGN_IDENTITY:--}

if [[ $(/usr/bin/uname -s) != Darwin ]]; then
    printf '%s\n' 'Build the installer on macOS with the Xcode Command Line Tools.' >&2
    exit 1
fi
if ! command -v "$go_command" >/dev/null 2>&1; then
    printf '%s\n' 'Install Go 1.26 or set GO to its absolute executable path.' >&2
    exit 1
fi
go_command=$(command -v "$go_command")
export GOTOOLCHAIN=local
go_version=$("$go_command" env GOVERSION)
case $go_version in
    go1.26|go1.26.*) ;;
    *) printf 'Go 1.26 is required for macOS 12 compatibility. Found %s.\n' "$go_version" >&2; exit 1 ;;
esac

clang=$(/usr/bin/xcrun --sdk macosx --find clang)
sdk=$(/usr/bin/xcrun --sdk macosx --show-sdk-path)
output="$root/build"
destination="$output/Install Wi-Fi Print Bridge.app"
/bin/mkdir -p "$output"
scratch=$(/usr/bin/mktemp -d "$output/.work.XXXXXXXX")
app="$scratch/Install Wi-Fi Print Bridge.app"
resources="$app/Contents/Resources"
payload="$resources/bridge"
previous="$scratch/previous.app"
trap '[[ ! -d "$scratch" ]] || /bin/rm -R -- "$scratch"' EXIT

/bin/mkdir -p "$app/Contents/MacOS" "$payload/licenses" "$scratch/InstallerIcon.iconset"
/usr/bin/install -m 0644 "$root/installer/Info.plist" "$app/Contents/Info.plist"
/usr/bin/install -m 0644 "$root/README.md" "$payload/README.md"
/usr/bin/install -m 0644 "$root/LICENSE" "$payload/LICENSE"
/usr/bin/install -m 0644 "$root/bridge/install.sh" "$root/bridge/service.sh" "$payload"
/usr/bin/install -m 0644 "$root/licenses/"*.txt "$payload/licenses"

/usr/bin/xcrun --sdk macosx swift "$root/installer/MakeIcon.swift" "$scratch/icon.png"
for size in 16 32 128 256 512; do
    /usr/bin/sips -z "$size" "$size" "$scratch/icon.png" \
        --out "$scratch/InstallerIcon.iconset/icon_${size}x${size}.png" >/dev/null
    /usr/bin/sips -z "$((size * 2))" "$((size * 2))" "$scratch/icon.png" \
        --out "$scratch/InstallerIcon.iconset/icon_${size}x${size}@2x.png" >/dev/null
done
/usr/bin/iconutil -c icns "$scratch/InstallerIcon.iconset" -o "$resources/InstallerIcon.icns"

build_slice() (
    local go_arch=$1 mac_arch=$2
    cd "$root/bridge/src"
    GOOS=darwin GOARCH="$go_arch" CGO_ENABLED=1 CC="$clang" SDKROOT="$sdk" MACOSX_DEPLOYMENT_TARGET=12.0 \
        "$go_command" build -buildvcs=false -mod=readonly -trimpath -ldflags '-s -w' \
        -o "$scratch/bridge.$mac_arch" .
    /usr/bin/xcrun --sdk macosx swiftc -sdk "$sdk" -parse-as-library -O \
        -target "$mac_arch-apple-macosx12.0" -o "$scratch/installer.$mac_arch" \
        "$root/installer/Installer.swift"
)

build_slice arm64 arm64
build_slice amd64 x86_64
/usr/bin/lipo -create "$scratch/bridge.arm64" "$scratch/bridge.x86_64" -output "$payload/print-bridge"
/usr/bin/lipo -create "$scratch/installer.arm64" "$scratch/installer.x86_64" \
    -output "$app/Contents/MacOS/PrintBridgeInstaller"
/bin/chmod 0755 "$payload/print-bridge" "$app/Contents/MacOS/PrintBridgeInstaller"

sign_flags=(--force --sign "$identity")
if [[ $identity != - ]]; then
    sign_flags+=(--options runtime --timestamp)
fi
/usr/bin/plutil -lint "$app/Contents/Info.plist" >&2
/usr/bin/codesign "${sign_flags[@]}" --identifier local.wifiprintbridge.bridge "$payload/print-bridge"
/usr/bin/codesign "${sign_flags[@]}" --identifier local.wifiprintbridge.installer "$app"
/usr/bin/codesign --verify --strict --all-architectures "$payload/print-bridge"
/usr/bin/codesign --verify --deep --strict --all-architectures "$app"
for binary in "$payload/print-bridge" "$app/Contents/MacOS/PrintBridgeInstaller"; do
    architectures=$(/usr/bin/lipo -archs "$binary")
    if [[ " $architectures " != *' arm64 '* || " $architectures " != *' x86_64 '* ]]; then
        printf 'Both Mac architectures are required. Found %s in %s.\n' "$architectures" "$binary" >&2
        exit 1
    fi
done

if [[ -e $destination || -L $destination ]]; then
    /bin/mv "$destination" "$previous"
fi
if ! /bin/mv "$app" "$destination"; then
    if [[ -e $previous || -L $previous ]]; then
        /bin/mv "$previous" "$destination" || trap - EXIT
    fi
    exit 1
fi
printf '%s\n' "$destination"
