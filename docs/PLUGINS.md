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

## Needing extra tools

The image contains Python 3, ffmpeg, qpdf, Ghostscript, Node.js and yt-dlp. If a plugin needs something else, either:

- install Python packages into a venv under `$AIO_DATA` from the plugin itself (see `plugins/ytdlp-update/run.sh`), or
- extend the image:

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
POST   /api/shares                     multipart: files, expires (1h|1d|7d|30d|never), password, note
GET    /api/shares
DELETE /api/shares/{id}
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
