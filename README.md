![Faro — Watch Together](branding/banner/faro-github-banner-1500x500.png)

# Faro

Faro keeps video playback in sync for people watching together. Host a room,
send an invite, and use the media player you already have.

[Download the latest release](https://github.com/Pouya-Amiri/Faro/releases/latest)

## Downloads

Desktop clients and standalone servers are published as separate files:

| Platform | Desktop client | Standalone server |
| --- | --- | --- |
| Windows x86-64 | Installer or portable ZIP | ZIP |
| macOS Apple Silicon | DMG | tar.gz |
| macOS Intel | DMG | tar.gz |
| Linux x86-64 | DEB, RPM, or AppImage | tar.gz |

Each release includes `SHA256SUMS.txt`. Linux AppImages may need to be made
executable with `chmod +x Faro-*.AppImage` before they can be opened.
The AppImage intentionally uses the distribution's GTK 4 and WebKitGTK 6
runtime.
Install `gtk4` and `webkitgtk6.0` on Fedora, or `libgtk-4-1` and
`libwebkitgtk-6.0-4` on Ubuntu/Debian. The DEB and RPM declare these
dependencies automatically.
Public-repository builds also receive GitHub/Sigstore provenance attestations,
which can be checked with `gh attestation verify <file> -R Pouya-Amiri/Faro`.

Faro's Windows and macOS builds are not signed with trusted distribution
identities because the project does not currently have Apple Developer ID and
notarization credentials or a Windows Authenticode certificate. Windows
SmartScreen and macOS Gatekeeper may therefore show an unknown-developer
warning. Verify the file against `SHA256SUMS.txt` first,
then use **More info → Run anyway** on Windows or **Open Anyway** in macOS
**Privacy & Security** if you trust the download. The macOS app is still
ad-hoc signed so its bundle integrity can be checked locally.

## What Faro does

- Synchronizes play, pause, seeking, and playback position
- Supports shared playlists, chat, and moderated rooms
- Works with local files, HTTP(S) streams, and YouTube links
- Matches local copies without sharing anyone's filesystem paths
- Can securely stream a selected file directly to friends who do not have it
- Reconnects automatically after temporary network interruptions

## Get started

1. Install a supported media player and download Faro for your platform.
2. Open Faro and choose **Host room → Easy · peer to peer**.
3. Copy the room invite and send it to the people you want to join.
4. Your friends paste the invite into **Join room** and select their player.

Easy hosting needs no account or port forwarding. The host must keep Faro open
for the duration of the room. Treat invite links as private: anyone who has a
valid invite can enter its room.

Add a local file or URL to the shared playlist once everyone is connected.
Friends with the same file can locate their own copy. If somebody is missing
the file, a participant who has it can choose **Share file**; Faro offers only
that file, never a folder or media library.

## Supported players

| Player | Platform |
| --- | --- |
| [mpv](https://mpv.io/) | Windows, macOS, Linux |
| [mpv.net](https://github.com/mpvnet-player/mpv.net) | Windows |
| [IINA](https://iina.io/) | macOS |
| [Memento](https://github.com/ripose-jp/Memento) | Windows, macOS, Linux |
| [VLC](https://www.videolan.org/vlc/) | Windows, macOS, Linux |

The macOS build requires macOS 12 Monterey or later.

Faro detects standard player installations automatically. Custom executable
paths and launch arguments can be configured in the connection screen. YouTube
playback requires [`yt-dlp`](https://github.com/yt-dlp/yt-dlp) and a supported
JavaScript runtime installed on the computer. Faro detects common install
locations and configures the runtime automatically; `Deno 2.3+` is recommended.
Restart Faro after installing either dependency.

## Privacy and security

Faro connections use TLS 1.3 from the first byte, and invites pin the host's
exact certificate. Easy hosting uses a high-entropy join token and establishes
peer connectivity through Tailcat without requiring a Tailscale account. There
is no plaintext or click-through certificate fallback.

Local paths are never sent to the room server. File availability is represented
by content-derived fingerprints, and shared file transfers are limited to the
file explicitly offered by a participant.

For networks where peer-to-peer connectivity is unavailable, choose
**Advanced hosting** to use a reachable Faro server and explicit network
settings.

## Build from source

Building the desktop app requires Go 1.27, Wails v3, and the platform WebView
development libraries required by Wails. On Linux, Faro uses GTK4 and
WebKitGTK 6.

```sh
wails3 build
```

Create a platform-native package with:

```sh
wails3 package
```

Run the test suite with `go test -race ./...`.

## Acknowledgments

Peer-to-peer connectivity is powered by
[Tailcat](https://github.com/tailscale/tailcat). Faro's desktop application is
built with [Wails](https://wails.io/).

## License

Faro is released under the [Mozilla Public License 2.0](LICENSE). Bundled
components retain the licenses listed in [Third-Party Notices](THIRD_PARTY_NOTICES.txt).
