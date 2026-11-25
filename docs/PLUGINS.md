# Writing plugins

A plugin is a folder with a `plugin.yaml` and a script. Put the folder in `./plugins/` (mounted at `/plugins` in the container). The server notices it within about 5 seconds; you can also click **↻ Reload plugins**. You don't need to rebuild or restart anything.

```
plugins/
  my-tool/
    plugin.yaml
    run.py          # or run.sh, or any executable
    ui/index.html   # optional custom UI
```

A plugin with the same `id` as a built-in one replaces it, so you can edit the shipped plugins in `./plugins/` too.

## plugin.yaml

```yaml
id: my-tool                 # lowercase, digits, . _ -   (defaults to the folder name)
name: My Tool
category: Utilities         # sidebar group
icon: "🛠️"                  # any emoji / short text
description: One line shown under the title.
run: [python3, run.py]      # argv; runs with the plugin folder as cwd
timeout: 30m                # optional, default 2h
submit_label: Go            # optional button text
ui: ui/index.html           # optional: custom UI instead of the generated form
inputs:
  - name: files
    type: file              # file | text | textarea | number | select | checkbox | url | hidden | json
    label: Input files
    accept: .pdf
    multiple: true
    required: true
  - name: mode
    type: select
    default: fast
    options:
      - fast                         # plain value…
      - {value: slow, label: "Slow but good"}   # …or value + label
  - name: level
    type: number
    min: 1
    max: 9
    default: 5
    show_if: {mode: [slow]}          # only shown (and sent) when mode == slow
  - name: verbose
    type: checkbox
    default: false
    help: Small helper text under the field.
  - name: pages
    type: json                       # any JSON value; not shown in the generated form,
                                     # set by a custom UI or an API caller
```

Optional keys for richer plugins (all explained below):

```yaml
version: 1.2.0                # shown in the UI; bump it when you publish
author: your-name
homepage: https://github.com/you/aio-my-tool
license: MIT
aio_api: 1                    # plugin API version you need; newer than the host's → refused
provides: [pdf.ocr]           # capabilities other plugins can look up
requires:                     # checked / installed together with this plugin
  - id: pdf-merge             # must exist (built-in or installed)
  - source: github.com/you/aio-ocr-models   # community plugin, installed automatically
setup: [python3, setup.py]    # runs once after every install/update (download tools, pip install…)
service:                      # long-running process, reachable at /s/<id>/
  run: [python3, server.py]
  health: /health             # optional HTTP path polled until it answers
  restart: always             # always (default) | on-failure | never
schedule:                     # start jobs automatically
  - every: 6h                 # Go duration, at least 1m
    params: {mode: full}      # input values (defaults for the rest)
  - cron: "0 3 * * 1-5"       # minute hour day month weekday, or @hourly/@daily/@weekly/@monthly
events:                       # start a job when an event fires
  - on: job.done              # exact topic, or a prefix like job.* ; * = everything
    filter: {plugin: ytdlp}   # event data fields that must match (a list = any of)
    params: {}
```

`run` is required unless the plugin has a `ui` or a `service`. Plugins with `schedule` or `events` need `run`.

**Declare every parameter your script reads.** The manifest is the plugin's API: anything not declared can only be set by a custom UI, and `go test ./...` fails if a built-in `run.py` reads an undeclared key.

## Runtime contract

Your script gets these environment variables:

