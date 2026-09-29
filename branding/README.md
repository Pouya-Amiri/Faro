# Branding sources

- `faro-icon.svg` is the app icon source artwork: the lighthouse with its
  beams on a rounded plate. `appicon-source.png` is its 1024 px render.
- `faro-icon-desktop.svg` is the same artwork on the Windows and Linux icon
  grid: the plate fills 15/16 of the canvas and carries no drop shadow.
  `faro-icon.svg` follows the macOS grid, whose smaller plate and shadow look
  undersized next to other apps on those desktops.
- `faro-icon-small.svg` drops the beams so the lighthouse stays legible at
  tray and small-icon sizes.
- `faro-icon-fullbleed.svg` fills the whole square, for platforms that apply
  their own mask.
- `faro-icon-symbolic.svg` is a single-colour glyph (`currentColor`).
- `banner/faro-github-banner-1500x500.png` is the repository banner used by the
  root README.

Runtime and package-specific derivatives live where their consumers require
them: `build/appicon.png` (desktop grid) for Wails and Linux packages,
`build/tray-icon.png` (small icon) for the system tray,
`build/windows/icon.ico` and the size-specific Linux hicolor icons (small
icon up to 32 px, desktop grid above), and `packaging/assets/Faro.icns`
(macOS grid) for macOS. These are intentional
format/build-boundary copies rather than additional branding sources.
