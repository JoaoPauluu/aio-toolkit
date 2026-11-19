"""Two-pass x264 encode that targets a file size."""
import os

import aio
from aio_media import ffmpeg, out_for, probe

p = aio.params()
mb = float(p.get("custom_mb") or 20) if p.get("target") == "custom" else float(p.get("target") or 10)
target_bits = mb * 1000 * 1000 * 8 * 0.96          # leave ~4% for container overhead
files = p["files"]

for i, src in enumerate(files):
    name = aio.original_name(src)
    info = probe(src)
    if not info.video or not info.duration:
        aio.fail(f"{name}: no video stream or unknown duration")
    if info.size and info.size <= mb * 1000 * 1000:
        aio.log(f"{name} is already {aio.human(info.size)} (≤ {mb:g} MB); copying unchanged")
        dst = out_for(src, aio.Path(name).suffix.lstrip(".") or "mp4")
        dst.write_bytes(aio.Path(src).read_bytes())
        continue

    total_kbps = target_bits / info.duration / 1000
    a_kbps = (96 if total_kbps < 2000 else 128) if info.audio else 0
    if total_kbps < 400 and info.audio:
        a_kbps = 64
    v_kbps = int(total_kbps - a_kbps)
    if v_kbps < 50:
        aio.fail(f"{name}: {info.duration:.0f}s is too long to fit in {mb:g} MB "
                 f"(would need {v_kbps} kbps video). Trim it or choose a bigger target.")

    # pick a resolution the bitrate can sustain (~0.08 bits per pixel per frame at 30fps)
    h = info.height
    for cap, need in ((1080, 3500), (720, 1500), (480, 700), (360, 0)):
        if v_kbps >= need:
            h = min(h, cap) if h else cap
            break
    vf = ["-vf", f"scale=-2:{h}"] if h and h < info.height else []

    lo, hi = i / len(files) * 100, (i + 1) / len(files) * 100
    mid = (lo + hi) / 2
    aio.status(f"{name}: {v_kbps} kbps video + {a_kbps} kbps audio @ {h or info.height}p ({i + 1}/{len(files)})")
    passlog = str(aio.JOB_DIR / f"pass{i}")
    common = ["-i", src, *vf, "-c:v", "libx264", "-preset", p.get("speed", "medium"),
              "-b:v", f"{v_kbps}k", "-maxrate", f"{int(v_kbps * 1.5)}k", "-bufsize", f"{v_kbps * 2}k",
              "-pix_fmt", "yuv420p", "-passlogfile", passlog]
    ffmpeg([*common, "-pass", "1", "-an", "-f", "null", os.devnull], info.duration, lo, mid)
    dst = out_for(src, "mp4", f"_{mb:g}MB")
    audio = ["-c:a", "aac", "-b:a", f"{a_kbps}k"] if info.audio else ["-an"]
    ffmpeg([*common, "-pass", "2", *audio, "-movflags", "+faststart", dst], info.duration, mid, hi)
    size = dst.stat().st_size
    verdict = "✓" if size <= mb * 1000 * 1000 else "⚠ slightly over target"
    aio.log(f"{name} → {dst.name}: {aio.human(size)} {verdict}")

aio.status(f"Compressed {len(files)} file(s) to ≤ {mb:g} MB")