| Variable | Meaning |
|---|---|
| `AIO_PARAMS` | Path to `params.json`, which holds the submitted values. A file input becomes a **list of absolute paths** in upload order, a checkbox becomes a bool and a number becomes a float. |
| `AIO_IN` | Folder holding the uploaded files. They are stored as `NNN_originalname`. |
| `AIO_OUT` | **Every file you write here becomes a download in the UI.** |
| `AIO_JOB_DIR` | Scratch space for this job. It is deleted together with the job. |
| `AIO_DATA` | The persistent data volume (`/data`). |
| `AIO_DOWNLOADS` | The persistent "library" folder (`/downloads`). |
| `AIO_PLUGIN_DIR` | Your plugin folder. |
| `AIO_PLUGIN_ID` | Your plugin id. |
| `AIO_PLUGIN_DATA` | Persistent folder for your plugin (`/data/plugins/<id>`). It survives updates; its `bin/` is on `PATH`. |
| `AIO_TOOLS` | Shared tool folder (`/data/tools/bin`), on every plugin's `PATH`. |
| `AIO_API` | Base URL of the toolkit's HTTP API, for calling other plugins. |
| `AIO_JOB_TOKEN` | Secret identifying this job. Send it as `X-AIO-Job-Token` when calling the API (the SDKs do). |
| `AIO_ARCH` | CPU architecture: `amd64` or `arm64`. |
| `AIO_EVENT` | Only for jobs started by an event hook: path to the event JSON (also in `params["event"]`). |

Lines your script prints to stdout or stderr appear in the job log. Lines in these special forms update the UI instead:

```
::progress 42          progress bar (0-100)
::status Encoding…     status line under the progress bar
::title Some name      job title shown in the Jobs panel
::error Message        error shown to the user if the script then exits non-zero
```

Exit code `0` means success. Any other exit code means the job failed. Jobs can be cancelled from the UI. Cancelling sends SIGTERM to the whole process group, so child processes such as ffmpeg stop too.

### Python helper

`sdk/python` is on `PYTHONPATH`, so a Python script can use the helper module:

```python
import aio
p = aio.params()
aio.status("Working…"); aio.progress(30)
aio.run(["ffmpeg", "-i", p["files"][0], aio.out("result.mp4")])  # streams output to the log, fails the job on error
aio.fail("something went wrong")                                # ::error + exit 1
aio.original_name(path)    # "001_foo.pdf" -> "foo.pdf"
aio.title("name.pdf")      # job title in the Jobs panel
aio.plugin_data()          # Path of $AIO_PLUGIN_DATA (persistent)
```

### Bash example

```sh
#!/bin/sh
URL=$(python3 -c 'import json,os;print(json.load(open(os.environ["AIO_PARAMS"]))["url"])')
echo "::status Fetching"
curl -fsSL "$URL" -o "$AIO_OUT/page.html"
```

### Media helper (`aio_media`)

```python
from aio_media import probe, ffmpeg, out_for, parse_time
info = probe(src)                    # .duration .width .height .vcodec .acodec .video .audio
ffmpeg(["-i", src, "-c:v", "libx264", out_for(src, "mp4")], info.duration)   # live progress bar
ffmpeg(args_pass1, info.duration, 0, 50); ffmpeg(args_pass2, info.duration, 50, 100)  # two-pass
parse_time("1:30.5")                 # -> 90.5
```

### Extra Python packages

If a plugin needs a pip package, call `aio.ensure_packages()`. On the first run it installs the packages into `$AIO_DATA/venvs/<name>`, so they persist across image updates, and every run after that just imports them:

```python
import aio
aio.ensure_packages("qrcode", "qrcode[pil]")
import qrcode
```

## Custom UIs

Set `ui: ui/index.html`. If a plugin has a `ui` but no `run`, it is UI-only and talks directly to the server API (`plugins/share` works this way). The page loads in an iframe at `/p/<id>/ui/index.html`, and only the files in that folder are served. Use the shared stylesheet and the SDK:

```html
<link rel="stylesheet" href="/style.css">
<script src="/aio-sdk.js"></script>
<script>
  const job = await AIO.submit({
    files:  { files: [fileA, fileB] },   // field -> File[]  (order preserved)
    fields: { output_name: "x.pdf" },    // typed by the manifest's inputs
    params: { extra: [...] },            // any JSON, merged into params.json (prefer declared `json` inputs)
    title:  "shown in the Jobs panel",
  });
  // The main Jobs panel tracks progress and downloads automatically.
  AIO.watch(job.id, snap => console.log(snap.state, snap.progress));
</script>
```

