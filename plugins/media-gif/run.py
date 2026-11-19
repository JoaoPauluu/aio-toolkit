"""Video clip → GIF (two-pass palette) or animated WebP."""
import aio
from aio_media import ffmpeg, out_for, parse_time, probe

p = aio.params()
src = p["files"][0]
info = probe(src)
if not info.video:
    aio.fail("This file has no video stream")
start = parse_time(p.get("start")) or 0.0
dur = float(p.get("duration") or 5)
if info.duration:
    dur = min(dur, max(0.1, info.duration - start))
fps = int(p.get("fps") or 12)
width = int(p.get("width") or 480)
loop = "0" if p.get("loop", True) else "-1"
base = f"fps={fps},scale={width}:-2:flags=lanczos"
cut = ["-ss", f"{start:.3f}", "-t", f"{dur:.3f}"]

if p.get("format") == "webp":
    dst = out_for(src, "webp")
    aio.status("Encoding animated WebP")
    ffmpeg([*cut, "-i", src, "-vf", base, "-an", "-c:v", "libwebp_anim", "-lossless", "0",
            "-q:v", "75", "-compression_level", "5", "-loop", loop if loop == "0" else "1", dst], dur)
else:
    dst = out_for(src, "gif")
    palette = aio.JOB_DIR / "palette.png"
    aio.status("Pass 1/2: building colour palette")
    ffmpeg([*cut, "-i", src, "-vf", f"{base},palettegen=stats_mode=diff", palette], dur, 0, 40)
    aio.status("Pass 2/2: rendering GIF")
    ffmpeg([*cut, "-i", src, "-i", palette, "-lavfi",
            f"{base}[x];[x][1:v]paletteuse=dither=bayer:bayer_scale=5:diff_mode=rectangle",
            "-loop", loop, dst], dur, 40, 100)
aio.status(f"Created {dst.name} ({aio.human(dst.stat().st_size)})")
