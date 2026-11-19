"""yt-dlp wrapper: downloads, converts with ffmpeg and reports progress."""
import os
import re
import subprocess

import aio

p = aio.params()
urls = [u.strip() for u in re.split(r"[\s,]+", p.get("urls", "")) if u.strip()]
if not urls:
    aio.fail("Enter at least one URL")
for u in urls:
    if not re.match(r"^https?://", u, re.I):
        aio.fail(f"Not a URL: {u}")

ytdlp = aio.which("yt-dlp")
aio.which("ffmpeg")
fmt = p.get("format", "mp4")
q = p.get("quality", "best")

dest = aio.DOWNLOADS if p.get("library") else aio.OUT
if p.get("library"):
    sub = "audio" if fmt in ("mp3", "m4a") else "video"
    dest = dest / sub
dest.mkdir(parents=True, exist_ok=True)

cmd = [ytdlp, "--newline", "--no-colors", "--ignore-config", "--no-mtime",
       "--paths", str(dest), "--paths", f"temp:{aio.JOB_DIR / 'tmp'}",
       "-o", "%(title).150B [%(id)s].%(ext)s",
       "--windows-filenames", "--no-part",
       "--progress-template", "download:AIOPROG %(progress._percent_str)s|%(progress._speed_str)s|%(progress._eta_str)s|%(info.playlist_index)s|%(info.n_entries)s",
       "--cache-dir", str(aio.DATA / "cache" / "yt-dlp")]

# yt-dlp needs a JavaScript runtime for YouTube; the image ships Node.
js = os.environ.get("AIO_YTDLP_JS_RUNTIME", "node")
if js:
    cmd += ["--js-runtimes", js]

cmd += ["--yes-playlist"] if p.get("playlist") else ["--no-playlist"]

h = "" if q == "best" else f"[height<={q}]"
if fmt == "mp4":
    cmd += ["-f", f"bv*{h}+ba/b{h}/bv*+ba/b",
            "-S", ("res:" + q + "," if q != "best" else "") + "vcodec:h264,acodec:aac,ext:mp4:m4a",
            "--merge-output-format", "mp4"]
    if p.get("recode"):
        cmd += ["--recode-video", "mp4", "--postprocessor-args", "VideoConvertor:-c:v libx264 -preset veryfast -crf 22 -c:a aac -b:a 192k"]
    else:
        cmd += ["--remux-video", "mp4"]
elif fmt == "mp3":
    cmd += ["-f", "ba/b", "-x", "--audio-format", "mp3", "--audio-quality", p.get("bitrate", "0")]
elif fmt == "m4a":
    cmd += ["-f", "ba[ext=m4a]/ba/b", "-x", "--audio-format", "m4a"]
else:
    cmd += ["-f", f"bv*{h}+ba/b{h}/bv*+ba/b"]

if p.get("metadata"):
    cmd += ["--embed-metadata", "--embed-thumbnail", "--embed-chapters"]
    if fmt == "mp4":
        cmd += ["--convert-thumbnails", "jpg"]
if p.get("subs") and fmt in ("mp4", "best"):
    cmd += ["--embed-subs", "--sub-langs", "en.*,en", "--write-subs", "--write-auto-subs"]

cookies = p.get("cookies") or []
if cookies:
    cmd += ["--cookies", cookies[0]]

cmd += ["--", *urls]

import time
started = time.time() - 2

aio.status("Starting yt-dlp")
aio.log("$ " + " ".join(cmd))
proc = subprocess.Popen(cmd, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, errors="replace")
last_err, title = "", None
for line in proc.stdout:
    line = line.rstrip()
    if line.startswith("AIOPROG "):
        pct, speed, eta, idx, n = (line[8:].split("|") + [""] * 5)[:5]
        try:
            v = float(pct.strip().rstrip("%"))
        except ValueError:
            continue
        # spread progress across playlist entries and the two (video+audio) streams
        if idx.isdigit() and n.isdigit() and int(n) > 0:
            v = ((int(idx) - 1) + v / 100) / int(n) * 100
        aio.progress(v * 0.95)
        what = f"item {idx}/{n} · " if idx.isdigit() and n.isdigit() else ""
        aio.status(f"{what}{speed.strip()} · ETA {eta.strip()}")
        continue
    m = re.match(r"\[download\] Destination: (.+)", line)
    if m and not title:
        title = os.path.splitext(os.path.basename(m[1]))[0]
        title = re.sub(r"\.f\d+$", "", title)  # strip yt-dlp's per-stream suffix
        print("::title " + title, flush=True)
    if re.match(r"\[(Merger|ExtractAudio|VideoConvertor|VideoRemuxer|EmbedThumbnail|Metadata)\]", line):
        aio.status(line.split("]", 1)[0].strip("[") + "…")
    if line.startswith("ERROR:"):
        last_err = line[6:].strip()
    aio.log(line)
proc.wait()

# --no-mtime makes finished files carry the current time, so new files are easy to find.
media = {".mp4", ".mp3", ".m4a", ".webm", ".mkv", ".opus", ".ogg", ".flac", ".wav", ".mov", ".avi"}
files = [f for f in dest.rglob("*") if f.is_file() and f.stat().st_mtime >= started
         and (f.suffix.lower() in media or fmt == "best")]
for f in files:
    aio.log(f"Saved: {f}")

if proc.returncode != 0 and not files:
    aio.fail(last_err or f"yt-dlp exited with code {proc.returncode}")
if proc.returncode != 0:
    aio.log(f"Some items failed: {last_err}")

if p.get("library"):
    aio.status(f"Saved {len(files)} file(s) to the library ({dest})")
else:
    aio.status(f"Downloaded {len(files)} file(s)")