`/vendor/` serves the bundled libraries: `pdf.min.mjs`, `pdf.worker.min.mjs` (pdf.js) and `Sortable.min.js`. See `plugins/pdf-merge/ui/index.html` for a full example.

## Talking to other plugins

Plugins call each other through the same HTTP API the UI uses, so any plugin can use any other one, built-in or community.

```python
import aio
res = aio.call("pdf-merge", files={"files": [a, b]})       # waits; raises RuntimeError if it fails
merged = res.files[0]                                       # outputs downloaded to the job folder
res = aio.call("ytdlp", {"urls": url, "format": "mp3"})     # params = the target's inputs

ocr = aio.find("pdf.ocr")          # plugins that declare provides: [pdf.ocr]
if ocr:
    aio.call(ocr[0]["id"], files={"files": [merged]})

aio.emit("my-tool.finished", {"count": 3})   # custom event; others can hook it
aio.event()                                  # the event that started this job (event hooks)
aio.api("GET", "/api/jobs")                  # anything else in the API
```

A job's calls carry its `AIO_JOB_TOKEN`. The called job then **skips the queue** (the caller already holds a slot, so `AIO_MAX_JOBS` can't deadlock), is canceled together with the caller, and shows its parent in `GET /api/jobs/<id>`. Calls can nest up to 4 levels.

From a custom UI:

```js
const job = await AIO.call("pdf-merge", { files: { files: [a, b] } });   // resolves when done
const found = await AIO.find("pdf.ocr");
const stop = AIO.on("job.done", (ev) => console.log(ev.data.plugin, ev.data.files));
await AIO.emit("my-tool.saved", { id: 1 });
fetch(AIO.serviceUrl() + "items");                                       // your background service
```

Any language works: send `X-AIO-Job-Token: $AIO_JOB_TOKEN` to `$AIO_API`.

## Events

The server publishes these events. Each one has `topic`, `time`, `data`, and `source` (the plugin whose event hook caused it, if any):

| Topic | `data` |
|---|---|
| `job.queued`, `job.started`, `job.done`, `job.failed`, `job.canceled` | `job`, `plugin`, `title`, `state`, `kind`, `files` (name, size, url), `error`, `parent` |
| `plugin.installed`, `plugin.updated`, `plugin.removed`, `plugin.enabled`, `plugin.disabled` | `source`, `url`, `ref`, `commit`, `plugins` |
| `service.started`, `service.stopped` | `plugin`, `state`, `port`, `error` |
| `share.created`, `share.deleted` | `share`, `files`, `size` |
| `<namespace>.<name>` | whatever a plugin publishes with `aio.emit` / `POST /api/events` |

Custom topics can't use the built-in prefixes. Use your plugin id as the namespace.

An `events:` hook starts a job of your plugin with the event in `params["event"]` and in the file `$AIO_EVENT`. To prevent loops, a plugin never receives events caused by its own hook jobs, and a chain of hook-started jobs stops after 3 hops.

`GET /api/events?topic=job.done,my-tool.*` streams events as Server-Sent Events.

## Schedules

`schedule:` entries start a normal job (it shows in the Jobs panel) every interval or at cron times in the container's time zone (set `TZ` to change it). `params` fill inputs; everything else takes its default.

## Background services

A `service:` is a process that runs as long as the plugin is enabled, such as a small web app, a watcher or a bot. The server:

- starts it with the plugin folder as cwd and the same environment as jobs, plus `AIO_SERVICE_PORT` (and `PORT`) — **listen on 127.0.0.1 at that port** — and `AIO_SERVICE_BASE=/s/<id>/`;
- proxies `/s/<id>/<path>` to `http://127.0.0.1:<port>/<path>` (streaming and SSE work; the original prefix is sent as `X-Forwarded-Prefix`);
- restarts it when it exits (with backoff, per `restart`) and after the plugin is updated, and stops it when the plugin is disabled or removed;
- keeps the last 500 lines of output: `GET /api/services/<id>/logs`, also shown in the Community plugins tab.

