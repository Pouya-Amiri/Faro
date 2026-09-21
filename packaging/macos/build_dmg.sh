#!/usr/bin/env bash
set -euo pipefail

# Script to package Faro.app into a polished DMG on macOS
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

VERSION="${1:-$(go run ./cmd/faro-version)}"
ARCH="${2:-$(uname -m)}"
APP_PATH="${REPO_ROOT}/build/bin/Faro.app"
OUT_DIR="${REPO_ROOT}/dist_actions"
DMG_NAME="Faro-${VERSION}-macos-${ARCH}.dmg"

mkdir -p "${OUT_DIR}"

if [ ! -d "${APP_PATH}" ]; then
  echo "Error: ${APP_PATH} not found. Please run wails3 package first." >&2
  exit 1
fi

echo "==> Building DMG for Faro ${VERSION} (${ARCH})..."

actual_arch="$(file "${APP_PATH}/Contents/MacOS/Faro")"
if [[ "${actual_arch}" != *"${ARCH}"* ]]; then
  echo "Error: app binary does not contain expected architecture ${ARCH}: ${actual_arch}" >&2
  exit 1
fi

# Ad-hoc sign the complete local bundle. Release signing can replace this
# identity in a protected distribution workflow.
codesign --force --deep --sign - "${APP_PATH}"
codesign --verify --deep --strict --verbose=2 "${APP_PATH}"

# Check for create-dmg
if command -v create-dmg >/dev/null 2>&1; then
  # Remove existing DMG if present
  rm -f "${OUT_DIR}/${DMG_NAME}"

  create-dmg \
    --volname "Faro" \
    --volicon "${REPO_ROOT}/packaging/assets/Faro.icns" \
    --window-pos 200 120 \
    --window-size 660 400 \
    --icon-size 100 \
    --icon "Faro.app" 180 170 \
    --hide-extension "Faro.app" \
    --app-drop-link 480 170 \
    "${OUT_DIR}/${DMG_NAME}" \
    "${APP_PATH}"
else
  echo "Warning: create-dmg not found. Falling back to hdiutil..."
  DMG_STAGE="$(mktemp -d)"
  trap 'rm -rf "${DMG_STAGE}"' EXIT
  cp -R "${APP_PATH}" "${DMG_STAGE}/Faro.app"
  ln -s /Applications "${DMG_STAGE}/Applications"
  hdiutil create -volname "Faro" -srcfolder "${DMG_STAGE}" -ov -format UDZO "${OUT_DIR}/${DMG_NAME}"
fi

echo "==> Successfully created ${OUT_DIR}/${DMG_NAME}"
