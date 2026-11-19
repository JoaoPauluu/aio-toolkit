"""Cut a time range from a media file."""
import aio
from aio_media import ffmpeg, out_for, parse_time, probe

p = aio.params()
src = p["files"][0]
info = probe(src)
start = parse_time(p.get("start")) or 0.0
if p.get("end_mode") == "duration":
    dur = parse_time(p.get("duration"))
    if not dur:
        aio.fail("Enter a duration")
else:
    end = parse_time(p.get("end"))
    dur = (end - start) if end is not None else (info.duration - start)
if dur <= 0:
    aio.fail("The end must be after the start")
if info.duration and start >= info.duration:
    aio.fail(f"Start is past the end of the file ({info.duration:.1f}s)")

ext = aio.Path(aio.original_name(src)).suffix.lstrip(".") or "mkv"
dst = out_for(src, ext, f"_cut_{start:.0f}s-{start + dur:.0f}s")
aio.status(f"Cutting {dur:.1f}s from {start:.1f}s ({p.get('mode')})")

if p.get("mode") == "precise":
    args = ["-ss", f"{start:.3f}", "-i", src, "-t", f"{dur:.3f}", "-map", "0:v?", "-map", "0:a?", "-map_metadata", "0"]
    if info.video:
        args += ["-c:v", "libx264", "-crf", "18", "-preset", "fast", "-pix_fmt", "yuv420p"]
        if ext not in ("mp4", "mkv", "mov"):
            dst = dst.with_suffix(".mp4")
    if info.audio:
        args += ["-c:a", "aac" if info.video or ext in ("m4a", "mp4") else
                 {"mp3": "libmp3lame", "flac": "flac", "wav": "pcm_s16le", "opus": "libopus", "ogg": "libvorbis"}.get(ext, "aac"),
                 "-b:a", "192k"]
else:
    args = ["-ss", f"{start:.3f}", "-i", src, "-t", f"{dur:.3f}", "-map", "0", "-c", "copy",
            "-avoid_negative_ts", "make_zero", "-map_metadata", "0"]
if dst.suffix in (".mp4", ".mov", ".m4a"):
    args += ["-movflags", "+faststart"]
ffmpeg(args + [dst], dur)
aio.status(f"Created {dst.name} ({aio.human(dst.stat().st_size)})")
