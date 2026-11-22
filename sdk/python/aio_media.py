"""ffmpeg helpers for AIO Toolkit media plugins."""
from __future__ import annotations

import json
import re
import subprocess
from pathlib import Path

import aio


class Probe:
    def __init__(self, data: dict):
        self.data = data
        self.streams = data.get("streams", [])
        fmt = data.get("format", {})
        self.duration = float(fmt.get("duration") or 0) or max(
            (float(s.get("duration") or 0) for s in self.streams), default=0.0)
        self.size = int(fmt.get("size") or 0)
        self.video = next((s for s in self.streams if s.get("codec_type") == "video"
                           and not s.get("disposition", {}).get("attached_pic")), None)
        self.audio = next((s for s in self.streams if s.get("codec_type") == "audio"), None)

    @property
    def width(self) -> int:
        return int(self.video.get("width", 0)) if self.video else 0

    @property
    def height(self) -> int:
        return int(self.video.get("height", 0)) if self.video else 0

    @property
    def vcodec(self) -> str | None:
        return self.video.get("codec_name") if self.video else None

    @property
    def acodec(self) -> str | None:
        return self.audio.get("codec_name") if self.audio else None


def probe(path) -> Probe:
    r = subprocess.run(["ffprobe", "-v", "error", "-print_format", "json", "-show_format",
                        "-show_streams", str(path)], capture_output=True, text=True)
    if r.returncode != 0:
        aio.fail(f"Not a readable media file: {aio.original_name(path)} ({r.stderr.strip()[:200]})")
    return Probe(json.loads(r.stdout or "{}"))


def ffmpeg(args: list, duration: float = 0, lo: float = 0, hi: float = 100, quiet_fail=False) -> int:
    """Run ffmpeg, mapping its progress into lo..hi percent. Fails the job on error."""
    cmd = ["ffmpeg", "-hide_banner", "-nostdin", "-y", "-progress", "pipe:1", "-nostats", *map(str, args)]
    aio.log("$ " + " ".join(cmd))
    proc = subprocess.Popen(cmd, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, errors="replace")
    last = ""
    for line in proc.stdout:
        line = line.rstrip()
        k, _, v = line.partition("=")
        if k == "out_time_us" or k == "out_time_ms":  # both are microseconds in ffmpeg
            if duration > 0 and v.isdigit():
                aio.progress(lo + (hi - lo) * min(1.0, int(v) / 1e6 / duration))
            continue
        if re.fullmatch(r"[a-z_0-9]+=.*", line):  # other -progress keys
            continue
        if line:
            last = line
            aio.log(line)
    proc.wait()
    if proc.returncode != 0 and not quiet_fail:
        aio.fail(f"ffmpeg failed: {last}")
    return proc.returncode


def out_for(src, ext: str, suffix: str = "") -> Path:
    ext = ext.lstrip(".")
    return aio.unique(aio.out(f"{aio.stem(src)}{suffix}.{ext}"))


def parse_time(t: str | None) -> float | None:
    """'90', '1:30', '00:01:30.5' -> seconds."""
    t = (t or "").strip()
    if not t:
        return None
    try:
        parts = [float(x) for x in t.replace(",", ".").split(":")]
    except ValueError:
        aio.fail(f"Invalid time '{t}' (use seconds or hh:mm:ss)")
    secs = 0.0
    for p in parts:
        secs = secs * 60 + p
    return secs


AUDIO_EXT_FOR_CODEC = {"aac": "m4a", "alac": "m4a", "mp3": "mp3", "opus": "opus", "vorbis": "ogg",
                       "flac": "flac", "pcm_s16le": "wav", "pcm_s24le": "wav", "ac3": "ac3", "eac3": "eac3"}


def ensure_h264(path, force=False, lo=0, hi=100) -> str:
    """Re-encode to H.264 + AAC (yuv420p) unless it already is; keeps cover art and metadata."""
    info = probe(path)
    if not info.video:
        return "no video"
    v_ok = info.vcodec == "h264" and info.video.get("pix_fmt") in ("yuv420p", "yuvj420p")
    a_ok = info.acodec in (None, "aac", "mp3")
    if v_ok and a_ok and not force:
        return f"already H.264/{info.acodec or 'no audio'}, kept as is"
    covers = [s for s in info.streams if s.get("codec_type") == "video" and s.get("disposition", {}).get("attached_pic")]
    path = Path(path)
    tmp = path.with_name(path.stem + ".reencode.mp4")
    args = ["-i", path, "-map", f"0:{info.video['index']}", "-map", "0:a?"]
    if covers:
        args += ["-map", f"0:{covers[0]['index']}", "-c:v:1", "copy", "-disposition:v:1", "attached_pic"]
    reencode_v = force or not v_ok
    args += ["-c:v:0", "libx264", "-preset", "veryfast", "-crf", "20", "-pix_fmt", "yuv420p"] if reencode_v else ["-c:v:0", "copy"]
    args += ["-c:a", "aac", "-b:a", "192k"] if (force or not a_ok) else ["-c:a", "copy"]
    args += ["-map_metadata", "0", "-map_chapters", "0", "-movflags", "+faststart", tmp]
    before = f"{info.vcodec}/{info.acodec}"
    ffmpeg(args, info.duration, lo, hi)
    tmp.replace(path)
    return f"re-encoded {before} → h264/aac"
