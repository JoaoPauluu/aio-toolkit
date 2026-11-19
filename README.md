# 🧰 AIO Toolkit

AIO Toolkit is a self-hosted toolbox with a web UI, designed to use very little memory. Every tool is a **plugin**: a folder with a `plugin.yaml` and a script in any language. The UI builds a form for each plugin from its manifest, runs the script as a job, shows live progress and logs, and offers the results as downloads. To add a tool, drop a folder into `./plugins/`. You don't need to rebuild anything.

**Built-in tools**
- **Merge & Reorder PDF**: merge several PDFs in drag-and-drop order, or arrange the pages of one or more PDFs on a thumbnail grid (reorder, rotate, delete).
- **Compress PDF**: Ghostscript presets, with optional grayscale.
- **Split PDF**: split into single pages, custom ranges (`1-3, 5, 9-z`) or chunks of N pages.
- **Video / Audio Downloader**: yt-dlp plus ffmpeg, producing MP4 (H.264/AAC preferred), MP3, M4A or the best original. It supports playlists, embedded metadata and subtitles, cookies, and saving to a server library folder.
- **Update yt-dlp**: updates yt-dlp in place without rebuilding the image.

## Run it

```sh
mkdir -p data downloads          # create these yourself so they're owned by your user
docker compose up -d --build
```

Then open `http://<host>:8080`. Set `PUID`/`PGID` and `AIO_PORT` in a `.env` file if your values differ from the defaults (1000 / 1000 / 8080).

The app has **no authentication**. Put it behind your reverse proxy or VPN (Tailscale, Authelia, a Traefik/Caddy forward-auth setup, …). For Server-Sent Events through nginx, the app already sends `X-Accel-Buffering: no`.

## Architecture

```
Browser ──HTTP / SSE──► aio (Go, ~10-20 MB RSS idle)
                         ├─ plugin registry: scans /app/plugins + /plugins, hot reload
                         ├─ job runner: queue (AIO_MAX_JOBS), subprocess per job, cancel, timeout
                         └─ job store: /data/jobs/<id>/{in,out}, deleted after AIO_JOB_TTL
                                 │
                       python3 run.py / sh run.sh  →  qpdf · gs · ffmpeg · yt-dlp
```

- **Idle memory** is just the Go binary. Heavy tools run only for as long as a job does.
- **Uploads** stream straight to disk and are never buffered in RAM.
- **PDF thumbnails** are rendered in the browser with pdf.js, not on the server.

## Configuration (env)

| Var | Default | |
|---|---|---|
| `AIO_MAX_JOBS` | `2` | jobs that run at the same time; extra jobs wait in a queue |
| `AIO_JOB_TTL` | `24h` | how long finished jobs and their files are kept |
| `AIO_MAX_UPLOAD_MB` | `4096` | maximum upload size per request |
| `AIO_PLUGIN_DIRS` | `/app/plugins:/plugins` | plugin roots; a plugin in a later root overrides one with the same id in an earlier root |
| `AIO_PLUGIN_POLL` | `5s` | how often to check for plugin changes |
| `AIO_YTDLP_JS_RUNTIME` | `node` | JS runtime passed to yt-dlp (`--js-runtimes`); empty disables it |

## Writing plugins

See **[docs/PLUGINS.md](docs/PLUGINS.md)**.

## Local development (without Docker)

You need Go 1.23+, plus python3, qpdf, ghostscript, ffmpeg and yt-dlp on your PATH.

```sh
make vendor   # fetch pdf.js + SortableJS into web/static/vendor
make run      # http://localhost:8080
```
