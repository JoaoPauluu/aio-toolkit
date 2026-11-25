# 🧰 AIO Toolkit

AIO Toolkit is a self-hosted toolbox with a web UI, designed to use very little memory. Every tool is a **plugin**: a folder with a `plugin.yaml` and a script in any language. The UI builds a form for each plugin from its manifest, runs the script as a job, shows live progress and logs, and offers the results as downloads. To add a tool, drop a folder into `./plugins/`. You don't need to rebuild anything.

## Built-in tools

**PDF**
- **Merge & Reorder PDF**: merge several PDFs in drag-and-drop order, or arrange the pages of one or more PDFs on a thumbnail grid (reorder, rotate, delete).
- **Compress PDF**: Ghostscript presets, with optional grayscale.
- **Split PDF**: split into single pages, custom ranges (`1-3, 5, 9-z`) or chunks of N pages.
- **Unlock / Protect PDF**: remove a password, or add AES-256 encryption with print, copy and edit permissions.
- **PDF → Images**: export pages as PNG or JPG at a chosen DPI, for all pages or a range.
- **Bulk Rotate / Remove Pages**: for example, rotate every even page or delete pages 3–5.

**Media** (ffmpeg)
- **Video / Audio Downloader**: yt-dlp plus ffmpeg, producing MP4 (H.264/AAC preferred), MP3, M4A or the best original. It supports playlists, embedded metadata and subtitles, cookies, and saving to a server library folder.
- **Update yt-dlp**: updates yt-dlp in place without rebuilding the image.
- **Media Converter**: convert any audio or video format to any other. You can choose the codec (H.264, H.265, VP9 or AV1), CRF, encoder speed, resolution and audio bitrate. "Auto" copies the streams unchanged when the target container supports them.
- **Trim / Cut**: either a lossless cut (instant, but it can only cut at keyframes) or a frame-accurate cut that re-encodes.
- **Extract Audio**: copies the original audio stream unchanged into the matching container (AAC→.m4a, Opus→.opus, …), or converts it to MP3, M4A, Opus, FLAC or WAV.
- **Video → GIF / WebP**: GIF with a two-pass optimised palette, or animated WebP.
- **Compress Video to Size**: hits a target such as "under 10 MB for Discord" by working out the bitrate from the duration. It uses two-pass x264 and lowers the resolution automatically when the bitrate gets too low.
- **Normalize Loudness**: two-pass EBU R128 `loudnorm`, with presets for podcasts (−16 LUFS), streaming (−14) and broadcast (−23).

