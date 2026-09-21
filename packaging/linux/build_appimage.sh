#!/usr/bin/env bash
set -euo pipefail

version="${1:?usage: build_appimage.sh VERSION ARCH}"
arch="${2:?usage: build_appimage.sh VERSION ARCH}"

case "$arch" in
  x86_64)
    tool_sha256="ed4ce84f0d9caff66f50bcca6ff6f35aae54ce8135408b3fa33abfc3cb384eb0"
    runtime_sha256="1cc49bcf1e2ccd593c379adb17c9f85a36d619088296504de95b1d06215aebbf"
    ;;
  aarch64)
    tool_sha256="f0837e7448a0c1e4e650a93bb3e85802546e60654ef287576f46c71c126a9158"
    runtime_sha256="7d5d772b7c32f0c84caf0a452a3072a5709027d7eac5856feb89a7a7a8881372"
    ;;
  *)
    echo "unsupported AppImage architecture: $arch" >&2
    exit 1
    ;;
esac

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
build_dir="$root_dir/build/linux/appimage"
app_dir="$build_dir/Faro.AppDir"
tool_version="1.9.1"
tool="$build_dir/appimagetool-$arch.AppImage"
runtime="$build_dir/runtime-$arch"
output="$root_dir/dist_actions/Faro-$version-linux-$arch.AppImage"

mkdir -p "$build_dir" "$root_dir/dist_actions"
if [[ ! -f "$tool" ]]; then
  curl --fail --location --retry 3 --silent --show-error --output "$tool" \
    "https://github.com/AppImage/appimagetool/releases/download/$tool_version/appimagetool-$arch.AppImage"
fi
printf '%s  %s\n' "$tool_sha256" "$tool" | sha256sum --check --status
chmod 0755 "$tool"
if [[ ! -f "$runtime" ]]; then
  curl --fail --location --retry 3 --silent --show-error --output "$runtime" \
    "https://github.com/AppImage/type2-runtime/releases/download/continuous/runtime-$arch"
fi
printf '%s  %s\n' "$runtime_sha256" "$runtime" | sha256sum --check --status

rm -rf "$app_dir"
install -Dm755 "$root_dir/build/bin/faro" "$app_dir/usr/bin/faro"
install -Dm755 "$root_dir/packaging/linux/AppRun" "$app_dir/AppRun"
install -Dm644 "$root_dir/packaging/linux/faro.desktop" \
  "$app_dir/usr/share/applications/io.github.pouya_amiri.Faro.desktop"
cp "$app_dir/usr/share/applications/io.github.pouya_amiri.Faro.desktop" \
  "$app_dir/io.github.pouya_amiri.Faro.desktop"
install -Dm644 "$root_dir/build/appicon.png" "$app_dir/faro.png"
install -Dm644 "$root_dir/packaging/linux/io.github.pouya_amiri.Faro.metainfo.xml" \
  "$app_dir/usr/share/metainfo/io.github.pouya_amiri.Faro.appdata.xml"
install -Dm644 "$root_dir/LICENSE" "$app_dir/usr/share/licenses/faro/LICENSE"
install -Dm644 "$root_dir/THIRD_PARTY_NOTICES.txt" "$app_dir/usr/share/licenses/faro/THIRD_PARTY_NOTICES.txt"
ln -s faro.png "$app_dir/.DirIcon"

# Do not introduce a bundled GUI stack here. WebKitGTK compiles its helper
# directory into libwebkitgtk, so an Ubuntu library cannot locate helpers on
# Fedora even if linuxdeploy copied them into the AppDir.
if find "$app_dir" -type f \( -name 'libwebkitgtk*.so*' -o -name 'libjavascriptcoregtk*.so*' -o -name 'libgtk-*.so*' \) | grep -q .; then
  echo "refusing to package a distribution-specific GTK/WebKit library" >&2
  exit 1
fi

VERSION="$version" ARCH="$arch" "$tool" --appimage-extract-and-run \
  --runtime-file "$runtime" "$app_dir" "$output"

extract_dir="$(mktemp -d)"
trap 'rm -rf "$extract_dir"' EXIT
(
  cd "$extract_dir"
  "$output" --appimage-extract >/dev/null
)
if find "$extract_dir/squashfs-root" -type f \( -name 'libwebkitgtk*.so*' -o -name 'libjavascriptcoregtk*.so*' -o -name 'libgtk-*.so*' \) | grep -q .; then
  echo "AppImage unexpectedly contains a distribution-specific GTK/WebKit library" >&2
  exit 1
fi
