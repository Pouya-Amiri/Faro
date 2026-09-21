# Branding sources

- `appicon-source.png` is the high-resolution source artwork.
- `banner/faro-github-banner-1500x500.png` is the repository banner used by the
  root README.

Runtime and package-specific derivatives live where their consumers require
them: `build/appicon.png` for Wails, `build/windows/icon.ico` for Windows,
`packaging/assets/Faro.icns` for macOS, the size-specific Linux hicolor icons,
and `frontend/dist/logo.png` for the embedded frontend. These are intentional
format/build-boundary copies rather than additional branding sources.
