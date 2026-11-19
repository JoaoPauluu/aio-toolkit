"""Two-pass loudnorm: measure, then apply with the measured values."""
import json
import re
import subprocess

import aio
from aio_media import AUDIO_EXT_FOR_CODEC, ffmpeg, out_for, probe

p = aio.params()
I = float(p.get("target") or -16)
TP = float(p.get("tp") if p.get("tp") is not None else -1.5)
LRA = 11
ENC = {"m4a": "aac", "mp3": "libmp3lame", "opus": "libopus", "ogg": "libvorbis", "flac": "flac", "wav": "pcm_s16le"}
files = p["files"]

for i, src in enumerate(files):
    name = aio.original_name(src)
    info = probe(src)
    if not info.audio:
        aio.fail(f"{name} has no audio")
    lo, hi = i / len(files) * 100, (i + 1) / len(files) * 100
    aio.status(f"Measuring loudness of {name} ({i + 1}/{len(files)})")
    r = subprocess.run(["ffmpeg", "-hide_banner", "-nostdin", "-i", src, "-map", "0:a:0", "-af",
                        f"loudnorm=I={I}:TP={TP}:LRA={LRA}:print_format=json", "-f", "null", "-"],
                       capture_output=True, text=True, errors="replace")
    m = re.search(r"\{[^{}]*\"input_i\"[^{}]*\}", r.stderr)
    if r.returncode != 0 or not m:
        aio.fail(f"Could not measure {name}: {r.stderr.strip().splitlines()[-1] if r.stderr.strip() else ''}")
    meas = json.loads(m.group(0))
    aio.log(f"{name}: measured {meas['input_i']} LUFS, peak {meas['input_tp']} dBTP, LRA {meas['input_lra']}")
    aio.progress(lo + (hi - lo) * 0.3)

    af = (f"loudnorm=I={I}:TP={TP}:LRA={LRA}:measured_I={meas['input_i']}:measured_TP={meas['input_tp']}:"
          f"measured_LRA={meas['input_lra']}:measured_thresh={meas['input_thresh']}:offset={meas['target_offset']}"
          f":linear=true:print_format=summary")
    orig_ext = aio.Path(name).suffix.lstrip(".").lower()
    sr = info.audio.get("sample_rate") or "48000"   # loudnorm upsamples to 192k internally
    if info.video:
        ext = orig_ext if orig_ext in ("mp4", "mkv", "mov", "webm") else "mkv"
        acodec = "libopus" if ext == "webm" else "aac"
        args = ["-i", src, "-map", "0:v?", "-map", "0:a:0", "-c:v", "copy", "-af", af, "-ar", sr,
                "-c:a", acodec, "-b:a", "192k", "-map_metadata", "0"]
    else:
        ext = orig_ext if orig_ext in ENC else AUDIO_EXT_FOR_CODEC.get(info.acodec, "m4a")
        if ext not in ENC:
            ext = "m4a"
        args = ["-i", src, "-map", "0:a:0", "-vn", "-af", af, "-ar", sr, "-c:a", ENC[ext], "-map_metadata", "0"]
        if ext in ("m4a", "mp3", "opus", "ogg"):
            args += ["-b:a", "192k"]
    aio.status(f"Normalizing {name} to {I:g} LUFS")
    ffmpeg(args + [out_for(src, ext, "_normalized")], info.duration, lo + (hi - lo) * 0.3, hi)

aio.status(f"Normalized {len(files)} file(s) to {I:g} LUFS")
