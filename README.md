# DubLift

**Your video sources, with synchronized Italian audio.**

DubLift is a local HTTP addon for Stremio and Nuvio. It combines video from
your configured upstream addons with Italian and English audio from Vixsrc,
keeping the original video quality.

## ✨ Features

- Automatic audio synchronization using English reference tracks, with
  manual timing controls when you need them.
- Video passes through without re-encoding; generated audio uses AAC.
- Finite HLS streams and indexed MKV/MP4 files over HTTP(S).
- A browser dashboard for guided setup, stream preparation, audio timing,
  and playback diagnostics. Open playback URLs in VLC too.

## 🖥️ Platforms

The server runs on **Linux**, with release builds for **amd64 and arm64**.
For Android, use [DubLiftApp](#-android).

Your player must support the source video's codec. Live streams and DRM are
unsupported; see the [media requirements](docs/usage.md#media-requirements).

## 📦 Installation

Install **Go 1.26.2 or newer**, **FFmpeg**, **ffprobe** (included with FFmpeg),
and Git. Then build from source:

```sh
git clone https://github.com/joojoooo/DubLift.git
cd DubLift
go build -trimpath -o bin/dublift ./cmd/dublift
```

## 🚀 Quick start

```sh
./bin/dublift
```

1. Open [localhost:7000](http://localhost:7000).
2. Follow the guided setup to install the addon in your player and connect
   upstream addon manifest URLs. For Stremio, follow the **Stremio Addon
   Manager** link shown in the dashboard.
3. Choose a movie or episode in Stremio/Nuvio, then select a DubLift result
   marked with the Italian flag. Italian audio is the default track.
4. Use **Streams → Adjust audio timing** if synchronization needs adjusting.

Keep DubLift running during playback. For another device on your network,
check that **Settings → Public base URL** uses the server's reachable LAN
address; `localhost` refers to the device running the player.

The server listens on `0.0.0.0:7000` by default and has no built-in
authentication. Use a trusted local network. See the
[usage guide](docs/usage.md) for configuration, timing controls, and
troubleshooting.

## 📱 Android

For Android, use **DubLiftApp**, the dedicated Android application:

<https://github.com/joojoooo/DubLiftApp>

Follow its installation and device requirements in the app repository.

## 🧪 Development

Contributions are welcome. See [validation](docs/validation.md) for the
canonical checks, prerequisites, and manual playback checks. Tests use local
fixtures; live provider checks are optional.

## ☕ Support the project

If you love this project, you can support development here:</br>
[![ko-fi](https://ko-fi.com/img/githubbutton_sm.svg)](https://ko-fi.com/s/a16a8bed86)

## 📄 License

DubLift is licensed under the [GNU General Public License v3.0](LICENSE)
(`GPL-3.0-only`). Third-party dependencies retain their respective licenses.