**Images & utilities**
- **Image Converter / Resizer**: libvips converts HEIC, JPG, PNG, WebP, AVIF and TIFF in bulk. It can resize by longest side, width or percent, strips EXIF and GPS data, and rotates images upright automatically.
- **QR Code Generator**: links, text or Wi-Fi networks, as PNG or SVG. Its Python library is installed the first time it runs.
- **Share files**: share files by private link at `https://your-host/files/<random-id>`, with an expiry and an optional password, plus a list of active shares. See [Sharing](#sharing-files-by-link).

## Community plugins

Open **🧩 Community plugins** (bottom of the sidebar) and paste a link:

- `https://github.com/owner/repo`, `owner/repo@v1.2.0`, or `https://github.com/owner/repo/tree/main/some/folder`
- GitLab (`https://gitlab.com/group/repo`), Codeberg / Gitea / Forgejo (`https://codeberg.org/owner/repo`)
- any direct `.zip` / `.tar.gz` link

The toolkit downloads the plugin over HTTPS (no `git` needed), shows what it will run, and installs it into `/plugins/community/` after you confirm. Without a ref it installs the latest release, or the default branch when there are no releases. Every 6 hours it checks for new commits or releases and updates automatically. You can turn auto-update off per plugin and update by hand instead, roll back to the previous version, disable a plugin or remove it. State lives in `/plugins/community/.state.json`, so **mount `/plugins`** (both compose examples do) to keep community plugins across restarts and image updates.

Community plugins run their own code inside the container. Only install plugins you trust. Set `AIO_COMMUNITY=off` to disable the feature.

To publish one, put a plugin folder (see [Writing plugins](docs/PLUGINS.md)) in a public repository, at the root or one folder per plugin. [`examples/latex-tectonic`](examples/latex-tectonic) is a complete example that downloads its own toolchain (Tectonic) to compile LaTeX. See [Publishing a plugin](docs/PLUGINS.md#publishing-a-community-plugin).

## Run it

```sh
mkdir -p data downloads          # create these yourself so they're owned by your user
docker compose up -d --build
```

Then open `http://<host>:8080`. Set `PUID`/`PGID` and `AIO_PORT` in a `.env` file if your values differ from the defaults (1000 / 1000 / 8080).

The app has **no authentication**. Put it behind your reverse proxy or VPN (Tailscale, Authelia, a Traefik/Caddy forward-auth setup, …). For Server-Sent Events through nginx, the app already sends `X-Accel-Buffering: no`.

## Deploy the prebuilt image

GitHub Actions builds `ghcr.io/joaopauluu/aio-toolkit` for amd64 and arm64. It rebuilds on every push to `main`, on `v*` tags, and once a week so yt-dlp and ffmpeg stay fresh. Add this service to your stack:

```yaml
  aio-toolkit:
    image: ghcr.io/joaopauluu/aio-toolkit:latest
    container_name: aio-toolkit
    restart: unless-stopped
    user: "1000:1000"
    ports:
      - "8765:8080"
    environment:
      AIO_MAX_JOBS: 2
      AIO_JOB_TTL: 24h
    volumes:
      - aio-data:/data
      - ./aio/downloads:/downloads
      - ./aio/plugins:/plugins
    mem_limit: 768m
```

You also need `aio-data:` under the stack's top-level `volumes:` key. Create `./aio/downloads` and `./aio/plugins` yourself first, so your user owns them. Then update with `docker compose pull aio-toolkit && docker compose up -d aio-toolkit`.

## Sharing files by link

The **Share files** tool uploads files and gives you a link such as `https://tools.example.com/files/Qm3x…`. The ID is 22 random characters, so links can't be guessed. Each share has an expiry (1 hour to never, 7 days by default) and an optional password. Expired shares are deleted automatically.

The rest of the app has no login. Your reverse proxy should therefore **let only `/files/*` through without authentication** and protect everything else:

- **Traefik + Authelia/forward-auth.** Add a second router for the share path that has a higher priority and no auth middleware:

  ```yaml
  labels:
    - traefik.http.routers.aio.rule=Host(`tools.example.com`)
    - traefik.http.routers.aio.middlewares=authelia@docker
    - traefik.http.routers.aio-share.rule=Host(`tools.example.com`) && PathPrefix(`/files/`)
    - traefik.http.routers.aio-share.priority=100
    - traefik.http.services.aio.loadbalancer.server.port=8080
  ```

- **Caddy:**

  ```
  tools.example.com {
      @public path /files/*
      handle @public {
          reverse_proxy aio-toolkit:8080
      }
      handle {
          forward_auth authelia:9091 {
              uri /api/authz/forward-auth
              copy_headers Remote-User
          }
          reverse_proxy aio-toolkit:8080
      }
  }
  ```

- **Authelia access rules** (if you protect the whole host with it):

  ```yaml
  - domain: tools.example.com
    resources: ['^/files/.*$']
    policy: bypass
  ```

Set `AIO_PUBLIC_URL=https://tools.example.com` so the links the UI hands out use your public domain, even when you created the share over your LAN or VPN address.

## Calling tools from other containers

Every tool is also an HTTP endpoint, with the same options as its form (for example the downloader's format and quality). Authelia protects the reverse proxy, not the container, so other services on the same docker network can call `http://aio-toolkit:8080/api/...` directly, without going through the proxy. There is no extra port or process: the same server handles both.

```sh
curl -F files=@a.pdf -F files=@b.pdf 'http://aio-toolkit:8080/api/plugins/pdf-merge/run?wait=5m&output=file' -o merged.pdf
```

`GET /api/plugins/<id>` describes a tool's fields, `?wait=` returns the finished job in one request, and `&output=file` returns the file itself. JSON bodies work for tools without file inputs. See [HTTP API](docs/PLUGINS.md#http-api).

The API has no authentication, so anything that can reach port 8080 can use it. Keep the container on an internal docker network shared only with the proxy and the services that need it, and remove `ports:` if nothing outside docker needs direct access.

## Architecture

```
Browser ──HTTP / SSE──► aio (Go, ~10-20 MB RSS idle)
                         ├─ plugin registry: scans /app/plugins, /plugins/community, /plugins; hot reload
                         ├─ community manager: install from GitHub/GitLab/Gitea/archives, update checks, rollback
                         ├─ job runner: queue (AIO_MAX_JOBS), subprocess per job, cancel, timeout
                         ├─ services, schedules and event hooks declared by plugins
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
| `AIO_PUBLIC_URL` | *(browser origin)* | base URL used in share links, e.g. `https://tools.example.com` |
| `AIO_SHARE_MAX_MB` | `2048` | maximum upload size per share |
| `AIO_COMMUNITY` | `on` | `off` disables installing community plugins |
| `AIO_COMMUNITY_DIR` | `/plugins/community` | where community plugins are installed (keep it on a persistent volume) |
| `AIO_COMMUNITY_CHECK` | `6h` | how often to check community plugins for updates (`0` = never) |
| `AIO_COMMUNITY_MAX_MB` | `200` | maximum download / unpacked size of one community plugin |
| `AIO_GITHUB_TOKEN`, `AIO_GITLAB_TOKEN`, `AIO_GITEA_TOKEN` | | optional API tokens for private repositories and higher rate limits |
| `AIO_INTERNAL_URL` | `http://127.0.0.1:<port>` | how plugins reach the server (`AIO_API`) |

## Writing plugins

See **[docs/PLUGINS.md](docs/PLUGINS.md)**.

## Local development (without Docker)

You need Go 1.23+, plus python3, qpdf, ghostscript, ffmpeg and yt-dlp on your PATH.

```sh
make vendor   # fetch pdf.js + SortableJS into web/static/vendor
make run      # http://localhost:8080
```