A plugin can have a service and a `ui` together: the UI talks to `AIO.serviceUrl()`. Services cost memory while they run, so prefer jobs when you can.

## Bringing your own tools

The image contains Python 3, ffmpeg, qpdf, Ghostscript, Node.js, libvips and yt-dlp. The container runs as a normal user on **Alpine Linux (musl)**, so `apk add` isn't available at runtime. Instead:

- **Prebuilt binaries:** `aio.ensure_binary()` downloads a binary (or extracts it from a `.tar.gz`/`.zip`) for the current architecture into your plugin's `bin/`, once per `version`. Use static or `*-linux-musl` builds:

  ```python
  exe = aio.ensure_binary("tectonic", {
      "amd64": "https://…/tectonic-0.17.0-x86_64-unknown-linux-musl.tar.gz",
      "arm64": "https://…/tectonic-0.17.0-aarch64-unknown-linux-musl.tar.gz",
  }, version="0.17.0", sha256={"amd64": "…"})   # sha256 optional
  ```

  `shared=True` puts it in `$AIO_TOOLS`, on every plugin's `PATH`, so one plugin can provide a tool for others.
- **Python packages:** `aio.ensure_packages()` (above).
- **Node packages:** `npm install --prefix "$AIO_PLUGIN_DATA" <pkg>`.
- **Compile from source:** pure-Go or Rust toolchains can be downloaded the same way into `$AIO_PLUGIN_DATA` and run from `setup`.

Do this work in `setup:` so it happens at install time, and its progress shows in the Jobs panel. A failed setup aborts the install or update and keeps the old version. Setup runs in a staging folder that is moved into place afterwards, so write anything that must persist to `$AIO_PLUGIN_DATA` rather than baking absolute plugin paths into files. Caches under `$XDG_CACHE_HOME` (`/data/cache`) persist too.

[`examples/latex-tectonic`](../examples/latex-tectonic) compiles LaTeX with Tectonic this way.

For something only a system package provides, extend the image:

  ```dockerfile
  FROM aio-toolkit:latest
  USER root
  RUN apk add --no-cache imagemagick
  ```

## HTTP API

The UI is built only on these endpoints, so anything the UI does can be scripted, e.g. from another container on the same docker network (`http://aio-toolkit:8080`):

```
GET    /api/plugins                    list plugins + load errors
GET    /api/plugins/{id}               one plugin's schema: inputs, types, defaults, options, min/max, show_if
POST   /api/plugins/reload
POST   /api/plugins/{id}/run           multipart, urlencoded or JSON → job JSON
GET    /api/jobs                       recent jobs
GET    /api/jobs/{id}                  job + logs (?wait=… to block until it finishes)
GET    /api/jobs/{id}/events           Server-Sent Events: snapshot / update / log
GET    /api/jobs/{id}/files/{name}     download an output (?inline=1 to view)
GET    /api/jobs/{id}/zip              all outputs as a zip
POST   /api/jobs/{id}/cancel
DELETE /api/jobs/{id}
GET    /api/plugins?provides=cap       plugins that declare a capability
POST   /api/shares                     multipart: files, expires (1h|1d|7d|30d|never), password, note
GET    /api/shares
DELETE /api/shares/{id}
GET    /api/events                     Server-Sent Events (?topic=job.done,plugin.*)
POST   /api/events                     {"topic": "my-tool.thing", "data": {…}}
GET    /api/services                   background services and their state
GET    /api/services/{id}/logs
POST   /api/services/{id}/restart
*      /s/{id}/{path}                  proxied to the plugin's service
GET    /api/community                  installed community sources
POST   /api/community/preview          {"url": "…"} → download + validate, returns a token and what would be installed
POST   /api/community/install          {"token": "…", "auto_update": true, "wait": false}
DELETE /api/community/preview/{token}
PATCH  /api/community/{id}             {"enabled": bool, "auto_update": bool, "ref": "v2"}
POST   /api/community/{id}/check       look for updates (applies them if auto-update is on); ?wait=1 to block
POST   /api/community/{id}/update      update now
POST   /api/community/{id}/rollback    back to the previous version (turns auto-update off)
DELETE /api/community/{id}
POST   /api/community/check            check every source
```

