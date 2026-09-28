# Branding sources

- `faro-icon.svg` is the app icon source artwork: the lighthouse with its
  beams on a rounded plate. `appicon-source.png` is its 1024 px render.
- `faro-icon-small.svg` drops the beams so the lighthouse stays legible at
  tray and small-icon sizes.
- `faro-icon-fullbleed.svg` fills the whole square, for platforms that apply
  their own mask.
- `faro-icon-symbolic.svg` is a single-colour glyph (`currentColor`).
- `banner/faro-github-banner-1500x500.png` is the repository banner used by the
  root README.

Runtime and package-specific derivatives live where their consumers require
them: `build/appicon.png` for Wails, `build/tray-icon.png` (from the small
icon) for the system tray, `build/windows/icon.ico` for Windows,
`packaging/assets/Faro.icns` for macOS and the size-specific Linux hicolor
icons, whose 16 to 32 px sizes also use the small icon. These are intentional
format/build-boundary copies rather than additional branding sources.
