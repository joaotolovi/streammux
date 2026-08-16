<div align="center">

# 🎬 StreamMux

**Best picture. Your language.**

StreamMux is a [Stremio](https://www.stremio.com/) addon that combines the
**highest-quality video** from one source with the **audio in your language**
from another — remuxed on the fly without re-encoding the video and delivered
as a single stream.

![Go](https://img.shields.io/badge/Go-1.25-00ADD8?logo=go&logoColor=white)
![React](https://img.shields.io/badge/React-19-61DAFB?logo=react&logoColor=white)
![Tailwind](https://img.shields.io/badge/Tailwind-4-06B6D4?logo=tailwindcss&logoColor=white)
![License](https://img.shields.io/badge/License-MIT-green)
![PRs welcome](https://img.shields.io/badge/PRs-welcome-brightgreen)

</div>

---

## Why StreamMux?

The best picture quality (4K REMUX, HDR, Dolby Vision, Atmos) is almost always
available in English. Dubbed versions in your language usually come with much
lower video quality.

**StreamMux fixes that.** It fetches streams from your configured addons, picks
the best video (usually English 4K) and the best audio in your preferred
language (usually dubbed), and remuxes them together with FFmpeg — so you get
**4K picture with audio in your language**.

For each title it returns two options:

- 🔊 **Dubbed** — the best video + the best audio in your language. If the
  video and audio are already in the same stream, a lightweight single-input
  remux selects the right track; otherwise both sources are combined by FFmpeg.
- 🎞️ **Subtitled** — the best video (usually English), always a direct link.

## Features

- **Smart source matching** — analyzes resolution, quality, encode, HDR/visual
  tags, audio tags and channels to rank every candidate.
- **Conservative release matching** — cross-source plans require matching
  duration, frame rate and edition metadata, avoiding combinations such as an
  extended cut's video with a theatrical cut's audio.
- **Multiaudio support** — detects the correct audio track for your language
  inside multiaudio files via FFprobe (e.g. `por`, `pt-BR`, `dubbed`, flags 🇵🇹/🇧🇷).
- **Debrid fallback** — when an addon returns an unresolved torrent, StreamMux
  resolves it through your debrid service (StremThru-backed: Real-Debrid,
  TorBox, AllDebrid, Premiumize, Debrid-Link and more).
- **Video is never re-encoded** — compatible audio is copied; codecs that are
  unreliable in HLS are converted to AAC for broad player compatibility.
- **Fast-start fallback race** — at most two plans are prepared concurrently;
  the first one to publish a complete segment wins and the other is cancelled.
- **Automatic recovery** — a dubbed source can supply both picture and audio,
  and the best subtitled source is the final fallback when dubbed plans fail.
- **Real session telemetry** — downgrade decisions use FFmpeg's actual media
  production rate during playback, without a separate bandwidth-test download.
- **Multi-user** — each config is stored per user (SQLite), protected with an
  encrypted password.
- **Beautiful config UI** — a modern React interface (Vite + Tailwind v4 +
  UntitledUI) to manage addons, language and debrid services.

## How it works

1. Add addons by their manifest URL, marking each as a source of **video**,
   **audio**, or **both**.
2. When Stremio requests streams, StreamMux queries all configured addons.
3. It ranks every result (resolution, quality, encode, visual/audio tags,
   channels, language).
4. It builds an ordered list of playback plans: best cross-source pair,
   high-quality single-source dubbed fallbacks, lower-quality combinations and
   finally direct subtitled sources.
5. Work stays lazy until play is pressed. The startup coordinator races at most
   two plans, selects the first one that produces a complete HLS segment and
   cancels wasted work.
6. During playback, FFmpeg progress is measured over stable windows. If the
   active source cannot sustain real time and the buffered lead is shrinking,
   StreamMux prepares the next best plan and switches at a segment boundary.

## Getting started

### Prerequisites

- **Go 1.25+**
- **Node.js 20+** and npm
- **FFmpeg** (with `ffprobe`) on your `PATH` — required for remuxing

### Quick start (Docker — recommended)

```bash
# Copy the env sample and edit SECRET_KEY
cp .env.sample .env
# edit .env, set a long random SECRET_KEY

docker compose up --build
```

The web UI will be available at <http://localhost:3001>.

### Quick start (from source)

```bash
# 1. Install dependencies
make deps

# 2. Build the frontend (the Go binary embeds it)
make frontend

# 3. Build the Go binary
make backend

# 4. Run it
export SECRET_KEY=your-long-random-secret-key
./bin/streammux
```

Open <http://localhost:3001> in your browser.

> **Heads up:** the `go:embed` step requires the frontend to be built first
> (`make frontend`). The `Makefile` handles this ordering for you, and the
> Docker image does it inside the build.

### Development

```bash
# Terminal 1 — frontend dev server (hot reload)
cd web && npm run dev

# Terminal 2 — Go server (proxies /api and /stream to :3001)
PORT=3001 go run ./cmd/streammux
```

## Configuration

Copy `.env.sample` to `.env` and adjust:

| Variable       | Default                        | Description                                                |
|----------------|--------------------------------|------------------------------------------------------------|
| `PORT`         | `3001`                         | HTTP port                                                  |
| `SECRET_KEY`   | `streammux-default-...`        | AES-GCM key for password encryption — **change in prod**   |
| `DATABASE_URI` | `sqlite:///data/streammux.db`  | SQLite database location                                    |
| `BASE_URL`     | `http://localhost:{PORT}`      | Public base URL (set when behind a reverse proxy)           |
| `FFMPEG_PATH`  | `ffmpeg`                       | Path to the ffmpeg binary (ffprobe must be alongside)      |

### Configuring your addons

In the web UI:

1. **Language** — choose your preferred audio language (e.g. *Portuguese (Brazil)*).
2. **Addons** — paste each addon's manifest URL and mark its role:
   - **Video** — sources of best picture quality (usually English).
   - **Audio** — sources of audio in your language (usually dubbed).
   - **Both** — sources that can serve either (e.g. a dubbed 4K REMUX).
3. **Debrid services** (optional) — add API keys so unresolved torrents can be
   resolved.
4. **Save** — set a password to protect your config, then install in Stremio
   with the provided manifest URL.

## Project layout

```
cmd/streammux/        entrypoint
internal/
  application/        business logic (parser, collector, analyzer, planner, muxer, ffmpeg, resolver)
  domain/             models, constants, ports (hexagonal core)
  infrastructure/     SQLite, crypto, in-memory store
  interface/http/     HTTP server, routes, SPA serving
web/                  React + Vite + Tailwind v4 + UntitledUI frontend
```

The codebase follows a **hexagonal / DDD** architecture: the domain layer is
dependency-free, the application layer implements the business logic, and the
infrastructure + interface layers provide concrete implementations.

## How stream selection works

Streams are ranked using a multi-factor score adapted from the AIOStreams
parser:

**Video score** = resolution + quality + encode + visual tags + size bonus
**Audio score** = audio tags + channels + size bonus

The language is a hard filter: a "dubbed" candidate must contain your language
(from the filename, language flags, ISO codes, or the addon's configured
language) **and** be confirmed by FFprobe to have a matching audio track.

### Duration matching

To avoid mixing audio and video from different releases (e.g. an extended cut
with a theatrical cut), StreamMux rejects conflicting edition tags and requires
known durations within `0.1%` of each other, clamped to a tolerance between one
and five seconds. Frame rates must also agree when both sources report them.

Roles are preferences rather than hard exclusions during recovery. A source
configured for dubbed audio can therefore become the video source when it also
contains a better 4K picture than the remaining video addons.

## Roadmap

- [ ] More debrid providers (Direct via provider APIs, not just StremThru)
- [ ] Configurable duration tolerance
- [ ] Per-title language overrides
- [ ] Persistent probe cache across process restarts

## License

[MIT](./LICENSE)