### Parameters

Field names are the manifest's input names; `GET /api/plugins/{id}` lists them with their allowed values. Fields work exactly as in the form: missing fields take their default, checkboxes accept `true/false/1/on/yes`, and a field whose `show_if` isn't met is ignored (its default is used), so the same choices give the same `params.json` whether they come from the UI or the API.

- **multipart/form-data**: needed for file inputs (repeat the field for several files). A `json` input is sent as JSON text.
- **application/json**: an object of field → value (numbers, booleans, strings, arrays for multi-value fields). A `json` input takes the value directly. Files can't be sent this way.
- `__title` sets the job title shown in the Jobs panel.

### Waiting for the result

By default `run` returns `201` immediately with the queued job. Add query options to get the result in one request:

- `?wait=5m` (Go duration or seconds, capped at the plugin timeout): wait for the job. `200` + job JSON (with logs, and a `url` for each output file) when it finished, or `202` + job JSON if it is still running when the wait ends — then poll `GET /api/jobs/{id}?wait=5m`. Disconnecting doesn't cancel the job.
- `&output=file`: reply with the output itself — the single output file, the one named by `&file=<name>`, or a zip when there are several. A failed or canceled job returns `422` + job JSON with the last log lines; a job with no outputs (e.g. saved to the server library) returns the job JSON.

```sh
# Download a video at 720p into the server library, waiting up to 30 minutes
curl -H 'Content-Type: application/json' \
     -d '{"urls":"https://youtu.be/…","format":"mp4","quality":"720","compat":"auto","library":true}' \
     'http://aio-toolkit:8080/api/plugins/ytdlp/run?wait=30m'

# Merge two PDFs with a custom page order and get the PDF back
curl -F files=@a.pdf -F files=@b.pdf -F 'pages=[{"file":1,"page":1},{"file":0,"page":2,"rotate":90}]' \
     'http://aio-toolkit:8080/api/plugins/pdf-merge/run?wait=5m&output=file' -o merged.pdf

# QR code as PNG
curl -H 'Content-Type: application/json' -d '{"text":"https://example.com"}' \
     'http://aio-toolkit:8080/api/plugins/qr-code/run?wait=1m&output=file' -o qr.png
```

## Publishing a community plugin

1. Put your plugin in a public Git repository: `plugin.yaml` at the repository root, or one folder per plugin (at the root or under `plugins/`) to publish a pack. Copying [`examples/latex-tectonic`](../examples/latex-tectonic) is a quick start.
2. Set `version`, `author`, `homepage` and `aio_api: 1`. If there's no `id`, the folder (or repository) name is used.
3. Tag releases (`v1.0.0`). Users who paste the plain repository URL get the **latest release** and are updated when you publish a new one. Without releases they follow the default branch, so every push becomes an update.
4. Share the link: `https://github.com/you/aio-my-tool`, or `…/tree/main/plugins/my-tool` for one folder of a bigger repository. GitLab, Codeberg/Gitea/Forgejo and plain `.zip`/`.tar.gz` links work too.

Users can pin a version with `owner/repo@v1.0.0` (or by changing the ref via the API). The toolkit refuses archives with absolute paths, `..`, links pointing outside the plugin or special files, and anything larger than `AIO_COMMUNITY_MAX_MB`.

For a quick test, run `make run` and copy the folder into `./plugins/` (it is picked up within 5 seconds), or install it from a `.zip` link.
